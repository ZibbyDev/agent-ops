package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZibbyDev/agent-ops/internal/driver"
	"github.com/ZibbyDev/agent-ops/internal/scheduler"
	"github.com/ZibbyDev/agent-ops/internal/state"
	"github.com/ZibbyDev/agent-ops/internal/task"
	"github.com/ZibbyDev/agent-ops/internal/tool"
)

type fakeDriver struct{}

func (fakeDriver) Name() string { return "fake" }
func (fakeDriver) Run(_ context.Context, req driver.Request) (driver.Result, error) {
	return driver.Result{FinalMessage: "ran: " + req.UserPrompt}, nil
}

func setup(t *testing.T) (*httptest.Server, *state.Store, *scheduler.Scheduler) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	tools := tool.NewRegistry()
	_ = tools.Register(tool.NewShellTool())

	runner := task.NewRunner(fakeDriver{}, tools, st)
	sched := scheduler.New(runner, st, slog.Default())
	sched.Start()
	t.Cleanup(func() { _ = sched.Stop(context.Background()) })

	srv, err := New(Config{
		Scheduler: sched,
		Store:     st,
		Tools:     tools,
		Token:     "test-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	return httpSrv, st, sched
}

func rpcCall(t *testing.T, base, method string, params any) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	req, _ := http.NewRequest("POST", base+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rpc %s: status %d", method, resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	return out
}

func decodeResult(t *testing.T, raw []byte, into any) {
	t.Helper()
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v; raw=%s", err, raw)
	}
	if len(resp.Error) > 0 {
		t.Fatalf("rpc error: %s", resp.Error)
	}
	if err := json.Unmarshal(resp.Result, into); err != nil {
		t.Fatalf("decode result: %v; result=%s", err, resp.Result)
	}
}

func TestInitialize(t *testing.T) {
	srv, _, _ := setup(t)
	var got struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "initialize", map[string]any{}), &got)
	if got.ProtocolVersion == "" {
		t.Fatal("no protocolVersion")
	}
	if got.ServerInfo.Name != "agent-ops" {
		t.Fatalf("server name = %q", got.ServerInfo.Name)
	}
	if got.Capabilities.Tools == nil {
		t.Fatal("no tools capability advertised")
	}
}

func TestToolsList_ContainsAllBuiltinsAndHostTools(t *testing.T) {
	srv, _, _ := setup(t)
	var got struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/list", map[string]any{}), &got)

	names := map[string]struct{}{}
	for _, t := range got.Tools {
		names[t.Name] = struct{}{}
	}
	for _, want := range []string{
		"agent_status", "agent_run_now", "agent_history", "agent_logs",
		"agent_list_tasks", "agent_get_task", "agent_set_task",
		"agent_integrate_add", "agent_integrate_remove", "agent_integrate_list",
		"host_shell",
	} {
		if _, ok := names[want]; !ok {
			t.Errorf("tool %q missing from tools/list (got %v)", want, names)
		}
	}
}

func TestAuth_RejectsMissingToken(t *testing.T) {
	srv, _, _ := setup(t)
	resp, err := http.Post(srv.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unauthorized") {
		t.Fatalf("expected unauthorized, got %s", body)
	}
}

func TestAuth_RejectsWrongToken(t *testing.T) {
	srv, _, _ := setup(t)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Authorization", "Bearer not-the-real-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unauthorized") {
		t.Fatalf("expected unauthorized, got %s", body)
	}
}

func TestSetTask_RunNow_HistoryEndToEnd(t *testing.T) {
	srv, _, _ := setup(t)

	// 1. Create a task
	var setRes struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name": "agent_set_task",
		"arguments": map[string]any{
			"name":   "smoke",
			"cron":   "0 9 * * 1",
			"prompt": "say hello",
		},
	}), &setRes)
	if setRes.IsError {
		t.Fatalf("set_task error: %s", setRes.Content[0].Text)
	}

	// 2. Run it
	var runRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_run_now",
		"arguments": map[string]any{"task_name": "smoke"},
	}), &runRes)
	if runRes.IsError {
		t.Fatalf("run_now error: %s", runRes.Content[0].Text)
	}
	if !strings.Contains(runRes.Content[0].Text, "completed") {
		t.Fatalf("expected status=completed in run_now output, got %s", runRes.Content[0].Text)
	}

	// 3. History should now include it
	time.Sleep(50 * time.Millisecond) // give the runner a beat
	var histRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_history",
		"arguments": map[string]any{"task_name": "smoke", "limit": 10},
	}), &histRes)
	if !strings.Contains(histRes.Content[0].Text, "smoke") {
		t.Fatalf("history missing the smoke run: %s", histRes.Content[0].Text)
	}
}

func TestHostToolPassthrough(t *testing.T) {
	srv, _, _ := setup(t)
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "host_shell",
		"arguments": map[string]any{"command": "echo hello-from-host"},
	}), &res)
	if res.IsError {
		t.Fatalf("host_shell error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "hello-from-host") {
		t.Fatalf("expected echo output, got %s", res.Content[0].Text)
	}
}

func TestMissionFlow_SetGetRemember(t *testing.T) {
	srv, store, _ := setup(t)

	// 1. set a mission statement
	var setRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_set_mission",
		"arguments": map[string]any{"statement": "I steward the OpenDesign instance."},
	}), &setRes)
	if setRes.IsError {
		t.Fatalf("set_mission error: %s", setRes.Content[0].Text)
	}

	// 2. add a fact
	var rememberRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name": "agent_remember_fact",
		"arguments": map[string]any{
			"fact":   "Postgres 16 listening on :5432, password in /etc/myapp/db.conf",
			"source": "bootstrap",
		},
	}), &rememberRes)
	if rememberRes.IsError {
		t.Fatalf("remember_fact error: %s", rememberRes.Content[0].Text)
	}

	// 3. read it back
	var getRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_get_mission",
		"arguments": map[string]any{},
	}), &getRes)
	text := getRes.Content[0].Text
	if !strings.Contains(text, "I steward the OpenDesign instance.") {
		t.Fatalf("statement missing from get_mission output:\n%s", text)
	}
	if !strings.Contains(text, "Postgres 16") {
		t.Fatalf("fact missing from get_mission output:\n%s", text)
	}

	// 4. underlying store reflects the same — belt + suspenders so a future
	// MCP regression that silently fails to persist would be caught.
	m, err := store.GetMission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Statement == "" || len(m.Facts) == 0 {
		t.Fatalf("store didn't persist mission: %+v", m)
	}
}

func TestToolsList_IncludesMissionTools(t *testing.T) {
	srv, _, _ := setup(t)
	var got struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/list", map[string]any{}), &got)
	names := map[string]struct{}{}
	for _, t := range got.Tools {
		names[t.Name] = struct{}{}
	}
	for _, want := range []string{"agent_get_mission", "agent_set_mission", "agent_remember_fact"} {
		if _, ok := names[want]; !ok {
			t.Errorf("mission tool %q missing from tools/list", want)
		}
	}
}

// ─── fact_inspect ──────────────────────────────────────────────────────────

func TestFactInspect_ReturnsUnfilteredFact(t *testing.T) {
	srv, store, _ := setup(t)
	ctx := context.Background()

	// Add two facts; the second carries npm-warn noise that the prompt
	// renderer would strip. fact_inspect must return the FULL text.
	if _, err := store.AddFact(ctx, "auto", "first fact"); err != nil {
		t.Fatal(err)
	}
	noisy := "bootstrap exited 7\nnpm warn deprecated foo@1.0.6: unsupported\nnpm warn deprecated bar@2.0.0: see migration"
	if _, err := store.AddFact(ctx, "bootstrap", noisy); err != nil {
		t.Fatal(err)
	}

	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "fact_inspect",
		"arguments": map[string]any{"index": 0},
	}), &res)
	if res.IsError {
		t.Fatalf("fact_inspect error: %s", res.Content[0].Text)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "npm warn deprecated foo@1.0.6") {
		t.Fatalf("unfiltered text missing the dropped lines: %s", text)
	}
	if !strings.Contains(text, "npm warn deprecated bar@2.0.0") {
		t.Fatalf("unfiltered text missing the second dropped line: %s", text)
	}
	if !strings.Contains(text, "bootstrap exited 7") {
		t.Fatalf("unfiltered text missing kept line: %s", text)
	}
	if !strings.Contains(text, `"source"`) || !strings.Contains(text, "bootstrap") {
		t.Fatalf("response did not include source field: %s", text)
	}

	// index=1 should be the older "first fact".
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "fact_inspect",
		"arguments": map[string]any{"index": 1},
	}), &res)
	if res.IsError {
		t.Fatalf("fact_inspect index=1 error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "first fact") {
		t.Fatalf("index=1 should have returned the oldest fact, got: %s", res.Content[0].Text)
	}
}

func TestFactInspect_NegativeIndex_ReturnsError(t *testing.T) {
	srv, store, _ := setup(t)
	if _, err := store.AddFact(context.Background(), "auto", "hi"); err != nil {
		t.Fatal(err)
	}
	// Negative index must trigger a JSON-RPC error (code -32602), not a
	// tool-result with isError=true. We need to see the .error envelope so
	// peek at the raw response.
	raw := rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "fact_inspect",
		"arguments": map[string]any{"index": -1},
	})
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode raw: %v; raw=%s", err, raw)
	}
	if env.Error == nil {
		t.Fatalf("expected JSON-RPC error envelope, got: %s", raw)
	}
	if env.Error.Code != -32602 {
		t.Fatalf("error code = %d, want -32602; raw=%s", env.Error.Code, raw)
	}
}

func TestFactInspect_OutOfRange_ReturnsError(t *testing.T) {
	srv, store, _ := setup(t)
	if _, err := store.AddFact(context.Background(), "auto", "only fact"); err != nil {
		t.Fatal(err)
	}
	raw := rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "fact_inspect",
		"arguments": map[string]any{"index": 5},
	})
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode raw: %v; raw=%s", err, raw)
	}
	if env.Error == nil {
		t.Fatalf("expected JSON-RPC error envelope, got: %s", raw)
	}
	if env.Error.Code != -32602 {
		t.Fatalf("error code = %d, want -32602; raw=%s", env.Error.Code, raw)
	}
	if !strings.Contains(env.Error.Message, "no fact at index 5") {
		t.Fatalf("error message = %q, want 'no fact at index 5'", env.Error.Message)
	}
}

func TestToolsList_IncludesFactInspect(t *testing.T) {
	srv, _, _ := setup(t)
	var got struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/list", map[string]any{}), &got)
	for _, tt := range got.Tools {
		if tt.Name == "fact_inspect" {
			return
		}
	}
	t.Fatalf("fact_inspect missing from tools/list (got %v)", got.Tools)
}

func TestHealthEndpoint(t *testing.T) {
	srv, _, _ := setup(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}
}

// ─── H5: fail-closed on empty token ────────────────────────────────────────

func TestNew_RejectsEmptyToken(t *testing.T) {
	tools := tool.NewRegistry()
	_, err := New(Config{
		Tools: tools,
		Token: "",
	})
	if err == nil {
		t.Fatal("expected error from New with empty token, got nil")
	}
	if err != ErrEmptyToken {
		t.Fatalf("expected ErrEmptyToken, got %v", err)
	}
}

// ─── H4: Origin allowlist ──────────────────────────────────────────────────

// originPost issues a POST /mcp with an optional Origin header and the
// canonical test Bearer. Returns status + body so the caller can assert.
func originPost(t *testing.T, base, origin string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestOrigin_AllowsAllowlisted(t *testing.T) {
	srv, _, _ := setup(t)
	status, body := originPost(t, srv.URL, "https://zibby.dev")
	if status != http.StatusOK {
		t.Fatalf("Origin=https://zibby.dev: status %d, body=%s", status, body)
	}
}

func TestOrigin_AllowsMissing(t *testing.T) {
	srv, _, _ := setup(t)
	status, body := originPost(t, srv.URL, "")
	if status != http.StatusOK {
		t.Fatalf("missing Origin: status %d, body=%s", status, body)
	}
}

func TestOrigin_RejectsForeign(t *testing.T) {
	srv, _, _ := setup(t)
	status, body := originPost(t, srv.URL, "https://evil.example.com")
	if status != http.StatusForbidden {
		t.Fatalf("Origin=https://evil.example.com: expected 403, got %d body=%s", status, body)
	}
	if !strings.Contains(body, "origin not allowed") {
		t.Fatalf("expected origin error message, got %s", body)
	}
}

func TestOrigin_EnvOverride(t *testing.T) {
	// Setting AGENT_OPS_ALLOWED_ORIGINS replaces the defaults entirely —
	// operators get an explicit list, not an additive one.
	t.Setenv("AGENT_OPS_ALLOWED_ORIGINS", "https://ops.example.internal, https://other.example.internal")

	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	tools := tool.NewRegistry()
	_ = tools.Register(tool.NewShellTool())
	runner := task.NewRunner(fakeDriver{}, tools, st)
	sched := scheduler.New(runner, st, slog.Default())
	sched.Start()
	t.Cleanup(func() { _ = sched.Stop(context.Background()) })

	mcpSrv, err := New(Config{
		Scheduler: sched,
		Store:     st,
		Tools:     tools,
		Token:     "test-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(httpSrv.Close)

	// Custom origin allowed
	if status, body := originPost(t, httpSrv.URL, "https://ops.example.internal"); status != http.StatusOK {
		t.Fatalf("custom origin should be allowed: status=%d body=%s", status, body)
	}
	// Default origin no longer allowed (env replaces defaults)
	if status, _ := originPost(t, httpSrv.URL, "https://zibby.dev"); status != http.StatusForbidden {
		t.Fatalf("default origin should be rejected when env override is set: status=%d", status)
	}
}

// ─── Generic config-driven trailing-slash redirect ─────────────────────────

// trailingSlashSetup spins up: (1) a fake upstream "app" that echoes its
// request path with 200, and (2) an MCP Server whose catch-all reverse proxy
// points at that upstream (via AGENT_OPS_APP_PORT) with the given
// AGENT_OPS_TRAILING_SLASH_PATHS env. Returns a non-redirect-following client
// + the front URL so callers can observe the 308 directly.
func trailingSlashSetup(t *testing.T, slashPaths string) (*http.Client, string) {
	t.Helper()

	// Fake upstream app — replies 200 with the path it received so "proxied"
	// cases are distinguishable from a redirect.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("UPSTREAM:" + r.URL.Path))
	}))
	t.Cleanup(upstream.Close)
	// httptest URL is http://127.0.0.1:PORT — extract the port for the proxy.
	_, port, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatalf("split upstream host: %v", err)
	}
	t.Setenv("AGENT_OPS_APP_PORT", port)
	if slashPaths != "" {
		t.Setenv("AGENT_OPS_TRAILING_SLASH_PATHS", slashPaths)
	}

	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tools := tool.NewRegistry()
	_ = tools.Register(tool.NewShellTool())
	runner := task.NewRunner(fakeDriver{}, tools, st)
	sched := scheduler.New(runner, st, slog.Default())
	sched.Start()
	t.Cleanup(func() { _ = sched.Stop(context.Background()) })

	mcpSrv, err := New(Config{Scheduler: sched, Store: st, Tools: tools, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(front.Close)

	// Do NOT follow redirects — we want to assert on the 308 itself.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return client, front.URL
}

func TestTrailingSlashRedirect_BarePathRedirects(t *testing.T) {
	client, base := trailingSlashSetup(t, "/god-mode,/spaces")
	resp, err := client.Get(base + "/god-mode?foo=bar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("bare path: expected 308, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/god-mode/?foo=bar" {
		t.Fatalf("bare path: expected Location=/god-mode/?foo=bar, got %q", loc)
	}
}

func TestTrailingSlashRedirect_TrailingSlashNotRedirected(t *testing.T) {
	client, base := trailingSlashSetup(t, "/god-mode,/spaces")
	resp, err := client.Get(base + "/god-mode/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("already-slashed path should be proxied (200), got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "UPSTREAM:/god-mode/" {
		t.Fatalf("already-slashed path should reach upstream, got %q", string(body))
	}
}

func TestTrailingSlashRedirect_SubPathNotRedirected(t *testing.T) {
	client, base := trailingSlashSetup(t, "/god-mode,/spaces")
	resp, err := client.Get(base + "/god-mode/foo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sub-path should be proxied (200), got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "UPSTREAM:/god-mode/foo" {
		t.Fatalf("sub-path should reach upstream, got %q", string(body))
	}
}

func TestTrailingSlashRedirect_UnsetEnvNoRedirect(t *testing.T) {
	client, base := trailingSlashSetup(t, "") // env unset → feature off
	resp, err := client.Get(base + "/god-mode")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unset env: bare path should be proxied (200), got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "UPSTREAM:/god-mode" {
		t.Fatalf("unset env: bare path should reach upstream, got %q", string(body))
	}
}

// ─── OpenHands agent-server bridge (/_oh_agent/<PORT>/…) ────────────────────

// ohAgentFront spins up a bare MCP Server (no app port set — the oh-agent route
// is independent of AGENT_OPS_APP_PORT) and returns its front URL + a
// non-redirect-following client.
func ohAgentFront(t *testing.T) (*http.Client, string) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tools := tool.NewRegistry()
	_ = tools.Register(tool.NewShellTool())
	runner := task.NewRunner(fakeDriver{}, tools, st)
	sched := scheduler.New(runner, st, slog.Default())
	sched.Start()
	t.Cleanup(func() { _ = sched.Stop(context.Background()) })

	mcpSrv, err := New(Config{Scheduler: sched, Store: st, Tools: tools, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(front.Close)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return client, front.URL
}

// TestOhAgentProxy_StripsPrefixAndProxies binds a fake agent server on a port in
// the allowlist range and asserts /_oh_agent/<PORT>/<rest> reaches it with the
// /_oh_agent/<PORT> prefix stripped and the query string preserved.
func TestOhAgentProxy_StripsPrefixAndProxies(t *testing.T) {
	// Grab a free port inside [8000,8099]; skip if the whole range is taken.
	var ln net.Listener
	var port int
	for p := ohAgentPortLo; p <= ohAgentPortHi; p++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			ln, port = l, p
			break
		}
	}
	if ln == nil {
		t.Skip("no free port in oh-agent allowlist range")
	}
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "OHAGENT:"+r.URL.Path+"?"+r.URL.RawQuery)
	})}
	go func() { _ = upstream.Serve(ln) }()
	t.Cleanup(func() { _ = upstream.Close() })

	client, base := ohAgentFront(t)
	resp, err := client.Get(base + "/_oh_agent/" + strconv.Itoa(port) + "/socket/io?foo=bar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OHAGENT:/socket/io?foo=bar" {
		t.Fatalf("prefix-strip/query: got %q, want OHAGENT:/socket/io?foo=bar", string(body))
	}
}

// TestOhAgentProxy_RejectsPortOutOfRange asserts ports outside [8000,8099] (and
// non-numeric segments) are rejected with 400 — the SSRF guard — before any
// dial happens.
func TestOhAgentProxy_RejectsPortOutOfRange(t *testing.T) {
	client, base := ohAgentFront(t)
	for _, bad := range []string{"7999", "8100", "22", "0", "notaport"} {
		resp, err := client.Get(base + "/_oh_agent/" + bad + "/x")
		if err != nil {
			t.Fatalf("port %s: %v", bad, err)
		}
		got := resp.StatusCode
		_ = resp.Body.Close()
		if got != http.StatusBadRequest {
			t.Fatalf("port %s: expected 400, got %d", bad, got)
		}
	}
}

// TestOhAgentProxy_DeadPort502 asserts an in-range but not-listening port
// surfaces a 502 (proxy ErrorHandler) rather than crashing the daemon.
func TestOhAgentProxy_DeadPort502(t *testing.T) {
	// Find an in-range port that's NOT listening.
	port := 0
	for p := ohAgentPortHi; p >= ohAgentPortLo; p-- {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			_ = l.Close() // closed → nothing listening there now
			port = p
			break
		}
	}
	if port == 0 {
		t.Skip("no bindable port in oh-agent allowlist range")
	}
	client, base := ohAgentFront(t)
	resp, err := client.Get(base + "/_oh_agent/" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("dead port: expected 502, got %d", resp.StatusCode)
	}
}

// ─── openvscode-server sub-path bridge (/_ohvs/<PORT>/…) ────────────────────

// TestOhvsProxy_PreservesPrefix binds a fake server on a port in the allowlist
// range and asserts /_ohvs/<PORT>/<rest> reaches it with the FULL prefixed path
// intact — i.e. /_ohvs/<PORT>/foo is forwarded UNCHANGED (NOT stripped), which
// is the whole point: openvscode-server runs with --server-base-path
// /_ohvs/<PORT>. The query string is preserved too. The ohAgentFront helper
// gives an MCP front whose mux includes the /_ohvs/ route.
func TestOhvsProxy_PreservesPrefix(t *testing.T) {
	// Grab a free port inside [8000,8099]; skip if the whole range is taken.
	var ln net.Listener
	var port int
	for p := ohAgentPortLo; p <= ohAgentPortHi; p++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			ln, port = l, p
			break
		}
	}
	if ln == nil {
		t.Skip("no free port in ohvs allowlist range")
	}
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "OHVS:"+r.URL.Path+"?"+r.URL.RawQuery)
	})}
	go func() { _ = upstream.Serve(ln) }()
	t.Cleanup(func() { _ = upstream.Close() })

	client, base := ohAgentFront(t)
	full := "/_ohvs/" + strconv.Itoa(port) + "/foo"
	resp, err := client.Get(base + full + "?baz=qux")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	want := "OHVS:" + full + "?baz=qux"
	if string(body) != want {
		t.Fatalf("prefix must be PRESERVED (not stripped): got %q, want %q", string(body), want)
	}
}

// TestOhvsProxy_RejectsPortOutOfRange asserts ports outside [8000,8099] (and
// non-numeric segments) are rejected with 400 — the same SSRF guard as the
// agent bridge — before any dial happens.
func TestOhvsProxy_RejectsPortOutOfRange(t *testing.T) {
	client, base := ohAgentFront(t)
	for _, bad := range []string{"7999", "8100", "22", "0", "notaport"} {
		resp, err := client.Get(base + "/_ohvs/" + bad + "/x")
		if err != nil {
			t.Fatalf("port %s: %v", bad, err)
		}
		got := resp.StatusCode
		_ = resp.Body.Close()
		if got != http.StatusBadRequest {
			t.Fatalf("port %s: expected 400, got %d", bad, got)
		}
	}
}

// TestOhvsProxy_DeadPort502 asserts an in-range but not-listening port surfaces
// a 502 (proxy ErrorHandler) rather than crashing the daemon.
func TestOhvsProxy_DeadPort502(t *testing.T) {
	// Find an in-range port that's NOT listening.
	port := 0
	for p := ohAgentPortHi; p >= ohAgentPortLo; p-- {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			_ = l.Close() // closed → nothing listening there now
			port = p
			break
		}
	}
	if port == 0 {
		t.Skip("no bindable port in ohvs allowlist range")
	}
	client, base := ohAgentFront(t)
	resp, err := client.Get(base + "/_ohvs/" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("dead port: expected 502, got %d", resp.StatusCode)
	}
}

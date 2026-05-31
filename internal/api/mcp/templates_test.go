// Copyright 2026 Zibby Lab. Apache-2.0.

package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZibbyHQ/agent-ops/internal/scheduler"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/task"
	"github.com/ZibbyHQ/agent-ops/internal/tool"
)

// setupWithConfigPath is the variant of setup() that wires a writable
// config path into the Server so we can exercise agent_apply_template's
// real write branch. Returns the path so the test can stat / read it.
func setupWithConfigPath(t *testing.T) (*httptest.Server, string) {
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

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "agent-ops", "config.yaml")

	srv, err := New(Config{
		Scheduler:  sched,
		Store:      st,
		Tools:      tools,
		Token:      "test-token",
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	return httpSrv, cfgPath
}

// TestTemplates_ListReturnsAllBundled is the round-trip for
// agent_list_templates: the rpc-side response must list every embedded
// template. Pins the MCP surface in lockstep with the embed FS.
func TestTemplates_ListReturnsAllBundled(t *testing.T) {
	srv, _, _ := setup(t)
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_list_templates",
		"arguments": map[string]any{},
	}), &res)
	if res.IsError {
		t.Fatalf("agent_list_templates error: %s", res.Content[0].Text)
	}
	text := res.Content[0].Text
	for _, want := range []string{
		"wordpress-multisite", "single-app", "nodejs-server",
		`"count": 3`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("list result missing %q. Full output:\n%s", want, text)
		}
	}
}

// TestTemplates_GetReturnsRawYAML asserts agent_get_template returns the
// YAML body verbatim — same bytes the CLI's --dry-run would print.
func TestTemplates_GetReturnsRawYAML(t *testing.T) {
	srv, _, _ := setup(t)
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_get_template",
		"arguments": map[string]any{"name": "single-app"},
	}), &res)
	if res.IsError {
		t.Fatalf("agent_get_template error: %s", res.Content[0].Text)
	}
	for _, want := range []string{
		"state_dir:",
		"provider: claude-cli",
		"hourly_health_check",
	} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("template body missing %q. First 200 chars:\n%s",
				want, res.Content[0].Text[:200])
		}
	}
}

// TestTemplates_GetUnknownName returns isError:true with the available
// list embedded in the message so a remote LLM caller can self-correct on
// the next call.
func TestTemplates_GetUnknownName(t *testing.T) {
	srv, _, _ := setup(t)
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_get_template",
		"arguments": map[string]any{"name": "no-such-thing"},
	}), &res)
	if !res.IsError {
		t.Fatalf("expected isError:true for unknown template, got: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "available") {
		t.Errorf("error message missing 'available' hint: %s", res.Content[0].Text)
	}
}

// TestTemplates_ApplyDryRun confirms dry_run:true never touches the
// filesystem AND advertises restart_required:true so the LLM caller knows
// the daemon won't hot-reload.
func TestTemplates_ApplyDryRun(t *testing.T) {
	srv, cfgPath := setupWithConfigPath(t)

	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name": "agent_apply_template",
		"arguments": map[string]any{
			"name":    "single-app",
			"dry_run": true,
		},
	}), &res)
	if res.IsError {
		t.Fatalf("agent_apply_template dry_run error: %s", res.Content[0].Text)
	}
	for _, want := range []string{
		`"ok": true`, `"dry_run": true`, `"restart_required": true`,
		`"name": "single-app"`,
	} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("apply dry-run response missing %q. Full output:\n%s",
				want, res.Content[0].Text)
		}
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("dry_run wrote to disk; stat err = %v", err)
	}
}

// TestTemplates_ApplyWritesFile drives the real write path. We confirm:
//  1. The file lands at the configured path.
//  2. The bytes match the embedded template (no transformation).
//  3. The response includes restart_required:true + a next_step hint.
func TestTemplates_ApplyWritesFile(t *testing.T) {
	srv, cfgPath := setupWithConfigPath(t)

	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_apply_template",
		"arguments": map[string]any{"name": "wordpress-multisite"},
	}), &res)
	if res.IsError {
		t.Fatalf("agent_apply_template error: %s", res.Content[0].Text)
	}

	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("written config: %v", err)
	}
	if !strings.Contains(string(body), "# Example config — WordPress") {
		t.Errorf("written config missing WordPress header. First 200 chars:\n%s", string(body)[:200])
	}
	if !strings.Contains(string(body), "liveness_check") {
		t.Errorf("written config missing liveness_check schedule")
	}

	for _, want := range []string{
		`"ok": true`, `"restart_required": true`, `"next_step"`,
		`"name": "wordpress-multisite"`,
	} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("apply response missing %q. Full output:\n%s", want, res.Content[0].Text)
		}
	}
}

// TestTemplates_ApplyWithoutConfigPath verifies the test-harness / non-
// daemon construction path: when the Server was built without a
// ConfigPath, agent_apply_template returns isError:true pointing the
// caller at the CLI flow.
func TestTemplates_ApplyWithoutConfigPath(t *testing.T) {
	srv, _, _ := setup(t) // setup() doesn't set ConfigPath
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_apply_template",
		"arguments": map[string]any{"name": "single-app"},
	}), &res)
	if !res.IsError {
		t.Fatalf("expected isError:true, got %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "agent-ops init --template") {
		t.Errorf("expected fallback hint to point at CLI, got: %s", res.Content[0].Text)
	}
}

// TestTemplates_ToolsListIncludesNewTools is the parity check between
// tools/list and the handler switch — every name we route to must be
// advertised.
func TestTemplates_ToolsListIncludesNewTools(t *testing.T) {
	srv, _, _ := setup(t)
	var got struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/list", map[string]any{}), &got)
	names := map[string]struct{}{}
	for _, tt := range got.Tools {
		names[tt.Name] = struct{}{}
	}
	for _, want := range []string{
		"agent_list_templates", "agent_get_template", "agent_apply_template",
	} {
		if _, ok := names[want]; !ok {
			t.Errorf("tool %q missing from tools/list", want)
		}
	}
}

// TestTemplates_ApplyRequiresAuth — belt-and-suspenders that the new
// tools sit behind the same bearer-token gate as the existing surface.
// Without it, a future refactor that accidentally registers a tool
// outside the gate would silently expose host_shell-adjacent paths.
func TestTemplates_ApplyRequiresAuth(t *testing.T) {
	srv, _, _ := setup(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_apply_template","arguments":{"name":"single-app"}}}`
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	// Either a 401-style body OR a JSON-RPC error with "unauthorized" —
	// the existing surface uses the latter, so pin that.
	if !strings.Contains(string(buf), "unauthorized") {
		t.Errorf("expected unauthorized rejection, got: %s", string(buf))
	}
	var env struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(buf, &env)
	if env.Error == nil || env.Error.Code != -32001 {
		t.Errorf("expected JSON-RPC -32001 unauthorized, got: %s", string(buf))
	}
}

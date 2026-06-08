// Copyright 2026 Zibby Lab. Apache-2.0.

// Package mcp implements the Model Context Protocol server that the daemon
// exposes to remote agents (the user's local Claude Code / Cursor / Codex /
// Gemini CLI).
//
// Transport: Streamable HTTP per MCP 1.x spec.
//   POST /mcp  → JSON-RPC request, JSON-RPC response (single round-trip)
//   GET  /mcp  → SSE stream for server→client notifications (not used in v0.1
//                but the endpoint is reserved so we don't break clients that
//                speculatively open it)
//
// Auth: Bearer token from the AGENT_OPS_TOKEN env var (or generated file).
//
// This server is intentionally hand-rolled — Anthropic's TypeScript MCP SDK
// has no Go counterpart yet, and the JSON-RPC wire format is small enough
// that depending on a half-baked third-party port would cost more than it
// saves.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZibbyHQ/agent-ops/examples"
	"github.com/ZibbyHQ/agent-ops/internal/scheduler"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/tool"
)

// defaultAllowedOrigins is the baked-in CORS allowlist for browser callers.
// Non-browser clients (curl, Go HTTP, the Zibby control plane Lambda) send
// no Origin header at all and bypass this check entirely. Operators can
// extend this list at runtime via AGENT_OPS_ALLOWED_ORIGINS (comma-sep).
var defaultAllowedOrigins = []string{
	"https://zibby.dev",
	"https://zibby.app",
	"https://www.zibby.dev",
	"https://www.zibby.app",
}

// ErrEmptyToken is returned by New when the configured bearer token is
// empty. The daemon must fail-closed: an unauthenticated MCP endpoint
// exposes host_shell to anyone who can reach the port.
var ErrEmptyToken = errors.New("mcp: bearer token is empty (refusing to start with auth disabled)")

// Server is the MCP HTTP handler. Construct via New, mount on net/http.
type Server struct {
	scheduler *scheduler.Scheduler
	store     *state.Store
	tools     *tool.Registry
	token     string // bearer; New refuses to build a Server with an empty token
	log       *slog.Logger

	// configPath is the on-disk config.yaml the daemon was started with.
	// Empty when the Server was constructed without one (e.g. unit-tests
	// that don't exercise template-write paths) — in that case the
	// agent_apply_template tool returns an error pointing the caller at
	// agent-ops init --template instead.
	configPath string

	// allowedOrigins is the set of Origin header values acceptable on
	// cross-origin browser requests. Requests with no Origin header are
	// allowed through (non-browser clients). See validateOrigin.
	allowedOrigins map[string]struct{}

	serverName    string
	serverVersion string
}

// Config bundles construction params.
type Config struct {
	Scheduler *scheduler.Scheduler
	Store     *state.Store
	Tools     *tool.Registry
	Token     string
	Logger    *slog.Logger

	// ConfigPath is the daemon's on-disk YAML config (the same path passed
	// to `agent-opsd --config`). Optional — leave empty in tests that don't
	// exercise agent_apply_template; the daemon supplies it from cfgPath.
	ConfigPath string

	ServerName    string
	ServerVersion string
}

// New builds an MCP Server.
//
// Returns ErrEmptyToken if Config.Token is empty. agent-ops exposes
// host_shell (arbitrary command execution in the container) over MCP —
// running with auth disabled is never the right answer, so we fail
// at startup rather than logging a warning that nobody will see.
func New(c Config) (*Server, error) {
	if c.Token == "" {
		return nil, ErrEmptyToken
	}
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	name := c.ServerName
	if name == "" {
		name = "agent-ops"
	}
	ver := c.ServerVersion
	if ver == "" {
		ver = "0.1.0"
	}
	return &Server{
		scheduler:      c.Scheduler,
		store:          c.Store,
		tools:          c.Tools,
		token:          c.Token,
		log:            logger,
		configPath:     c.ConfigPath,
		allowedOrigins: loadAllowedOrigins(),
		serverName:     name,
		serverVersion:  ver,
	}, nil
}

// loadAllowedOrigins parses AGENT_OPS_ALLOWED_ORIGINS (comma-separated)
// and falls back to defaultAllowedOrigins when unset/empty. Whitespace
// around entries is trimmed; empty entries are dropped.
func loadAllowedOrigins() map[string]struct{} {
	out := map[string]struct{}{}
	raw := os.Getenv("AGENT_OPS_ALLOWED_ORIGINS")
	if strings.TrimSpace(raw) == "" {
		for _, o := range defaultAllowedOrigins {
			out[o] = struct{}{}
		}
		return out
	}
	for _, o := range strings.Split(raw, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			out[o] = struct{}{}
		}
	}
	return out
}

// Handler returns the http.Handler the daemon mounts on its single public
// port (AGENT_OPS_OPS_PORT, default 7842).
//
// SINGLE IN-TASK FRONT. The Zibby fleet ALB has a hard, non-adjustable cap of
// 100 target groups per ALB. The old design registered TWO target groups +
// TWO listener rules per app (one for the user app port, one for this daemon's
// ops/MCP port), which capped cloud apps at ~50 (100 TGs / 2). To break that
// ceiling at zero new standing cost we collapse to ONE TG + ONE rule per app
// by making this always-running daemon the sole front for the whole task:
//
//   /_zibby_ops/mcp      → the MCP JSON-RPC + SSE handler (the control plane
//                          proxies POST https://<subdomain>/_zibby_ops/mcp).
//   /_zibby_ops/healthz  → liveness for the ALB target-group health check.
//   /healthz             → kept as a back-compat alias (legacy ops TG used it).
//   everything else      → reverse-proxied to the user app on
//                          127.0.0.1:AGENT_OPS_APP_PORT.
//
// The reverse proxy supports WebSocket/SSE upgrades and long-lived streams
// (FlushInterval < 0 flushes immediately; the daemon's http.Server sets no
// WriteTimeout) so n8n, code-server, grafana, etc. work through the front.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health: serve under the ops prefix (the new single TG health-checks
	// /_zibby_ops/healthz) AND at the bare /healthz for back-compat with the
	// legacy ops target group, which health-checked /healthz directly.
	mux.HandleFunc(APIPrefix+"/healthz", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)

	// MCP under the prefix the control plane actually calls. ALB `forward`
	// actions do not strip path prefixes, so the daemon must serve the full
	// /_zibby_ops/mcp path itself.
	mux.HandleFunc(APIPrefix+"/mcp", s.handleMCP)
	// Keep the bare /mcp mount too so local/in-container callers and tests
	// that hit the daemon directly (no ALB prefix) keep working.
	mux.HandleFunc("/mcp", s.handleMCP)

	// Catch-all: reverse-proxy every non-ops path to the user app port.
	mux.Handle("/", s.appReverseProxy())
	return mux
}

// APIPrefix is the path prefix the Zibby fleet ALB forwards control-plane
// (ops) traffic under. Must stay in sync with APPS_DAEMON_OPS_PATH_PREFIX in
// backend/src/handlers/apps.js.
const APIPrefix = "/_zibby_ops"

// appReverseProxy builds the catch-all reverse proxy to the user app port
// (AGENT_OPS_APP_PORT on 127.0.0.1). When the app port is unknown/unset the
// handler returns 503 (the app hasn't registered a port yet) rather than
// panicking.
func (s *Server) appReverseProxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Front-upstream port: AGENT_OPS_APP_UPSTREAM_PORT overrides the
		// reverse-proxy target ONLY (not the app's bind port). The Zibby
		// control plane sets it to the Caddy auth-sidecar port (:8888) when an
		// app has Basic/Bearer/IP access control enabled, so the chain becomes
		// ALB → daemon:7842 → Caddy:8888 (auth gate) → app:AGENT_OPS_APP_PORT.
		// Unset (the common case) → fall back to the app's bind port directly.
		// We intentionally do NOT key off AGENT_OPS_APP_PORT for the upstream so
		// bootstrap's readiness poll / port-register / runScript bind keep using
		// the app's real port.
		raw := strings.TrimSpace(os.Getenv("AGENT_OPS_APP_UPSTREAM_PORT"))
		if raw == "" {
			raw = strings.TrimSpace(os.Getenv("AGENT_OPS_APP_PORT"))
		}
		if raw == "" {
			http.Error(w, "app port not yet known", http.StatusServiceUnavailable)
			return
		}
		target, err := url.Parse("http://127.0.0.1:" + raw)
		if err != nil {
			http.Error(w, "bad app port", http.StatusBadGateway)
			return
		}
		rp := httputil.NewSingleHostReverseProxy(target)
		// FlushInterval < 0 flushes writes to the client immediately — required
		// for SSE / streaming responses (code-server terminals, n8n executions)
		// to not buffer. WebSocket upgrades are proxied transparently by
		// httputil since Go 1.12 (Connection: Upgrade is honored).
		rp.FlushInterval = -1
		rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, e error) {
			// Mirror the pre-front behavior: a not-yet-listening app surfaced
			// as a 502 from the ALB. Keep that semantic here.
			s.log.Warn("app reverse-proxy upstream error", "err", e, "target", target.String())
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
		// Tighten the upstream dial so a wedged app fails fast instead of
		// hanging the front (and the ALB health check).
		rp.Transport = &http.Transport{
			ResponseHeaderTimeout: 0, // unbounded: long-lived streams allowed
			DialContext: (&net.Dialer{
				Timeout: 5 * time.Second,
			}).DialContext,
		}
		rp.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handlePOST(w, r)
	case http.MethodGet:
		s.handleGET(w, r)
	case http.MethodDelete:
		// MCP 1.x clients send DELETE to terminate a session; we don't keep
		// per-session state in v0.1 so respond 204 to satisfy the client.
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGET serves the SSE channel. v0.1 emits no server-initiated messages —
// we open the stream and keep it alive so MCP clients that probe this work.
func (s *Server) handleGET(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		http.Error(w, "forbidden: origin not allowed", http.StatusForbidden)
		return
	}
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// Initial probe so clients know the channel is alive.
	_, _ = fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()
	<-r.Context().Done()
}

func (s *Server) handlePOST(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		// Defence-in-depth: a present-but-non-allowlisted Origin means a
		// browser tab from somewhere unexpected. The bearer-token gate
		// already blocks drive-bys (browsers can't read AGENT_OPS_TOKEN)
		// but a future leak shouldn't be one hop away from host_shell.
		http.Error(w, "forbidden: origin not allowed", http.StatusForbidden)
		return
	}
	if !s.authOK(r) {
		writeJSONRPCError(w, nil, -32001, "unauthorized")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	defer r.Body.Close()

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		writeJSONRPCError(w, req.ID, -32600, "invalid jsonrpc version")
		return
	}

	switch req.Method {
	case "initialize":
		s.respond(w, req.ID, s.initializeResult())
	case "initialized", "notifications/initialized":
		// Notification — no response.
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		s.respond(w, req.ID, s.toolsList())
	case "tools/call":
		s.handleToolsCall(w, r.Context(), req)
	case "ping":
		s.respond(w, req.ID, map[string]any{})
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) authOK(r *http.Request) bool {
	// Belt-and-suspenders: New refuses to build a Server with an empty
	// token, but if a future caller bypasses the constructor we still
	// fail-closed rather than fail-open.
	if s.token == "" {
		s.log.Error("mcp: refusing request — token unset (this should be unreachable)")
		return false
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	return strings.TrimPrefix(h, "Bearer ") == s.token
}

// originOK enforces a CORS-style allowlist on requests that carry an
// Origin header. Non-browser callers (curl, Go clients, the Zibby control
// plane Lambda) don't set Origin and pass through. Browsers set it
// automatically and we reject anything outside the allowlist.
func (s *Server) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if _, ok := s.allowedOrigins[origin]; ok {
		return true
	}
	s.log.Warn("mcp: rejecting cross-origin request", "origin", origin)
	return false
}

func (s *Server) initializeResult() map[string]any {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    s.serverName,
			"version": s.serverVersion,
		},
	}
}

// ─── tools/list ─────────────────────────────────────────────────────────────

// toolsList enumerates the agent-ops control surface PLUS the daemon's
// underlying Tools (shell, http, …) so a remote agent can either trigger
// scheduled task runs or invoke tools directly.
func (s *Server) toolsList() map[string]any {
	out := []map[string]any{}

	for _, t := range builtinTools() {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": rawJSON(t.schema),
		})
	}

	// Expose each registered host tool, namespaced so we don't clash with
	// builtin agent_* tools. The local LLM driver sees these by their bare
	// names; remote callers see the prefixed name.
	for _, t := range s.tools.List() {
		out = append(out, map[string]any{
			"name":        "host_" + t.Name(),
			"description": t.Description(),
			"inputSchema": rawJSON(string(t.Schema())),
		})
	}

	return map[string]any{"tools": out}
}

// ─── tools/call ─────────────────────────────────────────────────────────────

func (s *Server) handleToolsCall(w http.ResponseWriter, ctx context.Context, req jsonRPCRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}

	// host_* → invoke a registered Tool directly.
	if strings.HasPrefix(params.Name, "host_") {
		bare := strings.TrimPrefix(params.Name, "host_")
		t, ok := s.tools.Get(bare)
		if !ok {
			writeJSONRPCError(w, req.ID, -32602, "no such host tool: "+bare)
			return
		}
		res, err := t.Invoke(ctx, params.Arguments)
		if err != nil {
			s.respond(w, req.ID, toolErrorResult(err.Error()))
			return
		}
		s.respond(w, req.ID, toolTextResult(res.Output))
		return
	}

	// builtin agent-ops tools
	switch params.Name {
	case "agent_status":
		s.respond(w, req.ID, s.toolStatus(ctx))
	case "agent_run_now":
		s.toolRunNow(w, ctx, req.ID, params.Arguments)
	case "agent_history":
		s.toolHistory(w, ctx, req.ID, params.Arguments)
	case "agent_logs":
		s.toolLogs(w, ctx, req.ID, params.Arguments)
	case "agent_list_tasks":
		s.toolListTasks(w, ctx, req.ID)
	case "agent_set_task":
		s.toolSetTask(w, ctx, req.ID, params.Arguments)
	case "agent_get_task":
		s.toolGetTask(w, ctx, req.ID, params.Arguments)
	case "agent_get_mission":
		s.toolGetMission(w, ctx, req.ID)
	case "agent_set_mission":
		s.toolSetMission(w, ctx, req.ID, params.Arguments)
	case "agent_remember_fact":
		s.toolRememberFact(w, ctx, req.ID, params.Arguments)
	case "fact_inspect":
		s.toolFactInspect(w, ctx, req.ID, params.Arguments)
	case "agent_list_templates":
		s.respond(w, req.ID, s.toolListTemplates())
	case "agent_get_template":
		s.toolGetTemplate(w, req.ID, params.Arguments)
	case "agent_apply_template":
		s.toolApplyTemplate(w, req.ID, params.Arguments)
	case "agent_integrate_add":
		s.toolIntegrateAdd(w, req.ID, params.Arguments)
	case "agent_integrate_remove":
		s.toolIntegrateRemove(w, req.ID, params.Arguments)
	case "agent_integrate_list":
		s.toolIntegrateList(w, req.ID)
	default:
		writeJSONRPCError(w, req.ID, -32602, "no such tool: "+params.Name)
	}
}

// ─── builtin tool implementations ──────────────────────────────────────────

func (s *Server) toolStatus(ctx context.Context) map[string]any {
	runs, _ := s.store.ListRuns(ctx, "", 1)
	resp := map[string]any{
		"server":       s.serverName,
		"version":      s.serverVersion,
		"task_count":   len(s.scheduler.Entries()),
		"tool_count":   len(s.tools.List()),
	}
	if len(runs) > 0 {
		r := runs[0]
		resp["last_run"] = map[string]any{
			"id":         r.ID,
			"task_name":  r.TaskName,
			"trigger":    r.Trigger,
			"status":     r.Status,
			"started_at": r.StartedAt,
			"ended_at":   r.EndedAt,
			"summary":    r.Summary,
		}
	}
	return toolTextResult(mustEncodeJSON(resp))
}

func (s *Server) toolRunNow(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		TaskName       string `json:"task_name"`
		OverridePrompt string `json:"override_prompt"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if args.TaskName == "" {
		s.respond(w, id, toolErrorResult("task_name is required"))
		return
	}
	run, err := s.scheduler.RunNow(ctx, args.TaskName, args.OverridePrompt)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(run)))
}

func (s *Server) toolHistory(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		TaskName string `json:"task_name"`
		Limit    int    `json:"limit"`
	}
	_ = json.Unmarshal(raw, &args)
	runs, err := s.store.ListRuns(ctx, args.TaskName, args.Limit)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"runs":  runs,
		"count": len(runs),
	})))
}

func (s *Server) toolLogs(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		RunID string `json:"run_id"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if args.RunID == "" {
		s.respond(w, id, toolErrorResult("run_id required"))
		return
	}
	logs, err := s.store.LogsForRun(ctx, args.RunID, args.Limit)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"run_id": args.RunID,
		"logs":   logs,
	})))
}

func (s *Server) toolListTasks(w http.ResponseWriter, ctx context.Context, id any) {
	tasks, err := s.store.ListTasks(ctx)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{"tasks": tasks})))
}

func (s *Server) toolGetTask(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.Name == "" {
		writeJSONRPCError(w, id, -32602, "name required")
		return
	}
	t, err := s.store.GetTask(ctx, args.Name)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(t)))
}

// ─── Mission journal ───────────────────────────────────────────────────────

func (s *Server) toolGetMission(w http.ResponseWriter, ctx context.Context, id any) {
	m, err := s.store.GetMission(ctx)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(m)))
}

func (s *Server) toolSetMission(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		Statement string `json:"statement"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	// Empty statement is intentionally allowed — used to clear a mission.
	if err := s.store.SetStatement(ctx, args.Statement); err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult("ok"))
}

// toolFactInspect returns the UNFILTERED text of one fact by recent-index
// (0 == most recent, increasing backward). The system prompt strips
// npm-warn-style noise from facts before rendering them; the agent calls
// this tool when a filter hint flags something worth a closer look.
//
// The raw text is what the agent sees — we do NOT pass it through
// filterFactForPrompt here. That's the whole point of the tool.
func (s *Server) toolFactInspect(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	// We decode index manually because the test of "missing vs wrong type vs
	// negative" matters for the spec'd error semantics: all three return
	// -32602, but we want each branch to message clearly so the agent (which
	// has to read the error) knows what to fix.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	idxRaw, ok := probe["index"]
	if !ok {
		writeJSONRPCError(w, id, -32602, "index is required")
		return
	}
	var index int
	if err := json.Unmarshal(idxRaw, &index); err != nil {
		writeJSONRPCError(w, id, -32602, "index must be a non-negative integer")
		return
	}
	if index < 0 {
		writeJSONRPCError(w, id, -32602, "index must be a non-negative integer")
		return
	}
	m, err := s.store.GetMission(ctx)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	// index 0 maps to the LAST element so it mirrors the prompt-render
	// numbering (0 == most recent).
	if index >= len(m.Facts) {
		writeJSONRPCError(w, id, -32602, fmt.Sprintf("no fact at index %d", index))
		return
	}
	f := m.Facts[len(m.Facts)-1-index]
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"source": f.Source,
		"ts":     f.TS.Format("2006-01-02T15:04:05Z07:00"),
		"fact":   f.Fact,
	})))
}

func (s *Server) toolRememberFact(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		Fact   string `json:"fact"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if args.Fact == "" {
		s.respond(w, id, toolErrorResult("fact is required"))
		return
	}
	if args.Source == "" {
		args.Source = "user" // MCP callers default to user-supplied
	}
	facts, err := s.store.AddFact(ctx, args.Source, args.Fact)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"added":       true,
		"total_facts": len(facts),
	})))
}

func (s *Server) toolSetTask(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var t state.Task
	if err := json.Unmarshal(raw, &t); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if t.Name == "" || t.Prompt == "" {
		s.respond(w, id, toolErrorResult("name and prompt required"))
		return
	}
	// Defaults: enabled=true unless caller explicitly sent enabled=false.
	if !t.Enabled && raw != nil {
		// Peek at the raw json to distinguish "absent" from "false".
		if !strings.Contains(string(raw), `"enabled"`) {
			t.Enabled = true
		}
	}
	if err := s.scheduler.SetTask(ctx, t); err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult("ok"))
}

// ─── Bundled config templates ──────────────────────────────────────────────

// toolListTemplates returns the bundled template metadata as a structured
// JSON payload (mirrors the CLI's --list-templates table). The MCP wrapper
// stuffs it into a text-content block so any MCP client can render it.
func (s *Server) toolListTemplates() map[string]any {
	all := examples.List()
	rows := make([]map[string]any, 0, len(all))
	for _, t := range all {
		rows = append(rows, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"filename":    t.Filename,
		})
	}
	return toolTextResult(mustEncodeJSON(map[string]any{
		"templates": rows,
		"count":     len(rows),
	}))
}

// toolGetTemplate returns the raw YAML body of one bundled template. The
// caller is expected to display it to the operator and (after review)
// invoke agent_apply_template — we explicitly don't write here so a misuse
// of "get" doesn't clobber the daemon's config.
func (s *Server) toolGetTemplate(w http.ResponseWriter, id any, raw json.RawMessage) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if args.Name == "" {
		s.respond(w, id, toolErrorResult("name is required"))
		return
	}
	body, err := examples.Get(args.Name)
	if err != nil {
		// examples.Get already lists the available names in its error.
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(string(body)))
}

// toolApplyTemplate writes a bundled template to the daemon's configured
// config.yaml path. Always returns restart_required:true — the daemon
// does NOT hot-reload its config (no SIGHUP handler in v0.2); the operator
// has to `agent-ops restart` to pick up the new file. dry_run:true
// short-circuits the write so a remote agent can preview before
// committing.
//
// Failure modes (all surfaced as isError:true tool results, not JSON-RPC
// errors, so the LLM caller sees the human-readable message):
//   - unknown template name
//   - server constructed without ConfigPath (e.g. test harness)
//   - filesystem write fails (perm denied / disk full)
func (s *Server) toolApplyTemplate(w http.ResponseWriter, id any, raw json.RawMessage) {
	var args struct {
		Name   string `json:"name"`
		DryRun bool   `json:"dry_run"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if args.Name == "" {
		s.respond(w, id, toolErrorResult("name is required"))
		return
	}
	body, err := examples.Get(args.Name)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}

	if s.configPath == "" {
		s.respond(w, id, toolErrorResult(
			"daemon was constructed without a config path; "+
				"have the operator run `agent-ops init --template "+args.Name+"` instead"))
		return
	}

	res := map[string]any{
		"name":             args.Name,
		"path":             s.configPath,
		"restart_required": true,
		"bytes":            len(body),
	}

	if args.DryRun {
		res["ok"] = true
		res["dry_run"] = true
		s.respond(w, id, toolTextResult(mustEncodeJSON(res)))
		return
	}

	if err := os.MkdirAll(filepath.Dir(s.configPath), 0o755); err != nil {
		s.respond(w, id, toolErrorResult("mkdir config dir: "+err.Error()))
		return
	}
	if err := os.WriteFile(s.configPath, body, 0o644); err != nil {
		s.respond(w, id, toolErrorResult("write config: "+err.Error()))
		return
	}

	s.log.Info("mcp: applied bundled template",
		"name", args.Name, "path", s.configPath, "bytes", len(body))

	res["ok"] = true
	res["dry_run"] = false
	res["next_step"] = "run `agent-ops restart` (or your platform's equivalent) — config is not hot-reloaded"
	s.respond(w, id, toolTextResult(mustEncodeJSON(res)))
}

// ─── JSON-RPC plumbing ─────────────────────────────────────────────────────

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Result  any            `json:"result,omitempty"`
	Error   *jsonRPCError  `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) respond(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeJSONRPCError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: msg},
	})
}

// MCP "content" wrapper around a tool's result.
func toolTextResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	}
}

func toolErrorResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": true,
	}
}

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }

func mustEncodeJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// builtin returns the static schema entries for the daemon's MCP tools.
// Centralized so tools/list and tests both reference one source of truth.
type builtin struct {
	name        string
	description string
	schema      string
}

func builtinTools() []builtin {
	return []builtin{
		{
			name:        "agent_status",
			description: "Show the agent-ops daemon's status: scheduled task count, host-tool count, last run summary.",
			schema:      `{"type":"object","properties":{}}`,
		},
		{
			name:        "agent_run_now",
			description: "Trigger an immediate run of a scheduled task. Optionally override the task's prompt for this run only.",
			schema:      `{"type":"object","properties":{"task_name":{"type":"string"},"override_prompt":{"type":"string"}},"required":["task_name"]}`,
		},
		{
			name:        "agent_history",
			description: "List recent task runs across all tasks (or filtered to one by task_name).",
			schema:      `{"type":"object","properties":{"task_name":{"type":"string"},"limit":{"type":"integer","default":20}}}`,
		},
		{
			name:        "agent_logs",
			description: "Fetch the per-line log of one task run (returned by agent_run_now or agent_history).",
			schema:      `{"type":"object","properties":{"run_id":{"type":"string"},"limit":{"type":"integer","default":500}},"required":["run_id"]}`,
		},
		{
			name:        "agent_list_tasks",
			description: "List every persisted task (schedule + prompt + tools + enabled flag).",
			schema:      `{"type":"object","properties":{}}`,
		},
		{
			name:        "agent_get_task",
			description: "Return the full config of one task by name.",
			schema:      `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
		},
		{
			name:        "agent_set_task",
			description: "Create or update a scheduled task. Supply name, cron (e.g. '0 9 * * 1'), prompt, optional tools allowlist, optional enabled flag.",
			schema:      `{"type":"object","properties":{"name":{"type":"string"},"cron":{"type":"string"},"prompt":{"type":"string"},"tools":{"type":"array","items":{"type":"string"}},"enabled":{"type":"boolean"}},"required":["name","cron","prompt"]}`,
		},
		// ── Mission journal ───────────────────────────────────────────────
		{
			name:        "agent_get_mission",
			description: "Return the instance's mission journal: the natural-language charter set by the user, plus the list of facts the agent has learned over time. This is what the agent reads on every task run to know who it is and what it's been doing.",
			schema:      `{"type":"object","properties":{}}`,
		},
		{
			name:        "agent_set_mission",
			description: "Replace the instance's mission statement (natural-language charter). Example: 'I steward the OpenDesign instance. Upgrades require dry-run; alert me at >80%% disk; never touch /etc/secrets.' Pass empty string to clear.",
			schema:      `{"type":"object","properties":{"statement":{"type":"string"}},"required":["statement"]}`,
		},
		{
			name:        "agent_remember_fact",
			description: "Append one fact to the mission journal. Use for important context the agent should carry across runs (versions installed, ports in use, decisions made). source defaults to 'user'.",
			schema:      `{"type":"object","properties":{"fact":{"type":"string"},"source":{"type":"string"}},"required":["fact"]}`,
		},
		{
			name:        "fact_inspect",
			description: "Return the unfiltered text of a KNOWN FACT from the system prompt. The system prompt filters npm-warn noise from facts by default; if you need to see the dropped lines (e.g., to diagnose why a bootstrap exited 7 when the visible facts only show generic warns), call this with the fact's `<index>` from its rendered hint. Index 0 = most recent fact, increases backward.",
			schema:      `{"type":"object","properties":{"index":{"type":"integer","minimum":0}},"required":["index"]}`,
		},
		// ── Bundled config templates ──────────────────────────────────────
		// These three mirror the `agent-ops init --template …` CLI surface
		// over MCP so a remote agent (e.g. the user's Claude Code) can pick
		// a starting config without the operator typing YAML by hand. See
		// internal/examples for the embedded template set.
		{
			name:        "agent_list_templates",
			description: "List the config.yaml templates bundled into the agent-ops binary. Returns each template's name + one-line description. Use BEFORE agent_get_template / agent_apply_template so you know which template name to ask for. No arguments.",
			schema:      `{"type":"object","properties":{}}`,
		},
		{
			name:        "agent_get_template",
			description: "Return the raw YAML body of one bundled config template by name (use agent_list_templates to discover names). Read-only — does NOT modify the daemon's config. Pair this with agent_apply_template when the operator has reviewed the YAML and wants to install it.",
			schema:      `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
		},
		{
			name:        "agent_apply_template",
			description: "Overwrite the daemon's config.yaml with a bundled template. Always presents the operator with a restart_required:true in the response — the daemon does NOT hot-reload config in v0.2; the operator must `agent-ops restart` for changes to take effect. Set dry_run:true to preview without writing.",
			schema:      `{"type":"object","properties":{"name":{"type":"string"},"dry_run":{"type":"boolean"}},"required":["name"]}`,
		},
		// ── Outbound MCP-client integrations (v0.3) ──────────────────────
		// These wrap internal/integrate so a remote agent (e.g. the user's
		// Claude Code) can wire up a NEW outbound MCP server — generally
		// for notify / ticketing — without SSH'ing in to hand-edit
		// config.yaml + agent-ops.env. The on-disk write is atomic
		// (flock + temp+rename + 0600 env file).
		{
			name: "agent_integrate_add",
			description: "Add an outbound MCP-client integration. Atomically appends to config.yaml + writes the secret (token) into agent-ops.env. Tools the remote server advertises become available to the local LLM under the prefix `{name}_{remote_tool_name}` (e.g. integration `zibby` + remote tool `trigger_workflow` → local tool `zibby_trigger_workflow`). Response includes restart_required:true — daemon does NOT hot-reload; caller must `agent-ops restart` or call the matching control-plane API for changes to take effect. Args: name (unique), transport ('http' or 'stdio'), url (http only), command + args (stdio only), auth_env + token (http auth), extra_env (extra KEY=VAL persisted to env file), stdio_env (per-subprocess env, stdio only).",
			schema: `{"type":"object","properties":{"name":{"type":"string"},"transport":{"type":"string","enum":["http","stdio"]},"url":{"type":"string"},"command":{"type":"string"},"args":{"type":"array","items":{"type":"string"}},"auth_env":{"type":"string"},"token":{"type":"string"},"extra_env":{"type":"object","additionalProperties":{"type":"string"}},"stdio_env":{"type":"object","additionalProperties":{"type":"string"}},"env_file":{"type":"string"}},"required":["name","transport"]}`,
		},
		{
			name:        "agent_integrate_remove",
			description: "Remove an outbound MCP-client integration by name. Atomically drops the entry from config.yaml + the AuthEnv key from agent-ops.env. Returns restart_required:true. ExtraEnv keys added with `agent_integrate_add` are NOT auto-removed (they may be shared across integrations) — edit agent-ops.env manually if needed.",
			schema:      `{"type":"object","properties":{"name":{"type":"string"},"env_file":{"type":"string"}},"required":["name"]}`,
		},
		{
			name:        "agent_integrate_list",
			description: "List all configured outbound MCP-client integrations. Secrets are NOT included — only the AuthEnv name is returned so the operator can correlate to their env file.",
			schema:      `{"type":"object","properties":{}}`,
		},
	}
}


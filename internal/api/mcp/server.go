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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZibbyDev/agent-ops/internal/appauth"

	"github.com/ZibbyDev/agent-ops/examples"
	"github.com/ZibbyDev/agent-ops/internal/scheduler"
	"github.com/ZibbyDev/agent-ops/internal/state"
	"github.com/ZibbyDev/agent-ops/internal/tool"
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

	// Hot-swap the app access-control config (Basic / IP allowlist / token).
	// The control plane POSTs here when the user changes auth, so it applies
	// with no task restart. Bridge-token authed (handleSetAuth checks authOK).
	mux.HandleFunc(APIPrefix+"/auth", s.handleSetAuth)

	// MCP under the prefix the control plane actually calls. ALB `forward`
	// actions do not strip path prefixes, so the daemon must serve the full
	// /_zibby_ops/mcp path itself.
	mux.HandleFunc(APIPrefix+"/mcp", s.handleMCP)
	// Keep the bare /mcp mount too so local/in-container callers and tests
	// that hit the daemon directly (no ALB prefix) keep working.
	mux.HandleFunc("/mcp", s.handleMCP)

	// OpenHands agent-server bridge. OpenHands (the user app) spawns a fresh
	// "agent server" subprocess per conversation on a dynamic localhost port in
	// 8000..8099, and the browser must reach it through the public domain. This
	// route reverse-proxies /_oh_agent/<PORT>/<rest> → http://127.0.0.1:<PORT>/<rest>
	// (HTTP + WebSocket), stripping the /_oh_agent/<PORT> prefix. It is NOT gated
	// by appauth: the agent server enforces its own session-api-key auth, so
	// double-auth here would just break the browser. The PORT allowlist (see
	// ohAgentProxy) is the SSRF guard that stops this reaching arbitrary local
	// ports.
	mux.HandleFunc("/_oh_agent/", s.ohAgentProxy())

	// openvscode-server sub-path bridge — prefix preserved (NOT stripped)
	// because openvscode runs with --server-base-path /_ohvs/<port>. Unlike the
	// agent server (which serves at /), openvscode-server emits its redirects and
	// asset URLs UNDER that base path, so it expects the full /_ohvs/<PORT>/<rest>
	// to arrive intact; stripping the prefix would break it. Same
	// [ohAgentPortLo, ohAgentPortHi] SSRF allowlist and same NOT-appauth-gated
	// treatment as /_oh_agent/. Purely additive: no other app uses /_ohvs/.
	mux.HandleFunc(ohvsPrefix, s.ohvsProxy())

	// Catch-all: reverse-proxy every non-ops path to the user app port.
	mux.Handle("/", s.appReverseProxy())
	return mux
}

// APIPrefix is the path prefix the Zibby fleet ALB forwards control-plane
// (ops) traffic under. Must stay in sync with APPS_DAEMON_OPS_PATH_PREFIX in
// backend/src/handlers/apps.js.
const APIPrefix = "/_zibby_ops"

// ohAgentPrefix and the [ohAgentPortLo, ohAgentPortHi] allowlist bound the
// OpenHands agent-server bridge (see ohAgentProxy). OpenHands spawns its
// per-conversation agent servers on dynamic localhost ports in this range;
// restricting the proxy to it is the SSRF guard that keeps a crafted URL from
// reaching arbitrary in-container ports (e.g. the daemon itself, or a secrets
// sidecar).
const (
	ohAgentPrefix = "/_oh_agent/"
	ohAgentPortLo = 8000
	ohAgentPortHi = 8099
)

// ohvsPrefix bounds the openvscode-server sub-path bridge (see ohvsProxy). It
// reuses the SAME [ohAgentPortLo, ohAgentPortHi] port allowlist as the agent
// bridge, but — unlike ohAgentProxy — it preserves the full /_ohvs/<PORT>/…
// path because openvscode-server is launched with --server-base-path
// /_ohvs/<PORT> and emits its redirects/assets under that base path.
const ohvsPrefix = "/_ohvs/"

// appEverHealthy flips true the first time the upstream app answers with a
// non-5xx response (i.e. the app port is genuinely up). Before that — during
// cold boot, when the app port isn't listening yet — a dial failure surfaces
// the friendly "starting up" interstitial instead of a raw 502. After the app
// has answered once we STOP masking, so a genuine later crash shows the real
// error rather than a misleading "starting up" page.
var appEverHealthy atomic.Bool

// appReady gates the "starting up" interstitial for apps that come up in
// stages — e.g. Plane serves its OWN "didn't start up correctly" page (HTTP
// 200) from its web tier while its API is still booting. A dial-failure check
// can't catch that (the port IS listening). So when the catalog declares a
// readiness path (AGENT_OPS_READY_PATH), a background poller hits it until it
// returns an acceptable status; until then EVERY app request gets the
// interstitial. Permanent once true.
var appReady atomic.Bool
var readyGateOnce sync.Once

// startReadinessGate launches (once) a background poller against the app's
// readiness path. Sets appReady when it passes. FAIL-OPEN: after a hard
// deadline it gives up and marks ready anyway, so a mis-configured path can
// never permanently mask a working app.
func (s *Server) startReadinessGate(appPort, readyPath string, lo, hi int) {
	readyGateOnce.Do(func() {
		go func() {
			u := "http://127.0.0.1:" + appPort + readyPath
			client := &http.Client{Timeout: 4 * time.Second}
			deadline := time.Now().Add(6 * time.Minute)
			for {
				resp, err := client.Get(u)
				if err == nil {
					code := resp.StatusCode
					_ = resp.Body.Close()
					if code >= lo && code <= hi {
						appReady.Store(true)
						s.log.Info("readiness gate passed", "path", readyPath, "status", code)
						return
					}
				}
				if time.Now().After(deadline) {
					appReady.Store(true) // fail-open
					s.log.Warn("readiness gate deadline — failing open", "path", readyPath)
					return
				}
				time.Sleep(3 * time.Second)
			}
		}()
	})
}

// startingUpHTML is the cold-boot interstitial served while the app port isn't
// listening yet. Self-contained (no external assets) so it renders on the
// app's own origin with zero dependencies. Matches the dashboard's not-found
// page look: dark canvas, radial glow, gradient glyph. Auto-refreshes so the
// real app takes over the moment it's up.
const startingUpHTML = `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="4">
<title>Starting up…</title>
<style>
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
  background:#0d0d10;color:#e7e7ea;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
  padding:32px 24px;position:relative;overflow:hidden}
body::before{content:'';position:fixed;inset:0;pointer-events:none;z-index:0;
  background:radial-gradient(circle at 50% 40%,rgba(117,83,255,.10) 0%,transparent 55%),
  radial-gradient(circle at 50% 70%,rgba(255,107,107,.06) 0%,transparent 55%)}
.card{position:relative;z-index:1;text-align:center;max-width:520px;width:100%}
.ring{width:84px;height:84px;margin:0 auto 28px;border-radius:50%;
  background:conic-gradient(from 0deg,#7553FF,#FF6B6B,#FFA94D,#7553FF);
  -webkit-mask:radial-gradient(farthest-side,transparent calc(100% - 9px),#000 calc(100% - 8px));
  mask:radial-gradient(farthest-side,transparent calc(100% - 9px),#000 calc(100% - 8px));
  animation:spin 1.1s linear infinite;filter:drop-shadow(0 0 36px rgba(117,83,255,.35))}
@keyframes spin{to{transform:rotate(360deg)}}
h1{font-size:24px;font-weight:600;margin:0 0 12px;letter-spacing:-.01em;
  background:linear-gradient(135deg,#7553FF 0%,#FF6B6B 50%,#FFA94D 100%);
  -webkit-background-clip:text;background-clip:text;color:transparent}
p{font-size:14px;line-height:1.6;color:#9b9ba5;margin:0}
.dots::after{content:'';animation:dots 1.4s steps(4,end) infinite}
@keyframes dots{0%{content:''}25%{content:'.'}50%{content:'..'}75%{content:'...'}}
</style></head>
<body><div class="card">
  <div class="ring" aria-hidden="true"></div>
  <h1>Starting up<span class="dots"></span></h1>
  <p>Your app is provisioning — this usually takes a minute or two.<br>This page refreshes automatically.</p>
</div></body></html>`

// appReverseProxy builds the catch-all reverse proxy to the user app port
// (AGENT_OPS_APP_PORT on 127.0.0.1). When the app port is unknown/unset the
// handler returns 503 (the app hasn't registered a port yet) rather than
// panicking.
func (s *Server) appReverseProxy() http.Handler {
	// Parse the configured trailing-slash redirect paths ONCE at handler
	// construction (Handler() is called once per Server). This is a GENERIC,
	// config-driven capability: the exact paths come from the per-app catalog
	// (backend splices catalog.trailingSlashRedirects → this env var). NO app
	// name is ever hardcoded here — agent-ops only knows "redirect these bare
	// paths to their trailing-slash form". Unset/empty → no redirects (the
	// default, behavior-preserving case).
	//
	// Why this exists: some apps serve a single-page app under a subpath whose
	// client-side router uses a basename WITH a trailing slash. Landing on the
	// bare path (no slash) loads the page but hangs client-side navigation
	// (basename mismatch). The app itself doesn't redirect, so we 308 here.
	trailingSlashPaths := parsePathSet(os.Getenv("AGENT_OPS_TRAILING_SLASH_PATHS"))

	// Optional catalog-driven readiness gate (see appReady). AGENT_OPS_READY_PATH
	// is a single path that only succeeds once the app is TRULY serving (e.g.
	// Plane's `/api/instances/` 502s while the API boots, 200s once it's up);
	// AGENT_OPS_READY_STATUS is the acceptable status or inclusive range
	// (e.g. "200" or "200-399", default "200-399"). Empty path → no gate (the
	// dial-failure interstitial still covers the port-not-listening window).
	readyPath := strings.TrimSpace(os.Getenv("AGENT_OPS_READY_PATH"))
	readyLo, readyHi := parseStatusRange(os.Getenv("AGENT_OPS_READY_STATUS"))
	readyAppPort := strings.TrimSpace(os.Getenv("AGENT_OPS_APP_PORT"))
	if readyPath != "" && readyAppPort != "" {
		s.startReadinessGate(readyAppPort, readyPath, readyLo, readyHi)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ACCESS CONTROL FIRST — before the readiness interstitial and the
		// trailing-slash redirect, so a gated app returns 401/403 UNIFORMLY for
		// every path and leaks nothing pre-auth (not even the "starting up" page
		// or a reflected redirect Location). Ops/health/mcp are separate mux
		// entries and never reach here, so the ALB health check stays exempt. A
		// nil config (the default) is passthrough. Enforced in the always-on
		// daemon so the control plane changes it with ZERO task restart (config
		// hot-swapped via the register-port boot response + POST /_zibby_ops/auth).
		if ac := appauth.Load(); ac != nil {
			if ok, status, wwwAuth := ac.Enforce(r); !ok {
				if wwwAuth != "" {
					w.Header().Set("WWW-Authenticate", wwwAuth)
				}
				w.WriteHeader(status)
				return
			}
		}

		// Readiness gate: while a readiness path is configured and the app
		// hasn't reported ready yet, serve the friendly auto-refreshing
		// "starting up" page for browser GETs INSTEAD of proxying (this is what
		// hides an app's own "not ready" page during staged boot). APIs / non-
		// HTML clients still get proxied so they see real statuses.
		if readyPath != "" && !appReady.Load() &&
			(r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Retry-After", "4")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, startingUpHTML)
			return
		}
		// Generic trailing-slash redirect (config-driven; see above). Fire
		// ONLY when the request path EXACTLY equals a configured path and does
		// not already end in "/". A sub-path (/foo/bar) or the already-slashed
		// form (/foo/) passes straight through to the proxy. 308 preserves the
		// request method + body for all verbs (only GET/HEAD realistically hit
		// this, but 308 is safe regardless).
		if len(trailingSlashPaths) > 0 {
			p := r.URL.Path
			if !strings.HasSuffix(p, "/") && trailingSlashPaths[p] {
				loc := p + "/"
				if r.URL.RawQuery != "" {
					loc += "?" + r.URL.RawQuery
				}
				w.Header().Set("Location", loc)
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			}
		}

		// Reverse-proxy straight to the app's bind port. The daemon now enforces
		// access control itself (appauth, above), so we ALWAYS go direct to the
		// app — no Caddy auth-sidecar hop. AGENT_OPS_APP_UPSTREAM_PORT (the old
		// caddy:8888 override) is intentionally ignored: on a legacy instance
		// that still has the Caddy sidecar + that env, going direct BYPASSES the
		// now-stale sidecar so its env-pinned (un-hot-reloadable) rules can't
		// fight the daemon's live config. The sidecar is dropped on next deploy.
		raw := strings.TrimSpace(os.Getenv("AGENT_OPS_APP_PORT"))
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
		// Mark the app "ever healthy" the first time it answers with anything
		// below 5xx — that proves the app port is up. Used by ErrorHandler to
		// decide between the cold-boot interstitial and a real error.
		rp.ModifyResponse = func(resp *http.Response) error {
			if resp.StatusCode < 500 {
				appEverHealthy.Store(true)
			}
			return nil
		}
		rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, e error) {
			s.log.Warn("app reverse-proxy upstream error", "err", e, "target", target.String())
			// COLD BOOT: the app port isn't listening yet (dial failure) and the
			// app has never answered → show the friendly "starting up" page that
			// auto-refreshes, instead of a raw 502. Skip the interstitial for
			// non-GET or explicit non-HTML clients (APIs/health probes want the
			// real status code, not an HTML page). Once the app has been healthy
			// once, always surface the real 502 (a genuine crash, not a boot).
			if !appEverHealthy.Load() &&
				(req.Method == http.MethodGet || req.Method == http.MethodHead) &&
				strings.Contains(req.Header.Get("Accept"), "text/html") {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Retry-After", "4")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, startingUpHTML)
				return
			}
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

// ohAgentProxy reverse-proxies /_oh_agent/<PORT>/<rest...> to
// http://127.0.0.1:<PORT>/<rest...> inside the container — the bridge that lets
// a remote browser reach the per-conversation OpenHands "agent server"
// subprocess (HTTP + WebSocket) through the public domain. The /_oh_agent/<PORT>
// prefix is stripped; the query string is preserved.
//
// PORT must be an integer in [ohAgentPortLo, ohAgentPortHi]; anything else is
// rejected with 400 (this is the SSRF guard — see the const block). The route
// is deliberately NOT behind appauth: the agent server enforces its own
// session-api-key auth, and gating here would double-auth and break the
// browser. If the target port isn't listening the proxy's ErrorHandler returns
// 502 rather than crashing.
func (s *Server) ohAgentProxy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Path is "/_oh_agent/<PORT>/<rest...>". Strip the prefix, then split
		// the first segment (the port) from the remainder (the proxied path).
		rest := strings.TrimPrefix(r.URL.Path, ohAgentPrefix)
		portStr, tail, _ := strings.Cut(rest, "/")

		port, err := strconv.Atoi(portStr)
		if err != nil || port < ohAgentPortLo || port > ohAgentPortHi {
			http.Error(w, "oh-agent: port out of allowed range", http.StatusBadRequest)
			return
		}

		target, err := url.Parse("http://127.0.0.1:" + portStr)
		if err != nil {
			http.Error(w, "oh-agent: bad target", http.StatusBadGateway)
			return
		}

		rp := httputil.NewSingleHostReverseProxy(target)
		// Custom Director: NewSingleHostReverseProxy would join the inbound path
		// onto target.Path; we instead REPLACE it with the stripped "/<rest...>"
		// so /_oh_agent/<PORT> is gone from the upstream request. Query string is
		// carried verbatim. Host/scheme point at the loopback target. The std
		// proxy already preserves Upgrade/Connection headers, so WebSocket
		// upgrades pass through untouched (no hop-by-hop stripping here).
		rp.Director = func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = "/" + tail
			// RawQuery is left as-is — it survives the rewrite above.
		}
		// FlushInterval < 0 flushes immediately so SSE/streaming responses from
		// the agent server aren't buffered, and so WebSocket frames flow live.
		rp.FlushInterval = -1
		rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, e error) {
			s.log.Warn("oh-agent reverse-proxy upstream error", "err", e, "target", target.String())
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
		// Fail fast on a dead port instead of hanging the front.
		rp.Transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 5 * time.Second,
			}).DialContext,
		}
		rp.ServeHTTP(w, r)
	}
}

// ohvsProxy reverse-proxies /_ohvs/<PORT>/<rest...> to http://127.0.0.1:<PORT>
// inside the container — the bridge that lets a remote browser reach an
// in-container openvscode-server (the web IDE) through the public domain.
//
// Crucially, and UNLIKE ohAgentProxy, the /_ohvs/<PORT> prefix is PRESERVED
// (NOT stripped): openvscode-server is launched with
// --server-base-path /_ohvs/<PORT>, so it expects the full prefixed path and
// emits its redirects/asset URLs under /_ohvs/<PORT>/…. Stripping the prefix
// would break every redirect and asset load. The query string is preserved
// too, and WebSocket upgrades pass through (the IDE's terminal/LSP channels).
//
// PORT must be an integer in [ohAgentPortLo, ohAgentPortHi] — the SAME
// allowlist as the agent bridge; anything else is rejected with 400 (the SSRF
// guard). Like /_oh_agent/ and /healthz this route is deliberately NOT behind
// appauth. If the target port isn't listening the proxy's ErrorHandler returns
// 502 rather than crashing.
func (s *Server) ohvsProxy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Path is "/_ohvs/<PORT>/<rest...>". Strip the prefix only to read the
		// first segment (the port) for validation — the upstream request keeps
		// the ORIGINAL r.URL.Path verbatim (see the Director below).
		rest := strings.TrimPrefix(r.URL.Path, ohvsPrefix)
		portStr, _, _ := strings.Cut(rest, "/")

		port, err := strconv.Atoi(portStr)
		if err != nil || port < ohAgentPortLo || port > ohAgentPortHi {
			http.Error(w, "ohvs: port out of allowed range", http.StatusBadRequest)
			return
		}

		target, err := url.Parse("http://127.0.0.1:" + portStr)
		if err != nil {
			http.Error(w, "ohvs: bad target", http.StatusBadGateway)
			return
		}

		rp := httputil.NewSingleHostReverseProxy(target)
		// Custom Director that does NOT touch the path. NewSingleHostReverseProxy
		// would join target.Path (empty) onto the inbound path, which is a no-op
		// here — but we set it explicitly to make the no-strip contract obvious:
		// the full /_ohvs/<PORT>/<rest...> is forwarded UNCHANGED, because
		// openvscode-server runs with --server-base-path /_ohvs/<PORT> and serves
		// from that base. Only scheme/Host are pointed at the loopback target;
		// RawQuery is left as-is. The std proxy preserves Upgrade/Connection
		// headers, so WebSocket upgrades pass through untouched.
		origDirector := rp.Director
		rp.Director = func(req *http.Request) {
			origDirector(req) // sets scheme/Host + preserves the path & query
			req.Host = target.Host
		}
		// FlushInterval < 0 flushes immediately so streaming responses aren't
		// buffered and WebSocket frames flow live.
		rp.FlushInterval = -1
		rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, e error) {
			s.log.Warn("ohvs reverse-proxy upstream error", "err", e, "target", target.String())
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
		// Fail fast on a dead port instead of hanging the front.
		rp.Transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 5 * time.Second,
			}).DialContext,
		}
		rp.ServeHTTP(w, r)
	}
}

// parseStatusRange parses a readiness status spec into an inclusive [lo,hi].
// "200" → (200,200); "200-399" → (200,399); empty/garbage → (200,399).
func parseStatusRange(s string) (int, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 200, 399
	}
	if i := strings.IndexByte(s, '-'); i > 0 {
		lo, e1 := strconv.Atoi(strings.TrimSpace(s[:i]))
		hi, e2 := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		if e1 == nil && e2 == nil && lo > 0 && hi >= lo {
			return lo, hi
		}
		return 200, 399
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n, n
	}
	return 200, 399
}

// parsePathSet turns a comma-separated list of absolute paths
// (e.g. "/foo,/bar") into a lookup set. Entries are trimmed; blank
// entries are skipped. Returns a nil/empty map for empty input so callers can
// cheaply skip the feature. GENERIC: the actual paths are supplied per-app via
// the catalog — this function knows nothing about any specific app.
func parsePathSet(csv string) map[string]bool {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	out := make(map[string]bool)
	for _, p := range strings.Split(csv, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out[p] = true
		}
	}
	return out
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleSetAuth hot-swaps the app access-control config. POST + bridge-token
// authed (same AGENT_OPS_TOKEN the MCP/ops endpoints require). The body is the
// appauth wire JSON ({basic,token,ipAllowList}); an empty/null body clears all
// gates. Returns 204 on success. NEVER restarts anything — the swap is atomic
// and applies to the very next proxied request.
func (s *Server) handleSetAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if err := appauth.Apply(body); err != nil {
		s.log.Warn("set-auth: bad config", "err", err.Error())
		http.Error(w, "bad config", http.StatusBadRequest)
		return
	}
	s.log.Info("set-auth: access-control config applied (hot, no restart)")
	w.WriteHeader(http.StatusNoContent)
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


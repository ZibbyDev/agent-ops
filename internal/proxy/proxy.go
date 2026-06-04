// Copyright 2026 Zibby Lab. Apache-2.0.

// Package proxy renders + manages a Caddy reverse-proxy in SHARED INGRESS
// mode: one host (with a single public IP) terminating TLS for MANY private
// backends, routed by Host header.
//
// This is the OSS-generic counterpart to internal/bootstrap.stepConfigureCaddy
// (which configures Caddy for a SINGLE app on its own VM). Where that writes
// `<domain> { reverse_proxy 127.0.0.1:<port> }`, this writes an
// on_demand_tls front door that asks an authorization endpoint before
// minting a cert, then proxies each Host to a backend private IP:port looked
// up from a control-plane-synced map.
//
// VENDOR-NEUTRAL: every input is an AGENT_OPS_PROXY_* env var (matching the
// AGENT_OPS_TOKEN / AGENT_OPS_ACME_CA convention). Nothing here is
// Zibby-specific — an operator can point it at any control plane that
// answers the `ask` contract (GET <askURL>?domain=<host> -> 2xx allow) and
// any source of a Host->upstream map.
//
// The map delivery (SHARED_INGRESS_PLAN.md "Control plane: Host -> backend
// mapping", delivery options) is pluggable. Phase 1 ships the simplest:
// poll a JSON map (from the control-plane HTTP endpoint, or — when running
// in AWS — a DynamoDB table) and rewrite a Caddy-readable map file, then
// `caddy reload`. The DDB reader is intentionally a TODO stub here so this
// package stays SDK-free in OSS; an operator wires their own source via
// SetMapSource.
package proxy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Config is the proxy's render+run input, populated from AGENT_OPS_PROXY_*.
type Config struct {
	// DomainBase: the proxy answers on *.<DomainBase> (e.g. apps.zibby.dev).
	DomainBase string
	// AskURL: on_demand_tls authorization endpoint. Caddy GETs
	// <AskURL>?domain=<host> on the first handshake; 2xx => mint cert.
	AskURL string
	// ControlPlaneBaseURL: where the route-map is polled from (Phase 2 may
	// also use it for a /routes endpoint). Optional for Phase 1.
	ControlPlaneBaseURL string
	// RoutesTable: DynamoDB table holding the Host->upstream map (AWS
	// delivery option). Optional; when set the operator's map source reads
	// it (SDK-free here — see SetMapSource).
	RoutesTable string
	// Region: AWS region for the DDB read (cosmetic otherwise).
	Region string

	// CaddyfilePath / MapPath: where we render config. Defaults match the
	// Debian caddy package layout.
	CaddyfilePath string
	MapPath       string
	// ACMECa: pin the ACME directory (defaults to Let's Encrypt prod), same
	// reasoning as the solo bootstrap (avoids latching a baked STAGING acct).
	ACMECa string
	// SyncInterval: how often the route-map is refreshed. Default 30s.
	SyncInterval time.Duration
}

// FromEnv builds a Config from AGENT_OPS_PROXY_* env vars with sane
// defaults. The CDK UserData (SharedIngressNestedStack) sets these.
func FromEnv() Config {
	c := Config{
		DomainBase:          os.Getenv("AGENT_OPS_PROXY_DOMAIN_BASE"),
		AskURL:              os.Getenv("AGENT_OPS_PROXY_ASK_URL"),
		ControlPlaneBaseURL: os.Getenv("AGENT_OPS_PROXY_CP_URL"),
		RoutesTable:         os.Getenv("AGENT_OPS_PROXY_ROUTES_TABLE"),
		Region:              os.Getenv("AGENT_OPS_PROXY_REGION"),
		CaddyfilePath:       "/etc/caddy/Caddyfile",
		MapPath:             "/etc/caddy/upstreams.map",
		ACMECa:              os.Getenv("AGENT_OPS_ACME_CA"),
		SyncInterval:        30 * time.Second,
	}
	if c.ACMECa == "" {
		c.ACMECa = "https://acme-v02.api.letsencrypt.org/directory"
	}
	return c
}

// RenderCaddyfile produces the proxy-mode Caddyfile. The key pieces, per
// SHARED_INGRESS_PLAN.md:
//   - global on_demand_tls { ask <AskURL> } so Caddy authorizes a host
//     before minting a cert (stops cert-farming / ACME DoS).
//   - a wildcard *.<DomainBase> site that proxies to the upstream resolved
//     from the Host header via Caddy's `map` directive backed by an
//     on-disk map file we sync from the control plane. Hosts with no map
//     entry get a 502 (paused/unknown app) — same UX as today's
//     released-IP / deleted-A-record.
//   - :80 -> :443 redirect (Caddy also serves ACME http-01 on :80).
//
// We use the `map` directive + `reverse_proxy {http.request.host.upstream}`
// indirection so a route change is a map-file rewrite + `caddy reload`, no
// full re-render. (The dynamic-upstreams module is the alternative the plan
// notes; the map-file approach keeps zero plugins in the base image.)
func RenderCaddyfile(c Config) string {
	if c.DomainBase == "" {
		c.DomainBase = "example.com"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# agent-ops shared-ingress proxy; managed — do not edit by hand.\n")
	b.WriteString("{\n")
	fmt.Fprintf(&b, "    acme_ca %s\n", c.ACMECa)
	b.WriteString("    on_demand_tls {\n")
	if c.AskURL != "" {
		fmt.Fprintf(&b, "        ask %s\n", c.AskURL)
	}
	// NOTE: the on_demand_tls `interval`/`burst` rate-cap options were REMOVED
	// in Caddy v2.10+ (parsing them is now a hard error). The `ask` endpoint is
	// the authorization gate (only registered active shared hosts get a cert),
	// so it remains the primary + sufficient defense against ACME abuse.
	b.WriteString("    }\n")
	b.WriteString("}\n\n")

	// :80 — ACME http-01 + redirect everything else to https.
	b.WriteString(":80 {\n")
	b.WriteString("    redir https://{host}{uri} permanent\n")
	b.WriteString("}\n\n")

	// Wildcard https site. on_demand mints the cert (gated by `ask`); the
	// upstream is resolved from the synced map file keyed by Host.
	fmt.Fprintf(&b, "*.%s {\n", c.DomainBase)
	b.WriteString("    tls {\n")
	b.WriteString("        on_demand\n")
	b.WriteString("    }\n")
	// map {host} -> {upstream} from the on-disk map file. A missing entry
	// leaves {upstream} empty; we 502 that case.
	fmt.Fprintf(&b, "    map {host} {upstream} {\n")
	fmt.Fprintf(&b, "        import %s\n", c.MapPath)
	b.WriteString("    }\n")
	b.WriteString("    @noupstream expression {upstream} == \"\"\n")
	b.WriteString("    respond @noupstream \"no backend for this host\" 502\n")
	b.WriteString("    reverse_proxy {upstream} {\n")
	b.WriteString("        header_up Host {host}\n")
	b.WriteString("    }\n")
	b.WriteString("    encode gzip zstd\n")
	b.WriteString("    log {\n")
	b.WriteString("        output stdout\n")
	b.WriteString("        format json\n")
	b.WriteString("    }\n")
	b.WriteString("}\n")
	return b.String()
}

// Route is one Host->upstream mapping. Upstream is "10.0.x.x:PORT".
type Route struct {
	Host     string
	Upstream string
}

// RenderMapFile renders the Caddy `map` body: one `"<host>" "<upstream>"`
// line per active route. Sorted for deterministic output (stable reloads,
// testable). This is what Caddy `import`s inside the map directive.
func RenderMapFile(routes []Route) string {
	sorted := make([]Route, len(routes))
	copy(sorted, routes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Host < sorted[j].Host })
	var b strings.Builder
	for _, r := range sorted {
		if r.Host == "" || r.Upstream == "" {
			continue
		}
		fmt.Fprintf(&b, "    %q %q\n", r.Host, r.Upstream)
	}
	return b.String()
}

// MapSource yields the current Host->upstream routes. Phase 1 leaves the
// concrete AWS/DDB implementation to the operator (keeps this package
// SDK-free); SetMapSource injects it. When nil, Sync writes an empty map
// (proxy 502s everything until a source is wired) — safe default.
type MapSource func(ctx context.Context) ([]Route, error)

var mapSource MapSource

// SetMapSource installs the route provider (e.g. a DDB scan of the
// control-plane routes table, or an HTTP GET against ControlPlaneBaseURL).
// PHASE 2 TODO: wire the AWS DDB reader here (scan zibby-<stage>-ingress-
// routes, project Host+upstream where status=active && mode=shared).
func SetMapSource(s MapSource) { mapSource = s }

// WriteConfig renders + writes the Caddyfile and an (initially empty) map
// file. Idempotent — safe to call on every boot.
func WriteConfig(c Config) error {
	if err := os.MkdirAll("/etc/caddy", 0o755); err != nil {
		return fmt.Errorf("mkdir /etc/caddy: %w", err)
	}
	if err := os.WriteFile(c.CaddyfilePath, []byte(RenderCaddyfile(c)), 0o644); err != nil {
		return fmt.Errorf("write Caddyfile: %w", err)
	}
	// Ensure the map file exists so the first `caddy validate` / start
	// doesn't fail on a missing import target. The rendered Caddyfile does
	// `import <MapPath>` inside the `map` directive; Caddy fails to LOAD when
	// that path is missing — which is exactly what strands the proxy after a
	// reboot (the map lives on the boot disk and is otherwise only written by
	// the sync loop, which on a fresh/rebooted box hasn't run yet). An EMPTY
	// map file is a valid `import` target (zero routes => every host 502s,
	// the intended no-routes UX) so Caddy loads cleanly. Use EnsureMapFile so
	// the create-if-absent semantics live in one place (also called by Setup).
	return EnsureMapFile(c)
}

// EnsureMapFile guarantees the upstreams map file exists (creating an EMPTY
// one if absent) so the Caddyfile's `import <MapPath>` never fails on a fresh
// or rebooted box. An empty file is a valid Caddy `import` target. Idempotent:
// an existing map (any content) is left untouched. Split out from WriteConfig
// so Setup can assert it independently and so it's unit-testable.
func EnsureMapFile(c Config) error {
	if c.MapPath == "" {
		c.MapPath = "/etc/caddy/upstreams.map"
	}
	if err := os.MkdirAll(filepath.Dir(c.MapPath), 0o755); err != nil {
		return fmt.Errorf("mkdir for map file: %w", err)
	}
	if _, err := os.Stat(c.MapPath); err != nil {
		if err := os.WriteFile(c.MapPath, []byte(""), 0o644); err != nil {
			return fmt.Errorf("write map file: %w", err)
		}
	}
	return nil
}

// Sync pulls the current routes from the map source, rewrites the map file
// if it changed, and reloads Caddy. A no-op when the content is unchanged
// (avoids needless reloads). Returns the number of routes written.
func Sync(ctx context.Context, c Config) (int, error) {
	var routes []Route
	if mapSource != nil {
		r, err := mapSource(ctx)
		if err != nil {
			return 0, fmt.Errorf("map source: %w", err)
		}
		routes = r
	}
	next := RenderMapFile(routes)
	cur, _ := os.ReadFile(c.MapPath)
	if string(cur) == next {
		return len(routes), nil // unchanged — skip reload
	}
	if err := os.WriteFile(c.MapPath, []byte(next), 0o644); err != nil {
		return 0, fmt.Errorf("write map file: %w", err)
	}
	if err := reloadCaddy(ctx, c.CaddyfilePath); err != nil {
		return len(routes), fmt.Errorf("caddy reload: %w", err)
	}
	return len(routes), nil
}

// reloadCaddy applies a config change without dropping connections.
// Best-effort over `caddy reload`; the binary is present on the solo AMI.
func reloadCaddy(ctx context.Context, caddyfile string) error {
	cmd := exec.CommandContext(ctx, "caddy", "reload", "--config", caddyfile, "--adapter", "caddyfile")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setupRunner runs a command and returns combined output + error. Swappable
// in tests (mirrors natCmdRunner) so Setup's systemctl/NAT calls can be
// asserted without touching the host. Production uses execRunner.
var setupRunner natRunner = execRunner

// Setup performs the SYNCHRONOUS boot/startup work the proxy needs BEFORE it
// can enter the route-sync loop, IN A FIXED ORDER, and never hangs:
//
//	(a) render + write the Caddyfile,
//	(b) ensure /etc/caddy/upstreams.map exists (empty if absent) so Caddy's
//	    `import` doesn't fail on a fresh/rebooted box,
//	(c) reload-or-restart caddy AND verify it became active (log loudly if not),
//	(d) if NAT is enabled, EnableNAT (ip_forward + MASQUERADE, idempotent).
//
// It is fail-LOUD-but-NON-HANGING: a step that fails is logged and the next
// step still runs (a box that can't, say, persist a sysctl can still serve
// inbound). This is the root-cause fix for "proxy up hangs without setting up
// Caddy/NAT": previously the *only* startup path was Up(), which (1) ran
// `systemctl enable --now caddy.service` — a no-op if caddy was already
// loaded with a stale/broken config (it neither reloads nor restarts), (2)
// ignored the result entirely (`_ =`) and never verified caddy came up, and
// (3) the SSM/route lookups upstream could block on a not-yet-ready network.
// Setup ALWAYS completes the Caddy + NAT work before any blocking sync.
//
// nat controls step (d); callers pass the resolved AGENT_OPS_PROXY_NAT
// decision (kept a param so internal/proxy stays env-free).
func Setup(ctx context.Context, c Config, nat bool, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// (a) Caddyfile + (b) map file. WriteConfig does both (and EnsureMapFile
	// is also called directly below as belt+braces in case a custom path was
	// supplied). A write failure here is loud but we still try to (re)start
	// caddy — it may already have a usable on-disk config.
	if err := WriteConfig(c); err != nil {
		logf("proxy: WriteConfig failed (continuing): %v", err)
	}
	if err := EnsureMapFile(c); err != nil {
		logf("proxy: ensure upstreams.map failed (continuing): %v", err)
	}

	// (c) reload-or-restart caddy and VERIFY it became active. `reload-or-
	// restart` reloads if running (zero-downtime), starts it if not — exactly
	// the behavior we want on both a fresh boot and a config change. We do NOT
	// use `enable --now` (the old bug: it won't re-read a changed config on an
	// already-loaded unit). enable is for boot-persistence and is handled by
	// the systemd unit in UserData, not here.
	if out, err := setupRunner(ctx, "systemctl", "reload-or-restart", "caddy.service"); err != nil {
		logf("proxy: systemctl reload-or-restart caddy failed (continuing): %v: %s",
			err, strings.TrimSpace(string(out)))
	} else {
		logf("proxy: caddy reload-or-restart ok")
	}
	// Verify active. is-active prints "active" and exits 0 when up; anything
	// else is a loud warning (NOT fatal — we still serve sync + NAT, and the
	// systemd Restart=always on our own unit plus a future Sync reload give
	// caddy more chances).
	if out, err := setupRunner(ctx, "systemctl", "is-active", "caddy.service"); err != nil ||
		strings.TrimSpace(string(out)) != "active" {
		logf("proxy: WARNING caddy is not active after reload-or-restart (state=%q err=%v); "+
			"the proxy will keep retrying via the sync loop",
			strings.TrimSpace(string(out)), err)
	} else {
		logf("proxy: caddy is active")
	}

	// (d) NAT — idempotent. EnableNAT is itself best-effort + non-hanging.
	if nat {
		logf("proxy: NAT enabled; configuring ip_forward + MASQUERADE")
		if err := EnableNAT(ctx, logf); err != nil {
			logf("proxy: NAT setup had an error (continuing): %v", err)
		}
	}
}

// Up is the entrypoint the CLI / UserData (via the systemd unit) calls: do the
// synchronous Setup (Caddyfile + map + caddy + NAT) FIRST, then run the
// route-sync loop and block. Setup ALWAYS completes before the loop, so a
// missing/empty routes table, a missing control-plane param, or a transient
// AWS error in the loop can never strand the Caddy/NAT bring-up.
//
// nat carries the resolved AGENT_OPS_PROXY_NAT decision (the CLI reads the env
// and passes it here so this package stays env-free).
//
// PHASE 1: with no MapSource wired, this renders a working on_demand_tls
// front door that 502s every host (no routes yet) — proving the proxy +
// ask-gate path end to end without migrating any real app onto it. Phase 2
// installs the DDB MapSource and starts populating routes on app lifecycle.
func Up(ctx context.Context, c Config, nat bool) error {
	// SETUP FIRST — synchronous, fail-loud, non-hanging.
	Setup(ctx, c, nat, func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, f+"\n", a...)
	})

	// Initial sync is best-effort: the route source may be unwired (Phase 1),
	// the table empty, or AWS transiently unreachable. NEVER fatal — Setup has
	// already brought up Caddy + NAT; the loop just keeps routes fresh.
	if _, err := Sync(ctx, c); err != nil {
		fmt.Fprintf(os.Stderr, "proxy: initial sync (non-fatal): %v\n", err)
	}

	// Route-sync loop. TOLERATES missing/empty routes table, missing
	// control-plane param, and transient AWS errors: every Sync error is
	// logged and we keep ticking (the ticker is our backoff). The loop never
	// returns except on context cancellation.
	t := time.NewTicker(c.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := Sync(ctx, c); err != nil {
				fmt.Fprintf(os.Stderr, "proxy: sync (non-fatal, will retry): %v\n", err)
			}
		}
	}
}

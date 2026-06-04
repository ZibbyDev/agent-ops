// Copyright 2026 Zibby Lab. Apache-2.0.

package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderCaddyfile_OnDemandTLSAndAsk(t *testing.T) {
	c := Config{
		DomainBase: "apps.zibby.dev",
		AskURL:     "https://api-prod.zibby.app/apps/solo/ingress/allow",
		MapPath:    "/etc/caddy/upstreams.map",
		ACMECa:     "https://acme-v02.api.letsencrypt.org/directory",
	}
	out := RenderCaddyfile(c)

	wants := []string{
		"on_demand_tls",
		"ask https://api-prod.zibby.app/apps/solo/ingress/allow",
		"*.apps.zibby.dev {",
		"on_demand",
		"import /etc/caddy/upstreams.map",
		"reverse_proxy {upstream}",
		"redir https://{host}{uri} permanent", // :80 -> :443
		"acme_ca https://acme-v02.api.letsencrypt.org/directory",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("Caddyfile missing %q\n---\n%s", w, out)
		}
	}
	// Caddy v2.10+ REMOVED on_demand_tls interval/burst (parsing them is a hard
	// error → caddy.service fails to load). Must NOT be present.
	for _, bad := range []string{"interval", "burst"} {
		if strings.Contains(out, bad) {
			t.Errorf("Caddyfile contains removed on_demand_tls option %q (breaks Caddy v2.10+)\n---\n%s", bad, out)
		}
	}
}

func TestRenderMapFile_SortedAndQuoted(t *testing.T) {
	routes := []Route{
		{Host: "b.apps.zibby.dev", Upstream: "10.0.0.2:3000"},
		{Host: "a.apps.zibby.dev", Upstream: "10.0.0.1:8080"},
		{Host: "", Upstream: "10.0.0.9:1"}, // skipped (empty host)
	}
	out := RenderMapFile(routes)
	// a before b (sorted), empty-host line dropped.
	if !strings.Contains(out, `"a.apps.zibby.dev" "10.0.0.1:8080"`) {
		t.Errorf("missing a-host line:\n%s", out)
	}
	idxA := strings.Index(out, "a.apps.zibby.dev")
	idxB := strings.Index(out, "b.apps.zibby.dev")
	if idxA < 0 || idxB < 0 || idxA > idxB {
		t.Errorf("routes not sorted (a should precede b):\n%s", out)
	}
	if strings.Count(out, "\n") != 2 {
		t.Errorf("expected 2 lines (empty-host dropped), got:\n%s", out)
	}
}

// TestEnsureMapFile_CreatesEmptyWhenAbsent proves the upstreams.map import
// target is created (empty) on a fresh/rebooted box — the fix for Caddy
// `failed` after reboot because `import upstreams.map` had no file.
func TestEnsureMapFile_CreatesEmptyWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	mp := filepath.Join(dir, "upstreams.map")
	c := Config{MapPath: mp}

	if _, err := os.Stat(mp); err == nil {
		t.Fatal("precondition: map file should not exist yet")
	}
	if err := EnsureMapFile(c); err != nil {
		t.Fatalf("EnsureMapFile: %v", err)
	}
	b, err := os.ReadFile(mp)
	if err != nil {
		t.Fatalf("map file not created: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("expected empty map file, got %q", b)
	}
}

// TestEnsureMapFile_PreservesExisting proves an existing map (with routes) is
// left untouched — EnsureMapFile only creates when ABSENT (idempotent).
func TestEnsureMapFile_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	mp := filepath.Join(dir, "upstreams.map")
	want := `    "a.apps.zibby.dev" "10.0.0.1:8080"` + "\n"
	if err := os.WriteFile(mp, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMapFile(Config{MapPath: mp}); err != nil {
		t.Fatalf("EnsureMapFile: %v", err)
	}
	got, _ := os.ReadFile(mp)
	if string(got) != want {
		t.Errorf("EnsureMapFile clobbered existing map: got %q want %q", got, want)
	}
}

// recordingRunner records argv sequences for systemctl/NAT calls and returns
// scripted output keyed by the systemctl verb, so Setup ordering + behavior
// can be asserted without touching the host.
type recordingRunner struct {
	calls       [][]string
	caddyActive bool // what `is-active caddy.service` reports
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "systemctl" && len(args) >= 1 && args[0] == "is-active" {
		if r.caddyActive {
			return []byte("active\n"), nil
		}
		return []byte("failed\n"), nil
	}
	return nil, nil
}

func (r *recordingRunner) indexOf(match func([]string) bool) int {
	for i, c := range r.calls {
		if match(c) {
			return i
		}
	}
	return -1
}

// TestSetup_OrderingCaddyAndNATBeforeReturn proves Setup brings up Caddy AND
// NAT (in that order, both BEFORE returning to the caller's sync loop). The
// reboot bug was that the only startup path entered the blocking sync loop
// without ever completing Caddy/NAT bring-up.
func TestSetup_OrderingCaddyAndNATBeforeReturn(t *testing.T) {
	rr := &recordingRunner{caddyActive: true}
	// Both setupRunner (systemctl) and natCmdRunner (sysctl/ip/iptables) route
	// through the recorder so we see the full ordered sequence.
	origSetup, origNat := setupRunner, natCmdRunner
	setupRunner = rr.run
	natCmdRunner = rr.run
	defer func() { setupRunner = origSetup; natCmdRunner = origNat }()

	// Map file in a temp dir so we don't write /etc/caddy. Caddyfile too.
	dir := t.TempDir()
	c := Config{
		DomainBase:    "apps.zibby.dev",
		CaddyfilePath: filepath.Join(dir, "Caddyfile"),
		MapPath:       filepath.Join(dir, "upstreams.map"),
	}

	Setup(context.Background(), c, true /* nat */, nil)

	// (b) map file exists.
	if _, err := os.Stat(c.MapPath); err != nil {
		t.Errorf("Setup did not ensure the map file: %v", err)
	}
	// (c) caddy reload-or-restart happened.
	idxReload := rr.indexOf(func(c []string) bool {
		return len(c) >= 2 && c[0] == "systemctl" && c[1] == "reload-or-restart"
	})
	if idxReload < 0 {
		t.Fatal("Setup did not reload-or-restart caddy")
	}
	// (d) NAT happened AFTER caddy reload (ip_forward via sysctl is the first
	// NAT step).
	idxNat := rr.indexOf(func(c []string) bool {
		return len(c) >= 1 && c[0] == "sysctl"
	})
	if idxNat < 0 {
		t.Fatal("Setup did not run NAT (sysctl ip_forward) when nat=true")
	}
	if idxNat < idxReload {
		t.Errorf("NAT (idx %d) ran BEFORE caddy reload (idx %d); want Caddy first", idxNat, idxReload)
	}
}

// TestSetup_SkipsNATWhenDisabled proves NAT stays gated: nat=false => no
// sysctl/iptables touch (pure-ingress proxy doesn't modify the host firewall).
func TestSetup_SkipsNATWhenDisabled(t *testing.T) {
	rr := &recordingRunner{caddyActive: true}
	origSetup, origNat := setupRunner, natCmdRunner
	setupRunner = rr.run
	natCmdRunner = rr.run
	defer func() { setupRunner = origSetup; natCmdRunner = origNat }()

	dir := t.TempDir()
	c := Config{
		DomainBase:    "apps.zibby.dev",
		CaddyfilePath: filepath.Join(dir, "Caddyfile"),
		MapPath:       filepath.Join(dir, "upstreams.map"),
	}
	Setup(context.Background(), c, false /* nat */, nil)

	for _, call := range rr.calls {
		if len(call) > 0 && (call[0] == "sysctl" || call[0] == "iptables" || call[0] == "ip") {
			t.Errorf("nat=false but Setup touched the host firewall: %v", call)
		}
	}
}

func TestFromEnv_Defaults(t *testing.T) {
	t.Setenv("AGENT_OPS_PROXY_DOMAIN_BASE", "apps.zibby.dev")
	c := FromEnv()
	if c.DomainBase != "apps.zibby.dev" {
		t.Errorf("DomainBase = %q", c.DomainBase)
	}
	if c.ACMECa == "" {
		t.Error("ACMECa should default to Let's Encrypt prod")
	}
	if c.SyncInterval == 0 {
		t.Error("SyncInterval should have a default")
	}
}

// Copyright 2026 Zibby Lab. Apache-2.0.

package proxy

import (
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

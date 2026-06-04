// Copyright 2026 Zibby Lab. Apache-2.0.

// proxy.go — `agent-ops proxy up` subcommand.
//
// Runs agent-ops in SHARED INGRESS mode: a Caddy reverse proxy that
// terminates TLS (on_demand_tls, gated by an `ask` authz endpoint) for many
// private backends and routes by Host header. Invoked by the cloud
// provisioner's UserData (Zibby: SharedIngressNestedStack), but generic —
// every input is an AGENT_OPS_PROXY_* env var.
//
// Hidden from `--help` (infra-invoked, not typed by humans), same as
// `bootstrap`.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ZibbyHQ/agent-ops/internal/proxy"
)

func newProxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "proxy",
		Short:  "Shared-ingress reverse proxy (internal — invoked by cloud UserData).",
		Hidden: true,
	}
	cmd.AddCommand(newProxyUpCmd())
	return cmd
}

func newProxyUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Render the proxy Caddyfile and run the route-sync loop (blocks).",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := proxy.FromEnv()
			// Resolve SSM-param indirection: the CDK UserData passes the
			// ask/control-plane URLs as SSM PARAM NAMES (so they aren't
			// baked into the AMI). If the *_URL env isn't already set but a
			// *_PARAM name is, resolve it via the `aws` CLI (keeps this
			// binary SDK-free + OSS-generic — any operator with the AWS CLI
			// + an instance role can use it).
			if c.AskURL == "" {
				if p := os.Getenv("AGENT_OPS_PROXY_ASK_PARAM"); p != "" {
					if v, err := ssmGet(cmd, p, c.Region); err == nil && v != "" && v != "UNSET" {
						c.AskURL = v
					}
				}
			}
			if c.ControlPlaneBaseURL == "" {
				if p := os.Getenv("AGENT_OPS_PROXY_CP_PARAM"); p != "" {
					if v, err := ssmGet(cmd, p, c.Region); err == nil && v != "" && v != "UNSET" {
						c.ControlPlaneBaseURL = v
					}
				}
			}
			printf(cmd, "proxy: domain-base=%s ask=%s routes-table=%s\n",
				c.DomainBase, c.AskURL, c.RoutesTable)
			// PHASE 2: install the DynamoDB-backed MapSource so the sync loop
			// populates Host->upstream from the control-plane routes table.
			// SDK-free — it shells `aws dynamodb scan` (same convention as
			// ssmGet above), keeping the binary OSS-generic. When no
			// RoutesTable is configured we leave the source unset (Phase 1
			// behavior: 502 every host until a source is wired).
			if c.RoutesTable != "" {
				proxy.SetMapSource(ddbRouteSource(c.RoutesTable, c.Region))
			}
			return proxy.Up(cmd.Context(), c)
		},
	}
}

// ddbRouteSource returns a proxy.MapSource that reads the control-plane
// ingress-routes table and yields the active, shared-mode Host->upstream
// routes. SDK-FREE: it shells `aws dynamodb scan` (same convention as ssmGet)
// so internal/proxy stays free of AWS dependencies and any operator with the
// AWS CLI + an instance role granting dynamodb:Scan on the table can use it.
//
// Item shape (control-plane SCHEMA, ingress-routes-store.js):
//   host "<host>"  upstream "<ip:port>"  status "active"|...  mode "shared"|...
// We project only host+upstream+status+mode and keep rows where
// status==active && mode==shared (a paused/dedicated row is dropped => the
// proxy 502s that host, same UX as a deleted route).
func ddbRouteSource(table, region string) proxy.MapSource {
	return func(ctx context.Context) ([]proxy.Route, error) {
		args := []string{
			"dynamodb", "scan",
			"--table-name", table,
			"--projection-expression", "#h,upstream,#s,#m",
			// host/status/mode are DynamoDB reserved words — alias them.
			"--expression-attribute-names", `{"#h":"host","#s":"status","#m":"mode"}`,
			"--output", "json",
		}
		if region != "" {
			args = append(args, "--region", region)
		}
		out, err := exec.CommandContext(ctx, "aws", args...).Output()
		if err != nil {
			return nil, fmt.Errorf("aws dynamodb scan %s: %w", table, err)
		}
		return parseDDBScan(out)
	}
}

// ddbAttr is a minimal DynamoDB attribute-value decoder — only the S (string)
// form, which is all the route fields use. Avoids pulling in the AWS SDK.
type ddbAttr struct {
	S string `json:"S"`
}

type ddbScanResult struct {
	Items []map[string]ddbAttr `json:"Items"`
}

// parseDDBScan turns `aws dynamodb scan --output json` bytes into the active,
// shared-mode routes. Exported-ish (lowercase) for unit testing the parse +
// filter without a live table.
func parseDDBScan(raw []byte) ([]proxy.Route, error) {
	var res ddbScanResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse dynamodb scan json: %w", err)
	}
	routes := make([]proxy.Route, 0, len(res.Items))
	for _, it := range res.Items {
		host := it["host"].S
		upstream := it["upstream"].S
		status := it["status"].S
		mode := it["mode"].S
		if host == "" || upstream == "" {
			continue
		}
		// Only active, shared-mode routes are served. An empty mode is
		// treated as shared (the store defaults mode to "shared").
		if status != "active" {
			continue
		}
		if mode != "" && mode != "shared" {
			continue
		}
		routes = append(routes, proxy.Route{Host: host, Upstream: upstream})
	}
	return routes, nil
}

// ssmGet resolves an SSM parameter value via the AWS CLI. Returns the
// trimmed value. Kept here (not in internal/proxy) so the proxy package
// stays free of AWS-specifics.
func ssmGet(cmd *cobra.Command, name, region string) (string, error) {
	args := []string{"ssm", "get-parameter", "--name", name, "--query", "Parameter.Value", "--output", "text"}
	if region != "" {
		args = append(args, "--region", region)
	}
	out, err := exec.CommandContext(cmd.Context(), "aws", args...).Output()
	if err != nil {
		return "", fmt.Errorf("aws ssm get-parameter %s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

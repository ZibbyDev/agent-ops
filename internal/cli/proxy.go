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
			// PHASE 2 TODO: proxy.SetMapSource(ddbRouteSource(c.RoutesTable,
			// c.Region)) so the loop populates Host->upstream from the
			// control-plane table. Phase 1 runs with no source (502s every
			// host) to prove the ask-gated TLS front door.
			return proxy.Up(cmd.Context(), c)
		},
	}
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

// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ZibbyDev/agent-ops/internal/integrate"
)

// newIntegrateCmd builds the `agent-ops integrate {add,remove,list}`
// subtree. Each subcommand is a thin wrapper around internal/integrate so
// the on-disk semantics are identical whether the operator drives via CLI
// or a remote agent drives via the MCP `agent_integrate_*` tools.
func newIntegrateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "integrate",
		Short: "Add or remove outbound MCP-client integrations.",
		Long: `An "integration" is one OUTBOUND MCP-server connection the daemon
dials at boot. Tools the remote server advertises become available to
the local LLM driving scheduled tasks, prefixed with the integration
name (e.g. integration "zibby" + remote tool "trigger_workflow"
→ local tool "zibby_trigger_workflow").

This command atomically updates config.yaml + agent-ops.env. The daemon
does not hot-reload — restart with` + " `agent-ops restart` " + `for changes
to take effect.`,
	}
	c.AddCommand(newIntegrateAddCmd(), newIntegrateRemoveCmd(), newIntegrateListCmd())
	return c
}

func newIntegrateAddCmd() *cobra.Command {
	var (
		name      string
		transport string
		url       string
		command   string
		args      []string
		authEnv   string
		token     string
		kv        []string
		stdioKV   []string
		envFile   string
	)
	c := &cobra.Command{
		Use:   "add",
		Short: "Add a new MCP-client integration.",
		Long: `Examples:

  # HTTP integration (Bearer auth via env var)
  agent-ops integrate add \
    --name zibby \
    --transport http \
    --url https://api-prod.zibby.app/mcp \
    --auth-env ZIBBY_PAT_TOKEN \
    --token zby_pat_abc... \
    --kv AGENT_OPS_NOTIFY_WORKFLOW_ID=wf_xxx

  # stdio integration (local subprocess)
  agent-ops integrate add \
    --name jira \
    --transport stdio \
    --command npx --args -y --args @atlassian/jira-mcp-server \
    --stdio-env JIRA_API_TOKEN=xxx
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			extra, err := parseKVPairs(kv)
			if err != nil {
				return err
			}
			stdio, err := parseKVPairs(stdioKV)
			if err != nil {
				return err
			}
			res, err := integrate.Add(integrate.AddSpec{
				Name:      name,
				Transport: transport,
				URL:       url,
				Command:   command,
				Args:      args,
				AuthEnv:   authEnv,
				Token:     token,
				ExtraEnv:  extra,
				StdioEnv:  stdio,
			}, integrate.Options{
				ConfigPath: cfgPath,
				EnvPath:    envFile,
			})
			if err != nil {
				return err
			}
			return printJSON(cmd, res)
		},
	}
	c.Flags().StringVar(&name, "name", "", "unique integration name (also tool-name prefix)")
	c.Flags().StringVar(&transport, "transport", "http", "transport: http | stdio")
	c.Flags().StringVar(&url, "url", "", "MCP endpoint URL (http transport)")
	c.Flags().StringVar(&command, "command", "", "executable (stdio transport)")
	c.Flags().StringArrayVar(&args, "args", nil, "argv after --command (repeat per arg)")
	c.Flags().StringVar(&authEnv, "auth-env", "", "env var holding Bearer token (http transport)")
	c.Flags().StringVar(&token, "token", "", "literal Bearer token to persist into --auth-env")
	c.Flags().StringArrayVar(&kv, "kv", nil, "extra KEY=VAL persisted to agent-ops.env (repeatable)")
	c.Flags().StringArrayVar(&stdioKV, "stdio-env", nil, "subprocess KEY=VAL (stdio only, repeatable)")
	c.Flags().StringVar(&envFile, "env-file", "", "override env-file path (default /etc/agent-ops/agent-ops.env)")
	_ = c.MarkFlagRequired("name")
	return c
}

func newIntegrateRemoveCmd() *cobra.Command {
	var (
		name    string
		envFile string
	)
	c := &cobra.Command{
		Use:   "remove",
		Short: "Remove an integration by name.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			res, err := integrate.Remove(name, integrate.Options{
				ConfigPath: cfgPath,
				EnvPath:    envFile,
			})
			if err != nil {
				return err
			}
			return printJSON(cmd, res)
		},
	}
	c.Flags().StringVar(&name, "name", "", "integration name to remove")
	c.Flags().StringVar(&envFile, "env-file", "", "override env-file path")
	_ = c.MarkFlagRequired("name")
	return c
}

func newIntegrateListCmd() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List configured MCP-client integrations.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			items, err := integrate.List(integrate.Options{ConfigPath: cfgPath})
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd, items)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTRANSPORT\tURL/COMMAND\tAUTH_ENV")
			for _, it := range items {
				target := it.URL
				if it.Transport == "stdio" {
					target = it.Command + " " + strings.Join(it.Args, " ")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", it.Name, it.Transport, target, it.AuthEnv)
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a table")
	return c
}

// parseKVPairs converts ["A=1","B=2"] into a map.
func parseKVPairs(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, raw := range items {
		eq := strings.IndexByte(raw, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("integrate: --kv/--stdio-env entry %q must be KEY=VALUE", raw)
		}
		out[strings.TrimSpace(raw[:eq])] = raw[eq+1:]
	}
	return out, nil
}

func printJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

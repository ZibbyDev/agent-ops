// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ZibbyDev/agent-ops/internal/config"
)

func newScheduleCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "schedule",
		Short: "Inspect and trigger configured schedules.",
	}
	c.AddCommand(newScheduleListCmd(), newScheduleRunCmd())
	return c
}

func newScheduleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print configured cron jobs from config.yaml.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCRON\tENABLED\tTOOLS")
			for _, s := range cfg.Schedules {
				enabled := "true"
				if s.Enabled != nil && !*s.Enabled {
					enabled = "false"
				}
				tools := "[]"
				if len(s.Tools) > 0 {
					tools = fmt.Sprintf("%v", s.Tools)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, s.Cron, enabled, tools)
			}
			if cfg.Bootstrap != nil {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%v\n",
					cfg.Bootstrap.Name, "(first-run only)", "manual", cfg.Bootstrap.Tools)
			}
			return tw.Flush()
		},
	}
}

func newScheduleRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <name>",
		Short: "Trigger one schedule ad-hoc via the daemon's MCP endpoint.",
		Long: `Currently this prints the curl command you would run against the
daemon's MCP server — programmatic trigger needs a daemon-side
RPC that's tracked in v0.3. For now it serves as a discoverable
hook in the CLI rather than a TODO buried in docs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			found := false
			for _, s := range cfg.Schedules {
				if s.Name == args[0] {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("schedule run: no schedule named %q (use `agent-ops schedule list`)", args[0])
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Programmatic trigger lands in v0.3 (issue: control-plane API).")
			fmt.Fprintln(out, "For now, trigger via the daemon's MCP `agent_run_now` tool. Example with curl:")
			fmt.Fprintln(out, "")
			fmt.Fprintf(out,
				"  curl -s -X POST http://127.0.0.1%s/mcp \\\n"+
					"    -H 'Authorization: Bearer $AGENT_OPS_TOKEN' \\\n"+
					"    -H 'Content-Type: application/json' \\\n"+
					"    -d '{\"method\":\"tools/call\",\"params\":{\"name\":\"agent_run_now\",\"arguments\":{\"task\":\"%s\"}}}'\n",
				cfg.MCP.ListenAddr, args[0])
			return nil
		},
	}
}

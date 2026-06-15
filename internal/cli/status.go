// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZibbyDev/agent-ops/internal/config"
	"github.com/ZibbyDev/agent-ops/internal/service"
	"github.com/ZibbyDev/agent-ops/internal/state"
)

func newStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Print service state, configured schedules, and last task result.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			verbose, _ := cmd.Flags().GetBool("verbose")
			return runStatus(cmd.Context(), cmd.OutOrStdout(), cfgPath, verbose)
		},
	}
	c.Flags().BoolP("verbose", "v", false,
		"include raw systemctl / launchctl output")
	return c
}

func runStatus(ctx context.Context, out io.Writer, cfgPath string, verbose bool) error {
	// Service state — best-effort. NewManager error is non-fatal so the user
	// gets the config + schedule view even on an unsupported OS.
	mgr, mgrErr := service.NewManager()
	if mgrErr == nil {
		st, err := mgr.Status(ctx)
		if err == nil {
			fmt.Fprintf(out, "service: installed=%v active=%v unit=%s\n",
				st.Installed, st.Active, mgr.UnitPath())
			if verbose && st.Raw != "" {
				fmt.Fprintln(out, "---")
				fmt.Fprintln(out, st.Raw)
				fmt.Fprintln(out, "---")
			}
		} else {
			fmt.Fprintf(out, "service: status query failed: %v\n", err)
		}
	} else {
		fmt.Fprintf(out, "service: skipped (%v)\n", mgrErr)
	}

	cfg, cfgErr := config.Load(cfgPath)
	if cfgErr != nil {
		fmt.Fprintf(out, "config: load failed (%s): %v\n", cfgPath, cfgErr)
		return nil
	}
	fmt.Fprintf(out, "config: %s\n", cfgPath)
	fmt.Fprintf(out, "provider: %s (model=%s)\n", cfg.Agent.Provider, cfg.Agent.Model)
	fmt.Fprintf(out, "state_dir: %s\n", cfg.StateDir)
	fmt.Fprintf(out, "mcp listen: %s\n", cfg.MCP.ListenAddr)

	// Schedules
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nSCHEDULES")
	fmt.Fprintln(tw, "NAME\tCRON\tENABLED")
	for _, s := range cfg.Schedules {
		enabled := "true"
		if s.Enabled != nil && !*s.Enabled {
			enabled = "false"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, s.Cron, enabled)
	}
	if cfg.Bootstrap != nil {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", cfg.Bootstrap.Name, "(first-run)", "manual")
	}
	tw.Flush()

	// Last task — opens state DB read-only enough; Store.Open uses WAL so
	// the daemon's open handle is fine. We catch + report errors instead of
	// failing the whole command because `status` should always show as much
	// as it can.
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		fmt.Fprintf(out, "\nlast run: state unavailable (%v)\n", err)
		return nil
	}
	defer store.Close()

	runs, err := store.ListRuns(ctx, "", 1)
	if err != nil {
		fmt.Fprintf(out, "\nlast run: query failed: %v\n", err)
		return nil
	}
	if len(runs) == 0 {
		fmt.Fprintln(out, "\nlast run: (none yet)")
		return nil
	}
	r := runs[0]
	fmt.Fprintf(out, "\nlast run: %s task=%s status=%s started=%s",
		r.ID, r.TaskName, r.Status, r.StartedAt.Format(time.RFC3339))
	if !r.EndedAt.IsZero() {
		fmt.Fprintf(out, " duration=%s", r.EndedAt.Sub(r.StartedAt).Round(time.Second))
	}
	fmt.Fprintln(out)
	if r.Summary != "" {
		fmt.Fprintf(out, "  summary: %s\n", truncate(r.Summary, 200))
	}
	if r.Error != "" {
		fmt.Fprintf(out, "  error: %s\n", truncate(r.Error, 200))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

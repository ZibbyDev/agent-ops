// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/ZibbyDev/agent-ops/internal/daemon"
)

// newDaemonCmd exposes the daemon loop as `agent-ops daemon` for parity with
// `agent-opsd`. Hidden from --help because end users go through `start`;
// surfaced only for the (rare) operator who wants to run the daemon in
// foreground for debugging.
func newDaemonCmd(version string) *cobra.Command {
	c := &cobra.Command{
		Use:    "daemon",
		Short:  "Run the daemon loop in the foreground (advanced).",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			logger := slog.New(slog.NewJSONHandler(os.Stdout,
				&slog.HandlerOptions{Level: slog.LevelInfo}))
			slog.SetDefault(logger)
			return daemon.Run(cfgPath, version, logger)
		},
	}
	return c
}

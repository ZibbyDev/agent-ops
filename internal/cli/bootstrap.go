// Copyright 2026 Zibby Lab. Apache-2.0.

// bootstrap.go — `agent-ops bootstrap --from-spec <path>` subcommand.
//
// Used by Zibby solo-mode: cloud-init writes /etc/zibby/spec.json,
// then the AMI's agent-ops.service shells out to
//
//	agent-ops bootstrap --from-spec /etc/zibby/spec.json
//
// which dispatches to internal/bootstrap.RunSoloFromSpec. The daemon
// loop (cmd/agent-opsd) is NOT involved — solo is a one-shot install,
// not a long-running supervisor.
//
// This subcommand is intentionally separate from `agent-ops daemon` so:
//  1. systemd unit can `Type=oneshot` it (no MCP server to keep open).
//  2. Tests can drive a fake spec.json + assert phase + exit code
//     without spinning up the daemon dependencies.
//  3. Future cloud-mode bootstrap paths (agent_script, cheatsheet)
//     stay in the daemon, untouched by this code.
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ZibbyDev/agent-ops/internal/bootstrap"
)

// newBootstrapCmd builds the `bootstrap` subcommand. Hidden from
// `agent-ops --help` since it's invoked by systemd, not humans.
func newBootstrapCmd() *cobra.Command {
	var specPath string
	var logPath string
	cmd := &cobra.Command{
		Use:    "bootstrap",
		Short:  "Run the solo-mode bootstrap from a spec file (internal — invoked by systemd).",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if specPath == "" {
				return fmt.Errorf("bootstrap: --from-spec is required")
			}
			// Resolve to absolute so log lines + marker writes don't
			// depend on the systemd unit's CWD.
			abs, err := filepath.Abs(specPath)
			if err != nil {
				return fmt.Errorf("bootstrap: resolve %s: %w", specPath, err)
			}

			// Logger — JSON to stdout (captured by journalctl) +
			// optionally a file for CloudWatch agent pickup.
			handlerOpts := &slog.HandlerOptions{Level: slog.LevelInfo}
			var logger *slog.Logger
			if logPath != "" {
				_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
				f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
				if ferr == nil {
					// Tee — write to both file and stdout so an operator
					// can `journalctl -u agent-ops` AND tail the file.
					logger = slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, f), handlerOpts))
				}
			}
			if logger == nil {
				logger = slog.New(slog.NewJSONHandler(os.Stdout, handlerOpts))
			}
			slog.SetDefault(logger)

			paths := bootstrap.DefaultSoloPaths()
			paths.SpecPath = abs
			if err := bootstrap.RunSoloFromSpec(cmd.Context(), paths, logger); err != nil {
				// RunSoloFromSpec already pushed PhaseFailed + wrote
				// the marker. Returning err here exits non-zero so
				// systemd's Restart=on-failure DOESN'T loop (the
				// failed marker also blocks the next attempt).
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&specPath, "from-spec", "", "Path to the SoloDeploySpec JSON file (e.g. /etc/zibby/spec.json).")
	cmd.Flags().StringVar(&logPath, "log", "/var/log/zibby/agent.log", "Path to write the bootstrap log (in addition to stdout/journalctl).")
	return cmd
}

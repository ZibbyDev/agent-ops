// Copyright 2026 Zibby Lab. Apache-2.0.

// Package cli builds the cobra command tree for the user-facing `agent-ops`
// binary. The daemon binary (`agent-opsd`) intentionally stays separate so
// its CLI surface — frozen by systemd / launchd unit ExecStart lines that
// are already in production — doesn't change.
//
// Subcommand tree:
//
//	agent-ops
//	├── version
//	├── daemon          (hidden — used by systemd / launchd unit if operator opts in)
//	├── init            (interactive config + service install)
//	├── start | stop | restart
//	├── status
//	├── logs
//	├── doctor
//	├── uninstall
//	├── schedule
//	│   ├── list
//	│   └── run <name>
//	├── mcp
//	│   └── token
//	└── integrate
//	    ├── add
//	    ├── remove
//	    └── list
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// New builds the root command. version is baked in via -ldflags by the
// release pipeline; tests pass "test" so output is deterministic.
func New(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "agent-ops",
		Short: "Autonomous DevOps engineer for a single host.",
		Long: `agent-ops runs a small LLM-backed agent next to your application,
firing scheduled and on-demand ops tasks (health checks, upgrades,
incident response) using a configurable provider (Claude / Codex /
local Ollama).

Install once with ` + "`agent-ops init`" + `, then ` + "`agent-ops start`" + ` runs the
daemon under systemd (Linux) or launchd (macOS). All other subcommands
talk to that daemon — they do NOT spawn one transiently.`,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	// Persistent --config flag is inherited by every subcommand that needs
	// to read the config file (doctor, schedule list, status, …). Default
	// matches the daemon's default.
	root.PersistentFlags().StringP("config", "c", "/etc/agent-ops/config.yaml",
		"path to agent-ops config.yaml")

	root.AddCommand(
		newVersionCmd(version),
		newDaemonCmd(version),
		newInitCmd(),
		newStartCmd(),
		newStopCmd(),
		newRestartCmd(),
		newStatusCmd(),
		newLogsCmd(),
		newDoctorCmd(),
		newUninstallCmd(),
		newScheduleCmd(),
		newMCPCmd(),
		newIntegrateCmd(),
		// `bootstrap --from-spec` — Zibby solo-mode entrypoint invoked
		// by the AMI's systemd unit. Hidden from `--help` since humans
		// don't type it directly.
		newBootstrapCmd(),
		// `proxy up` — Zibby shared-ingress entrypoint invoked by the
		// cloud provisioner's UserData. Hidden (infra-invoked, not human).
		newProxyCmd(),
	)
	return root
}

// printf writes to cmd.OutOrStdout. Centralized so tests can capture output.
func printf(cmd *cobra.Command, format string, args ...any) {
	fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}

// printlnTo wraps fmt.Fprintln targeting cmd's stdout.
func printlnTo(w io.Writer, args ...any) {
	fmt.Fprintln(w, args...)
}

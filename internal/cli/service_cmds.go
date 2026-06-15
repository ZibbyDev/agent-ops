// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZibbyDev/agent-ops/internal/service"
)

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Install (if needed) and start the agent-ops service.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := service.NewManager()
			if err != nil {
				return err
			}
			cfgPath, _ := cmd.Flags().GetString("config")
			// Install lazily — most operators come here AFTER `init`, but
			// `start` on a fresh box without `init` still works as long as
			// the config exists. We don't run init's interactive flow here.
			if _, statErr := os.Stat(mgr.UnitPath()); errors.Is(statErr, os.ErrNotExist) {
				if _, cfgStatErr := os.Stat(cfgPath); cfgStatErr != nil {
					return fmt.Errorf("start: %s missing — run `agent-ops init` first", cfgPath)
				}
				if err := mgr.Install(cmd.Context(), serviceSpecFor(cfgPath, defaultStateDir())); err != nil {
					return fmt.Errorf("start: install service: %w", err)
				}
				printlnTo(cmd.OutOrStdout(), "installed unit at "+mgr.UnitPath())
			}
			if err := mgr.Start(cmd.Context()); err != nil {
				return err
			}
			// Wait up to 10s for the daemon to report active. The plan asked
			// for this — early feedback when the unit started but the binary
			// crashed mid-boot.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				st, _ := mgr.Status(cmd.Context())
				if st.Active {
					printlnTo(cmd.OutOrStdout(), "agent-ops is active.")
					return nil
				}
				time.Sleep(500 * time.Millisecond)
			}
			printlnTo(cmd.OutOrStdout(), "agent-ops started but did not report active within 10s — run `agent-ops status` for details.")
			return nil
		},
	}
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the agent-ops service.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := service.NewManager()
			if err != nil {
				return err
			}
			if err := mgr.Stop(cmd.Context()); err != nil {
				return err
			}
			printlnTo(cmd.OutOrStdout(), "agent-ops stopped.")
			return nil
		},
	}
}

func newRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the agent-ops service.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := service.NewManager()
			if err != nil {
				return err
			}
			if err := mgr.Restart(cmd.Context()); err != nil {
				return err
			}
			printlnTo(cmd.OutOrStdout(), "agent-ops restarted.")
			return nil
		},
	}
}

func newUninstallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the service and remove the unit file.",
		Long: `Stops the daemon, disables auto-start, and removes the systemd
unit / launchd plist. Config and state are kept by default —
pass --purge to remove them too.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := service.NewManager()
			if err != nil {
				return err
			}
			if err := mgr.Uninstall(cmd.Context()); err != nil {
				return err
			}
			printlnTo(cmd.OutOrStdout(), "removed "+mgr.UnitPath())

			purge, _ := cmd.Flags().GetBool("purge")
			if purge {
				cfg, _ := cmd.Flags().GetString("config")
				if cfg != "" {
					_ = os.Remove(cfg)
					printlnTo(cmd.OutOrStdout(), "removed "+cfg)
				}
				// State dir is best-effort — we don't recurse if RemoveAll
				// would be too aggressive in unusual paths.
				state := defaultStateDir()
				if err := os.RemoveAll(state); err == nil {
					printlnTo(cmd.OutOrStdout(), "removed "+state)
				}
			}
			return nil
		},
	}
	c.Flags().Bool("purge", false, "also remove config + state")
	return c
}

func newLogsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "logs",
		Short: "Tail agent-ops logs (journalctl on Linux, log file on macOS).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := service.NewManager()
			if err != nil {
				return err
			}
			since, _ := cmd.Flags().GetString("since")
			follow, _ := cmd.Flags().GetBool("follow")
			argv := mgr.FollowLogsCmd(since)
			// `--follow` is the default for the plan's `logs -f` UX. When
			// !follow, we drop the `-f` from journalctl + emulate `cat` on
			// the launchd log file.
			if !follow {
				argv = withoutFollowFlag(argv)
			}
			return runForeground(cmd.Context(), argv)
		},
	}
	c.Flags().StringP("since", "s", "",
		"only show entries since this duration (e.g. 1h, 30m) — Linux only")
	c.Flags().BoolP("follow", "f", true,
		"stream new lines as they arrive (default true)")
	return c
}

// withoutFollowFlag drops `-f` from journalctl argv or swaps `tail -f` for
// `tail -n 200`. Used when `logs --follow=false` is set.
func withoutFollowFlag(argv []string) []string {
	out := make([]string, 0, len(argv))
	swapped := false
	for _, a := range argv {
		switch a {
		case "-f":
			continue
		case "tail":
			out = append(out, "tail")
			swapped = true
		default:
			out = append(out, a)
		}
	}
	if swapped {
		// "tail -f <file>" became "tail <file>" — convert to "tail -n 200 <file>"
		// for a useful one-shot dump.
		return []string{out[0], "-n", "200", out[len(out)-1]}
	}
	return out
}

// runForeground execs argv with stdio wired to the user's terminal, blocking
// until it exits. Used by logs / schedule run.
func runForeground(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("internal: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZibbyDev/agent-ops/internal/config"
)

// newDoctorCmd runs a battery of "is this host ready?" checks. Each check
// outputs one line ("ok" / "warn" / "fail"); we exit non-zero only when at
// least one fail is found, so CI can wire this as a smoke test.
//
// Designed to be safe to run without sudo: every check is read-only or
// best-effort.
func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Self-check: config, provider binary, state dir, network.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			fail := runDoctor(cmd.Context(), cmd.OutOrStdout(), cfgPath)
			if fail > 0 {
				return fmt.Errorf("doctor: %d check(s) failed", fail)
			}
			return nil
		},
	}
}

// providerSpec captures the per-provider expectations doctor checks against.
// Centralised so the test table + the doctor body agree on the install
// commands printed to operators.
//
// Why both an "env var" + "binary"?
//   - "claude" (REST) needs ANTHROPIC_API_KEY but no extra CLI binary.
//   - "claude-cli" needs CLAUDE_CODE_OAUTH_TOKEN (read by the `claude`
//     binary itself, not by agent-ops) AND the `claude` binary on PATH.
//   - "codex" needs OPENAI_API_KEY AND the `codex` binary on PATH.
//
// The install hint strings here are the ones printed verbatim to users —
// keep them copy-pasteable. README and the install runbook quote them.
type providerSpec struct {
	// envVar is the env var that must be set; "" means no env-var check
	// (e.g. ollama runs fully local).
	envVar string
	// binary is the CLI binary that must be on PATH; "" means no binary
	// check (e.g. provider=claude uses the REST API directly).
	binary string
	// installHint is the multi-line remediation printed under a missing
	// binary [fail] line. Includes the exact `npm install -g` line + Node
	// version requirement + a NodeSource bootstrap one-liner.
	installHint string
	// networkHost is the host:port checked by the network probe. Empty
	// disables the network check (e.g. ollama, which is local-only).
	networkHost string
}

// providerSpecs is the source of truth for doctor's per-provider behaviour.
// Tests reference the install hint strings via providerSpecs[name].installHint
// so a copy-edit here flows through to the test fixture without manual sync.
var providerSpecs = map[string]providerSpec{
	"claude": {
		envVar:      "ANTHROPIC_API_KEY",
		binary:      "",
		installHint: "",
		networkHost: "api.anthropic.com:443",
	},
	"claude-cli": {
		envVar: "CLAUDE_CODE_OAUTH_TOKEN",
		binary: "claude",
		installHint: "       install: sudo npm install -g @anthropic-ai/claude-code\n" +
			"       requires: Node.js 20+\n" +
			"       no node? curl -fsSL https://deb.nodesource.com/setup_20.x | sudo bash - && sudo apt install -y nodejs\n",
		networkHost: "api.anthropic.com:443",
	},
	"codex": {
		envVar: "OPENAI_API_KEY",
		binary: "codex",
		installHint: "       install: sudo npm install -g @openai/codex\n" +
			"       requires: Node.js 22+\n" +
			"       no node? curl -fsSL https://deb.nodesource.com/setup_22.x | sudo bash - && sudo apt install -y nodejs\n",
		networkHost: "api.openai.com:443",
	},
	"gemini": {
		envVar:      "GEMINI_API_KEY",
		binary:      "",
		installHint: "",
		networkHost: "generativelanguage.googleapis.com:443",
	},
	"ollama": {
		envVar:      "",
		binary:      "",
		installHint: "",
		networkHost: "",
	},
}

// runDoctor writes one human-readable line per check + returns the number of
// failures. Exposed (lowercase) so future tests can drive it without going
// through cobra.
func runDoctor(_ context.Context, out io.Writer, cfgPath string) int {
	fails := 0
	passes := 0

	// Config readable?
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(out, "[fail] config %s: %v\n", cfgPath, err)
		fails++
		// Still try the rest of the checks with a zero-value cfg so the
		// operator gets a full picture.
		cfg = &config.Config{}
	} else {
		fmt.Fprintf(out, "[ok]   config parsed: provider=%s model=%s\n",
			cfg.Agent.Provider, cfg.Agent.Model)
		passes++
	}

	spec, hasSpec := providerSpecs[cfg.Agent.Provider]

	// Per-provider auth env var. Missing → [fail] (was [warn]) so a host
	// that can't possibly start the daemon doesn't masquerade as "mostly ok".
	if hasSpec && spec.envVar != "" {
		if v := os.Getenv(spec.envVar); v == "" {
			fmt.Fprintf(out, "[fail] env %s is unset — required for provider=%s; export it before starting the daemon\n",
				spec.envVar, cfg.Agent.Provider)
			fails++
		} else {
			fmt.Fprintf(out, "[ok]   env %s is set (%d chars)\n", spec.envVar, len(v))
			passes++
		}
	} else if hasSpec && spec.envVar == "" {
		// Local provider (ollama) or none required.
		fmt.Fprintf(out, "[ok]   provider=%s needs no auth env var\n", cfg.Agent.Provider)
		passes++
	}

	// Per-provider CLI binary on PATH.
	if hasSpec && spec.binary != "" {
		if p, err := exec.LookPath(spec.binary); err == nil {
			fmt.Fprintf(out, "[ok]   %s on PATH (%s)\n", spec.binary, p)
			passes++
		} else {
			fmt.Fprintf(out, "[fail] %s NOT on PATH — required for provider=%s\n",
				spec.binary, cfg.Agent.Provider)
			if spec.installHint != "" {
				fmt.Fprint(out, spec.installHint)
			}
			fails++
		}
	}

	// State dir writable?
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	if err := checkWritable(stateDir); err != nil {
		fmt.Fprintf(out, "[fail] state dir %s: %v\n", stateDir, err)
		fails++
	} else {
		fmt.Fprintf(out, "[ok]   state dir %s is writable\n", stateDir)
		passes++
	}

	// MCP listen addr — does anything already own it?
	addr := cfg.MCP.ListenAddr
	if addr == "" {
		addr = ":7842"
	}
	if err := checkPortFree(addr); err != nil {
		fmt.Fprintf(out, "[warn] MCP addr %s appears busy: %v\n", addr, err)
	} else {
		fmt.Fprintf(out, "[ok]   MCP addr %s is free\n", addr)
		passes++
	}

	// Network — provider-aware. Skip entirely for local providers (ollama)
	// and unknown providers (treat as "we don't know what host to ping").
	if hasSpec && spec.networkHost != "" {
		if err := checkNetwork(spec.networkHost); err != nil {
			fmt.Fprintf(out, "[warn] %s unreachable: %v\n", spec.networkHost, err)
		} else {
			fmt.Fprintf(out, "[ok]   %s reachable\n", spec.networkHost)
			passes++
		}
	}

	fmt.Fprintf(out, "[summary] %d pass, %d fail\n", passes, fails)
	return fails
}

func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return err
	}
	return os.Remove(probe)
}

func checkPortFree(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	_ = l.Close()
	return nil
}

func checkNetwork(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

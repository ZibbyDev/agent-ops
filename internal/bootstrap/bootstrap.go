// Copyright 2026 Zibby Lab. Apache-2.0.

// Package bootstrap performs first-run initialization for the daemon:
// generates/loads the MCP bearer token, persists it under <state>/mcp.token,
// and (when a bootstrap schedule is configured) runs it exactly once.
//
// "First run" is detected by the presence of <state>/bootstrap.done; this is
// a single file rather than a state.events query so a future Raft replay
// doesn't trigger a fresh bootstrap on every follower.
package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/config"
	"github.com/ZibbyHQ/agent-ops/internal/scheduler"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/task"
	"github.com/ZibbyHQ/agent-ops/internal/zibby"
)

// EnsureToken returns the daemon's MCP bearer token. Priority:
//  1. AGENT_OPS_TOKEN env var (the Zibby control plane sets this when
//     provisioning a sidecar — token is already known upstream)
//  2. <stateDir>/mcp.token file (preserved across restarts)
//  3. A fresh 32-byte random token, persisted to (2)
func EnsureToken(stateDir, envName string) (string, error) {
	if envName != "" {
		if v := os.Getenv(envName); v != "" {
			return v, nil
		}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("bootstrap.EnsureToken: state dir: %w", err)
	}
	path := filepath.Join(stateDir, "mcp.token")
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		return string(raw), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("bootstrap.EnsureToken: read: %w", err)
	}
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", fmt.Errorf("bootstrap.EnsureToken: persist: %w", err)
	}
	return tok, nil
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "ao_" + hex.EncodeToString(buf), nil
}

// MaybeRunFirstRun fires the configured Bootstrap schedule exactly once if
// <stateDir>/bootstrap.done is absent. Idempotent — subsequent restarts skip.
func MaybeRunFirstRun(
	ctx context.Context,
	cfg *config.Config,
	sched *scheduler.Scheduler,
	store *state.Store,
) error {
	if cfg.Bootstrap == nil {
		return nil
	}
	marker := filepath.Join(cfg.StateDir, "bootstrap.done")
	if _, err := os.Stat(marker); err == nil {
		return nil // already done
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("bootstrap.MaybeRunFirstRun: stat marker: %w", err)
	}

	// Persist the bootstrap as a normal task so MCP can re-run it ad-hoc.
	t := state.Task{
		Name:    cfg.Bootstrap.Name,
		Cron:    "@yearly", // never auto-fires; bootstrap is manual-only thereafter
		Prompt:  cfg.Bootstrap.Prompt,
		Tools:   cfg.Bootstrap.Tools,
		Enabled: false,
	}
	if err := store.UpsertTask(ctx, t); err != nil {
		return err
	}

	// Trigger one synchronous run. Pass through the scheduler so the same
	// path is used as RunNow + tests.
	slog.Info("bootstrap: invoking first-run task", "name", t.Name)
	run, err := sched.RunNow(ctx, t.Name, t.Prompt)
	if err != nil {
		// Don't write the marker on failure — operator may want to retry
		// once they fix what was broken.
		return fmt.Errorf("bootstrap: first-run task failed: %w", err)
	}

	// Surface what the agent actually did. Without this the daemon goes
	// silent between "mcp token ready" and "mcp server listening" while
	// the LLM works, and the operator sees nothing in CloudWatch even
	// after the run completes — the result lives only in SQLite. Logging
	// the post-run summary is the cheapest way to make container logs
	// useful for inspecting bootstrap outcomes.
	summary := strings.TrimSpace(run.Summary)
	if summary == "" {
		summary = "(no summary returned by bootstrap agent)"
	}
	slog.Info("bootstrap: first-run task complete",
		"run_id", run.ID,
		"status", run.Status,
		"tool_calls", run.ToolCalls,
		"cost_usd_micro", run.CostUSDMicro,
		"summary", truncate(summary, 1200),
		"error", run.Error,
	)

	if _, addErr := store.AddFact(ctx, "bootstrap",
		"Initial setup completed at "+time.Now().UTC().Format(time.RFC3339)+
			": "+truncate(summary, 600)); addErr != nil {
		// Non-fatal — bootstrap succeeded, just couldn't write the fact.
		// Log into the run's log so it's discoverable.
		_ = store.AppendRunLog(ctx, run.ID, 99999, "warn",
			"could not persist bootstrap fact: "+addErr.Error())
	}

	// Tell the Zibby control plane (if integrated) which port the
	// installed app picked, so ALB host-routing can be wired. Silent no-op
	// when ZIBBY_API_BASE_URL / INSTANCE_ID / AGENT_OPS_TOKEN aren't set.
	mcpPort := parseMCPPort(cfg.MCP.ListenAddr)
	zibby.RegisterPortIfNeeded(ctx, mcpPort)

	return os.WriteFile(marker, []byte("ok"), 0o600)
	// the marker has no business value beyond "we've been here" — its
	// presence alone keeps subsequent restarts idempotent.
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseMCPPort extracts the numeric port from cfg.MCP.ListenAddr, which is
// typically ":7842" or "0.0.0.0:7842". Returns 7842 as a safe default when
// parsing fails — only used to exclude the daemon's own listener when
// scanning for the installed app's port.
func parseMCPPort(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 7842
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 7842
	}
	return n
}

// We use task.TriggerBootstrap directly nowhere in this file (RunNow tags it
// as TriggerManual), but the constant is re-exported here so future cluster
// code can label cluster-induced bootstraps separately.
var _ = task.TriggerBootstrap

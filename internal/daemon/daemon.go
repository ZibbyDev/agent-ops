// Copyright 2026 Zibby Lab. Apache-2.0.

// Package daemon contains the long-running supervisor loop that backs both
// the `agent-opsd` binary (back-compat entrypoint baked into systemd /
// launchd unit files of already-deployed Fargate task defs) and the new
// `agent-ops daemon` subcommand. Keeping the loop here — rather than in
// cmd/agent-opsd/main.go where it used to live — means there is exactly
// ONE source of truth for daemon lifecycle, and `agent-ops start` /
// `agent-ops doctor` can read the same package without circular-importing
// `main`.
//
// Lifecycle:
//  1. Load + validate config
//  2. Ensure state directory + bearer token exist
//  3. Open SQLite state
//  4. Register host tools (shell)
//  5. Construct Driver (claude in v0.1)
//  6. Hydrate scheduler from State + Config
//  7. Start MCP HTTP server + cron scheduler
//  8. Run first-run bootstrap once (if configured)
//  9. Block on signals; gracefully drain on SIGTERM / SIGINT
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/api/mcp"
	"github.com/ZibbyHQ/agent-ops/internal/bootstrap"
	"github.com/ZibbyHQ/agent-ops/internal/config"
	"github.com/ZibbyHQ/agent-ops/internal/disku"
	"github.com/ZibbyHQ/agent-ops/internal/driver"
	"github.com/ZibbyHQ/agent-ops/internal/driver/claude"
	"github.com/ZibbyHQ/agent-ops/internal/driver/claudecli"
	"github.com/ZibbyHQ/agent-ops/internal/driver/codex"
	"github.com/ZibbyHQ/agent-ops/internal/node"
	"github.com/ZibbyHQ/agent-ops/internal/runreport"
	"github.com/ZibbyHQ/agent-ops/internal/scheduler"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/task"
	"github.com/ZibbyHQ/agent-ops/internal/tool"
)

// Run blocks executing the daemon loop. Returns when a signal causes a
// graceful shutdown, or with an error if a startup step fails.
//
// `version` is supplied by the caller so cmd/agent-opsd and cmd/agent-ops
// (which carry independent -ldflags-baked `main.version` strings) feed the
// MCP server a coherent server_version string.
func Run(cfgPath, version string, logger *slog.Logger) error {
	logger.Info("agent-ops starting", "version", version, "config", cfgPath)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Identity
	n, err := node.LoadOrInit(cfg.StateDir)
	if err != nil {
		return err
	}
	logger.Info("node identity", "id", string(n.ID()), "role", string(n.Role()))

	// State store
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	defer store.Close()

	// Tools
	tools := tool.NewRegistry()
	tools.MustRegister(tool.NewShellTool())
	// v0.1 ships shell only — pure OSS daemon. Vendor-coupled integrations
	// (Slack, Jira, vendor APIs, etc.) belong in a flavoured image that
	// installs the vendor's CLI and lets the agent shell-out to it.

	// Driver
	d, err := buildDriver(cfg)
	if err != nil {
		return fmt.Errorf("driver: %w", err)
	}

	// Task runner
	runner := task.NewRunner(d, tools, store)
	runner.MaxToolCalls = cfg.Agent.MaxToolCallsPerTask
	if cfg.Agent.TaskTimeout > 0 {
		runner.TaskTimeout = cfg.Agent.TaskTimeout
	}
	runner.Reporter = runreport.NewHTTPReporter()

	// Scheduler
	sched := scheduler.New(runner, store, logger)
	ctx, cancel := signalContext()
	defer cancel()
	if err := sched.Hydrate(ctx, cfg); err != nil {
		return fmt.Errorf("scheduler.Hydrate: %w", err)
	}
	sched.Start()

	// Per-instance EFS usage emitter.
	disku.Start(ctx, logger, cfg.StateDir, 60*time.Second)

	// MCP server token
	tok, err := bootstrap.EnsureToken(cfg.StateDir, cfg.MCP.TokenEnv)
	if err != nil {
		return err
	}
	logger.Info("mcp token ready", "token_prefix", tok[:8])

	mcpSrv, err := mcp.New(mcp.Config{
		Scheduler:     sched,
		Store:         store,
		Tools:         tools,
		Token:         tok,
		Logger:        logger,
		ConfigPath:    cfgPath,
		ServerName:    "agent-ops",
		ServerVersion: version,
	})
	if err != nil {
		return fmt.Errorf("mcp.New: %w", err)
	}

	httpSrv := &http.Server{
		Addr:         cfg.MCP.ListenAddr,
		Handler:      mcpSrv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming GET can stay open
		IdleTimeout:  120 * time.Second,
	}

	// First-run bootstrap — sync; failure exits the daemon.
	if err := bootstrap.MaybeRunFirstRun(ctx, cfg, sched, store); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	httpErr := make(chan error, 1)
	go func() {
		logger.Info("mcp server listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("daemon: shutdown signal received")
			return shutdown(httpSrv, sched, logger)
		case err := <-httpErr:
			return fmt.Errorf("http server: %w", err)
		case <-heartbeat.C:
			n.Touch()
		}
	}
}

func buildDriver(cfg *config.Config) (driver.Driver, error) {
	switch cfg.Agent.Provider {
	case "claude":
		key := os.Getenv(cfg.Agent.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("env %s is empty (required for provider=claude)", cfg.Agent.APIKeyEnv)
		}
		return &claude.Driver{
			APIKey:          key,
			Model:           cfg.Agent.Model,
			MaxOutputTokens: 4096,
		}, nil
	case "claude-cli":
		d := &claudecli.Driver{Model: cfg.Agent.Model}
		if err := d.Preflight(); err != nil {
			return nil, err
		}
		return d, nil
	case "codex":
		d := &codex.Driver{Model: cfg.Agent.Model}
		if err := d.Preflight(); err != nil {
			return nil, err
		}
		return d, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (v0.1 ships claude + claude-cli + codex)", cfg.Agent.Provider)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

func shutdown(httpSrv *http.Server, sched *scheduler.Scheduler, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Warn("http shutdown", "error", err)
	}
	if err := sched.Stop(ctx); err != nil {
		logger.Warn("scheduler stop", "error", err)
		return err
	}
	logger.Info("daemon: shutdown complete")
	return nil
}

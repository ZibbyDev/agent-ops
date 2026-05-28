// Copyright 2026 Zibby Lab. Apache-2.0.

// agent-opsd is the agent-ops daemon binary.
//
// Usage:
//   agent-opsd --config /etc/agent-ops/config.yaml
//   agent-opsd version
//
// Lifecycle:
//   1. Load + validate config
//   2. Ensure state directory + bearer token exist
//   3. Open SQLite state
//   4. Register host tools (shell)
//   5. Construct Driver (claude in v0.1)
//   6. Hydrate scheduler from State + Config
//   7. Start MCP HTTP server + cron scheduler
//   8. Run first-run bootstrap once (if configured)
//   9. Block on signals; gracefully drain on SIGTERM / SIGINT
package main

import (
	"context"
	"errors"
	"flag"
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
	"github.com/ZibbyHQ/agent-ops/internal/node"
	"github.com/ZibbyHQ/agent-ops/internal/scheduler"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/task"
	"github.com/ZibbyHQ/agent-ops/internal/tool"
)

// version is set via -ldflags by the release pipeline.
var version = "0.1.19"

func main() {
	// Subcommand routing: `agent-opsd version` short-circuits config load.
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}

	cfgPath := flag.String("config", "/etc/agent-ops/config.yaml", "path to YAML config")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(*cfgPath, logger); err != nil {
		logger.Error("daemon: fatal", "error", err)
		os.Exit(1)
	}
}

func run(cfgPath string, logger *slog.Logger) error {
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
	// zibby_workflow closes the agent→user comm loop: the LLM can fire
	// a user-defined Zibby workflow (Slack notify, page on-call, open
	// Jira ticket, etc.) when shell evidence warrants. The tool is a
	// no-op when ZIBBY_API_BASE_URL / ZIBBY_PAT_TOKEN / ZIBBY_PROJECT_ID
	// aren't set, so non-Zibby deployments don't see surprise failures.
	tools.MustRegister(tool.NewZibbyWorkflowTool())
	// (v0.1 ships shell + zibby_workflow; fs/http/docker land in v0.2)

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

	// Scheduler
	sched := scheduler.New(runner, store, logger)
	ctx, cancel := signalContext()
	defer cancel()
	if err := sched.Hydrate(ctx, cfg); err != nil {
		return fmt.Errorf("scheduler.Hydrate: %w", err)
	}
	sched.Start()

	// Per-instance EFS usage emitter. AWS does not expose StorageBytes
	// per EFS access point — only per filesystem — so the only way to
	// surface "MB used" in the per-instance UI is to measure from inside
	// the container and log the number. The control plane reads these
	// `msg=efs_usage` lines from CloudWatch to fill the Storage column.
	// 60s cadence is cheap on the small (<10GB) EFS volumes managed apps
	// use; bump if a workload turns out to make this expensive.
	disku.Start(ctx, logger, cfg.StateDir, 60*time.Second)

	// MCP server token
	tok, err := bootstrap.EnsureToken(cfg.StateDir, cfg.MCP.TokenEnv)
	if err != nil {
		return err
	}
	logger.Info("mcp token ready", "token_prefix", tok[:8])

	mcpSrv := mcp.New(mcp.Config{
		Scheduler:     sched,
		Store:         store,
		Tools:         tools,
		Token:         tok,
		Logger:        logger,
		ServerName:    "agent-ops",
		ServerVersion: version,
	})

	httpSrv := &http.Server{
		Addr:         cfg.MCP.ListenAddr,
		Handler:      mcpSrv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming GET can stay open
		IdleTimeout:  120 * time.Second,
	}

	// First-run bootstrap — sync; failure exits the daemon so the operator
	// notices early (rather than running for hours with a half-set-up host).
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

	// Heartbeat tick to keep node.LastSeen monotone. Cluster mode will turn
	// this into the Raft heartbeat hook.
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
		// Shells out to the `claude` Code CLI binary; auth via
		// CLAUDE_CODE_OAUTH_TOKEN env (read by the CLI itself, not us).
		d := &claudecli.Driver{Model: cfg.Agent.Model}
		if err := d.Preflight(); err != nil {
			return nil, err
		}
		return d, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (v0.1 ships claude + claude-cli)", cfg.Agent.Provider)
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
	// 30s budget: tear down HTTP first (refuse new MCP calls), then drain
	// in-flight cron ticks.
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

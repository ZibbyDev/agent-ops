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
	"strings"
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
	"github.com/ZibbyHQ/agent-ops/internal/mcpclient"
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
	// `shell` is always present. v0.3 adds OPTIONAL MCP-client integrations
	// (see internal/mcpclient + cfg.MCPClients) which register additional
	// remote tools below — wholly off by default; existing v0.2.x configs
	// without `mcp_clients:` see zero behavior change.

	// Outbound MCP clients. Each entry in cfg.MCPClients becomes one
	// supervised connection; tools the remote server advertises are
	// registered into the local registry under `{name}_{remoteName}`.
	mcpMgr, started, err := bootMCPClients(ctx0(), cfg, logger)
	if err != nil {
		// Boot itself only returns errors for unrecoverable internal
		// problems (slog nil etc) — per-client failures are logged + skipped
		// so a bad integration cannot prevent the daemon from starting.
		return fmt.Errorf("mcp clients: %w", err)
	}
	defer mcpMgr.Close()
	registerRemoteTools(tools, started, logger)

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
	// Gate the AUTONOMOUS scheduler (and first-run below). The daemon itself
	// always comes up — MCP server, `zibby app run`, log tailing all work
	// regardless. We only skip starting scheduled agent runs when the app
	// has agent-ops turned off, or when there's no Claude token to run with.
	schedOn, schedReason := schedulerGate(cfg)
	if schedOn {
		sched.Start()
	} else {
		logger.Info("agent-ops scheduler idle (not started)", "reason", schedReason)
	}

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

	// First-run bootstrap — sync; failure exits the daemon. Skipped when the
	// scheduler is gated off (disabled or no token), same as the scheduled
	// runs above — a paused / token-less app must not kick off agent work.
	if schedOn {
		if err := bootstrap.MaybeRunFirstRun(ctx, cfg, sched, store); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	} else {
		logger.Info("first-run bootstrap skipped (scheduler idle)", "reason", schedReason)
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

// schedulerGate decides whether the autonomous scheduler + first-run should
// run. Returns (run, reason). It never affects whether the daemon starts —
// only whether agent-ops fires scheduled / first-run agent activity.
//
// Two reasons to idle:
//  1. AGENT_OPS_SCHEDULER_ENABLED is explicitly falsey (false/0/off/no/
//     disabled) — the Zibby dashboard's per-app agent-ops toggle sets this
//     on the task-def / instance env so users can pause agent-ops without
//     destroying the app.
//  2. No Claude token is configured — scheduled runs would only fail, so we
//     idle rather than crash-loop. "Run if a token is present; don't if it
//     isn't." The daemon stays up so the app keeps serving + `zibby app run`
//     still works (interactive runs supply their own token at exec time).
func schedulerGate(cfg *config.Config) (bool, string) {
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_SCHEDULER_ENABLED")); v != "" {
		switch strings.ToLower(v) {
		case "false", "0", "off", "no", "disabled":
			return false, "AGENT_OPS_SCHEDULER_ENABLED=" + v
		}
	}
	if !hasAgentToken(cfg) {
		return false, "no Claude token configured (CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY unset)"
	}
	return true, ""
}

// hasAgentToken reports whether any Claude credential the scheduler could run
// with is present in the environment: the claude-cli OAuth token, the
// api-key path, or whatever env the config names.
func hasAgentToken(cfg *config.Config) bool {
	envs := []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}
	if cfg != nil && strings.TrimSpace(cfg.Agent.APIKeyEnv) != "" {
		envs = append(envs, cfg.Agent.APIKeyEnv)
	}
	for _, e := range envs {
		if strings.TrimSpace(os.Getenv(e)) != "" {
			return true
		}
	}
	return false
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

// ctx0 returns a background context for boot-time MCP client dialing. We
// can't use the scheduler/signal context because that's constructed AFTER
// the tool registry is wired (the order matters: the LLM Driver factory
// reads the registry through Runner, so tools must be registered first).
func ctx0() context.Context { return context.Background() }

// bootMCPClients converts cfg.MCPClients into mcpclient.Configs, resolves
// AuthEnv → real token, then asks the Manager to dial them. Per-client
// failures are logged but never fatal.
func bootMCPClients(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*mcpclient.Manager, []mcpclient.Started, error) {
	if len(cfg.MCPClients) == 0 {
		// Return an empty Manager so the caller's `defer Close()` is a no-op.
		mgr, started, _ := mcpclient.Boot(ctx, nil, logger)
		return mgr, started, nil
	}
	out := make([]mcpclient.Config, 0, len(cfg.MCPClients))
	for _, mc := range cfg.MCPClients {
		entry := mcpclient.Config{
			Name:      mc.Name,
			Transport: mcpclient.Transport(mc.Transport),
			URL:       mc.URL,
			Command:   mc.Command,
			Args:      append([]string(nil), mc.Args...),
			Env:       mc.Env,
		}
		if mc.AuthEnv != "" {
			entry.AuthToken = os.Getenv(mc.AuthEnv)
			if entry.AuthToken == "" {
				logger.Warn("mcpclient: auth env var is empty",
					"name", mc.Name, "auth_env", mc.AuthEnv)
			}
		}
		out = append(out, entry)
	}
	return mcpclient.Boot(ctx, out, logger)
}

// registerRemoteTools wires each remote server's tools into the local
// tool.Registry under the `{clientName}_{remoteName}` convention.
// Conflict policy: last-wins with a Warn log. We intentionally don't
// abort registration on conflict — an operator who renamed an
// integration without removing the old one should still get a working
// daemon, just with a noisy log.
func registerRemoteTools(reg *tool.Registry, started []mcpclient.Started, logger *slog.Logger) {
	for _, s := range started {
		invoker := &mcpclient.ToolInvoker{C: s.Client}
		for _, td := range s.Tools {
			adapter := tool.NewRemoteToolAdapter(s.Client.Name(), td.Name, td.Description, td.InputSchema, invoker)
			if err := reg.Register(adapter); err != nil {
				logger.Warn("mcpclient: tool register conflict (last-wins not yet enabled — skipping)",
					"local_name", adapter.Name(), "client", s.Client.Name(), "error", err)
				// last-wins: drop then re-register
				// (Registry has no Unregister yet; safe fallback is to skip
				// the new one. We log loudly so the operator sees it.)
				continue
			}
			logger.Info("mcpclient: tool registered",
				"local_name", adapter.Name(), "client", s.Client.Name())
		}
	}
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

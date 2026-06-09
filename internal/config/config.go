// Copyright 2026 Zibby Lab. Apache-2.0.

// Package config loads and validates the daemon's YAML config.
//
// The schema is intentionally compact in v0.1 but namespaces every section
// so v0.x additions (cluster.*, telemetry.*, connectors.*) plug in without
// breaking existing files.
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Config is the top-level shape persisted at /etc/agent-ops/config.yaml
// (or wherever the operator points the daemon).
type Config struct {
	// StateDir is the directory holding node.id, state.db, and other
	// persistent artifacts. Default: /var/lib/agent-ops.
	StateDir string `yaml:"state_dir"`

	// Agent picks the LLM backend + identity injected into prompts.
	Agent AgentConfig `yaml:"agent"`

	// Schedules is the list of cron-fired tasks. Each entry becomes one
	// scheduled job at boot. Manual triggers (via MCP) reference the
	// schedule's Name to reuse its prompt + tools allowlist.
	Schedules []Schedule `yaml:"schedules"`

	// Bootstrap is the prompt+tools run exactly once on first daemon start.
	// Use this to do initial install / setup of the host application.
	Bootstrap *Schedule `yaml:"bootstrap,omitempty"`

	// MCP configures the streamable-HTTP MCP server exposed to remote agents.
	MCP MCPConfig `yaml:"mcp"`

	// MCPClients is the list of OUTBOUND MCP servers the daemon dials at
	// boot (see internal/mcpclient). For each entry the daemon connects,
	// runs tools/list, and registers each discovered tool into the local
	// tool registry under the name `{Name}_{remoteToolName}`. The LLM
	// driving scheduled tasks then sees those remote tools alongside the
	// built-in `shell`. Empty / unset → daemon runs in pure OSS mode with
	// only `shell` available (back-compat with v0.2.x).
	//
	// Each entry is appended-to / removed-from atomically via
	// `agent-ops integrate add | remove` (and the matching MCP tools), so
	// operators don't have to hand-edit YAML to wire up an integration.
	MCPClients []MCPClientConfig `yaml:"mcp_clients,omitempty"`

	// Reserved namespaces — kept here so config files written for v0.1
	// don't reject unknown top-level keys when v0.2 adds these. The fields
	// are intentionally not parsed yet; the loader emits a warning.
	Cluster   map[string]any `yaml:"cluster,omitempty"`
	Telemetry map[string]any `yaml:"telemetry,omitempty"`
	Connectors map[string]any `yaml:"connectors,omitempty"`
}

// AgentConfig picks the LLM provider and how to authenticate.
type AgentConfig struct {
	// Provider is one of: claude, codex, gemini, ollama. v0.1 ships claude only.
	Provider string `yaml:"provider"`

	// Model is provider-specific (e.g. "claude-sonnet-4-6").
	Model string `yaml:"model"`

	// APIKeyEnv names the env var holding the user's API key. Reading from
	// env (vs literal key in YAML) keeps config files diffable + commit-safe.
	APIKeyEnv string `yaml:"api_key_env"`

	// MaxToolCallsPerTask caps how many tool-call iterations a single Task can
	// make. Defensive bound to keep a stuck agent from running forever.
	MaxToolCallsPerTask int `yaml:"max_tool_calls_per_task"`

	// Default timeout for a single Task run. 0 means no daemon-side cap (the
	// LLM API has its own).
	TaskTimeout time.Duration `yaml:"task_timeout"`
}

// Schedule is one cron-fired task definition.
type Schedule struct {
	Name   string   `yaml:"name"`
	Cron   string   `yaml:"cron"` // standard 5-field; "@hourly", "@daily" etc. accepted
	Prompt string   `yaml:"prompt"`
	Tools  []string `yaml:"tools"` // allowlist; empty = all registered tools
	// Model lets a single schedule (or bootstrap) pin a cheaper or beefier
	// model than the daemon-wide default. Empty → use agent.model. Cost
	// lever: route routine checks to Haiku, reserve Sonnet/Opus for
	// install / upgrade / incident-response prompts that need reasoning.
	Model string `yaml:"model,omitempty"`

	// VerifyPrompt enables an independent verifier pass that runs AFTER the
	// main task completes. The verifier is a second LLM invocation (typically
	// a cheaper model — set VerifyModel) whose job is to re-check the main
	// agent's work via shell and emit strict JSON: {"pass": bool, ...}. The
	// caller (bootstrap.MaybeRunFirstRun today) decides what to do with a
	// pass=false result. Empty → no verifier runs. Used to catch the case
	// where the main agent claims "done" but the install actually failed
	// mid-way and the LLM lied about it.
	VerifyPrompt string `yaml:"verify_prompt,omitempty"`

	// VerifyModel overrides the verifier's model. Empty → falls back to the
	// daemon default (agent.model). Pin this to Haiku for cheap re-checks.
	VerifyModel string `yaml:"verify_model,omitempty"`

	Enabled *bool `yaml:"enabled,omitempty"`
}

// MCPClientConfig is one outbound MCP client entry in `mcp_clients:`. The
// daemon dials each entry at boot and exposes the discovered remote tools
// to the local LLM under the prefix `{Name}_`.
//
// Two transports are supported, picked by Transport:
//   - "http":  AuthEnv names an env var holding a Bearer token. URL is the
//     full /mcp endpoint (e.g. https://api-prod.zibby.app/mcp).
//   - "stdio": Command + Args are exec'd; the daemon talks JSON-RPC over
//     stdin/stdout. Env supplies extra environment for the subprocess.
//
// Secrets live in /etc/agent-ops/agent-ops.env (mode 0600), NOT in this
// file — AuthEnv resolves to whatever the operator (or `integrate add`)
// wrote into that env file. This keeps config.yaml diffable + commit-safe.
type MCPClientConfig struct {
	// Name uniquely identifies the integration. Used as the local tool-name
	// prefix and as the key for `integrate remove`.
	Name string `yaml:"name"`

	// Transport selects "http" or "stdio".
	Transport string `yaml:"transport"`

	// URL is the MCP endpoint (transport=http only).
	URL string `yaml:"url,omitempty"`

	// Command is the executable to spawn (transport=stdio only).
	Command string `yaml:"command,omitempty"`

	// Args are the argv after Command (transport=stdio only).
	Args []string `yaml:"args,omitempty"`

	// AuthEnv names the env var holding the Bearer token (http only). The
	// daemon resolves this at boot via os.Getenv; never load a literal
	// token into YAML.
	AuthEnv string `yaml:"auth_env,omitempty"`

	// Env supplies extra environment for the subprocess (stdio only).
	// Layered on top of the daemon's inherited env.
	Env map[string]string `yaml:"env,omitempty"`
}

// MCPConfig drives the agent-ops MCP server.
type MCPConfig struct {
	// ListenAddr e.g. ":7842" or "127.0.0.1:7842".
	ListenAddr string `yaml:"listen_addr"`

	// TokenEnv names the env var holding the bearer token clients must send.
	// On first boot, bootstrap mints a token and writes it to <state>/mcp.token;
	// the operator (or the Zibby control plane) reads it back and configures
	// the client side. The same token is loadable via this env var for
	// non-Zibby deploys.
	TokenEnv string `yaml:"token_env"`
}

// Load reads + validates a config file. Missing optional fields get defaults.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse is the buffer-friendly variant of Load.
func Parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(false) // allow reserved fields without failing
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.StateDir == "" {
		c.StateDir = "/var/lib/agent-ops"
	}
	if c.Agent.Provider == "" {
		c.Agent.Provider = "claude"
	}
	if c.Agent.MaxToolCallsPerTask == 0 {
		c.Agent.MaxToolCallsPerTask = 25
	}
	if c.Agent.TaskTimeout == 0 {
		// Bumped from 10m → 15m in 0.3.6: bootstrap (the dominant per-task
		// cost on Managed Apps) routinely hits 10-12 minutes on slow npm
		// CDN days; the scriptBootstrapTimeout 2x heuristic on top of this
		// gives 30 min wall-clock for script-mode installs, matching the
		// new agent-mode bootstrap default for goal-mode installs.
		c.Agent.TaskTimeout = 15 * time.Minute
	}
	if c.MCP.ListenAddr == "" {
		c.MCP.ListenAddr = ":7842"
	}
	if c.MCP.TokenEnv == "" {
		c.MCP.TokenEnv = "AGENT_OPS_TOKEN"
	}

	// AGENT_OPS_PROVIDER env override. Per-instance billing axis: an
	// operator running a tenant container can pick "claude-cli" (OAuth /
	// subscription billing) without re-baking the image. Mirrors the same
	// pattern as AGENT_OPS_BOOTSTRAP_PROMPT below — env wins over baked
	// config so per-tenant config.yaml mounts aren't required.
	if p := strings.TrimSpace(os.Getenv("AGENT_OPS_PROVIDER")); p != "" {
		c.Agent.Provider = p
	}

	// AGENT_OPS_BOOTSTRAP_PROMPT env override. The Zibby control plane sets
	// this on the Fargate task at RunTask time so the per-instance goal
	// ("install n8n on port 5678") ships as an env var instead of needing a
	// custom config.yaml per task. If config.yaml already has a bootstrap
	// section the env value wins; if it doesn't, we synthesize one with
	// sensible defaults (shell tool only — agent picks what it needs).
	if p := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_PROMPT")); p != "" {
		if c.Bootstrap == nil {
			c.Bootstrap = &Schedule{
				Name:  "bootstrap",
				Cron:  "@yearly",
				Tools: []string{"shell"},
			}
		}
		c.Bootstrap.Prompt = p
	}

	// AGENT_OPS_BOOTSTRAP_MODEL env override. Per-deploy model pin from the
	// Zibby control plane (CLI --model / MCP model field / POST /apps body
	// .model). Replaces cfg.Bootstrap.Model so the same scheduler path
	// that already honors per-task model overrides (scheduler.go's
	// modelOverrides map, populated from cfg.Bootstrap.Model in
	// Hydrate) picks up the new value. Synthesized Bootstrap when none
	// exists, mirroring the BOOTSTRAP_PROMPT pattern above. Logged at
	// load so operators can grep CloudWatch for the swap.
	if m := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_MODEL")); m != "" {
		if c.Bootstrap == nil {
			c.Bootstrap = &Schedule{
				Name:  "bootstrap",
				Cron:  "@yearly",
				Tools: []string{"shell"},
			}
		}
		prev := c.Bootstrap.Model
		c.Bootstrap.Model = m
		slog.Info("bootstrap: model override from env",
			"from_config", prev, "from_env", m)
	}

	// AGENT_OPS_BOOTSTRAP_MAX_TURNS env override. Per-deploy turn cap from
	// the Zibby control plane (CLI --max-turns / MCP maxTurns / POST /apps
	// body.maxTurns). Replaces cfg.Agent.MaxToolCallsPerTask so the
	// runner (daemon.go:109) picks it up before the first task fires.
	// Heavy goal-mode installs (n8n, OpenHands) exhaust the 25-default
	// before finishing — bumping to 60-100 lets them complete. Logged
	// at load so operators can grep CloudWatch for the swap.
	//
	// Parse failures: invalid integer / non-positive → ignored (keeps
	// the baked default; logged at WARN so a misconfigured task def
	// surfaces in logs but doesn't fail the daemon boot).
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_MAX_TURNS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			prev := c.Agent.MaxToolCallsPerTask
			c.Agent.MaxToolCallsPerTask = n
			slog.Info("bootstrap: max_tool_calls_per_task override from env",
				"from_config", prev, "from_env", n)
		} else {
			slog.Warn("bootstrap: AGENT_OPS_BOOTSTRAP_MAX_TURNS ignored — not a positive integer",
				"raw", v)
		}
	}

	// AGENT_OPS_BOOTSTRAP_TIMEOUT_MS env override. Per-deploy wall-clock
	// cap (in milliseconds) from the Zibby control plane (CLI
	// --timeout-min / MCP timeoutMin / POST /apps body.timeoutMin —
	// minutes * 60 * 1000 on the wire). Replaces cfg.Agent.TaskTimeout
	// so both the runner (agent-mode bootstrap) AND scriptBootstrapTimeout
	// (script-mode bootstrap = 2x this on the heuristic fallback) pick
	// up the new wall-clock budget.
	//
	// Heavy installs that npm-install ~500MB or compile native deps need
	// 30-45 min instead of the 20-min baked default. Same parse-failure
	// policy as MAX_TURNS above: invalid → ignored + WARN.
	//
	// Note: AGENT_OPS_BOOTSTRAP_TIMEOUT (Go duration string, e.g. "30m")
	// is still honored by scriptBootstrapTimeout for direct operator
	// override. The MS variant is what the Zibby control plane emits.
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_TIMEOUT_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			prev := c.Agent.TaskTimeout
			c.Agent.TaskTimeout = time.Duration(n) * time.Millisecond
			slog.Info("bootstrap: task_timeout override from env",
				"from_config", prev, "from_env_ms", n, "from_env", c.Agent.TaskTimeout)
		} else {
			slog.Warn("bootstrap: AGENT_OPS_BOOTSTRAP_TIMEOUT_MS ignored — not a positive integer",
				"raw", v)
		}
	}

	// AGENT_OPS_HEALTH_CHECK_PROMPT env override. Same pattern as the
	// bootstrap override above: the control plane injects a fully-formed,
	// per-app concrete prompt ("Verify n8n on port 5678…") at deploy time
	// without re-baking config.yaml. Only replaces the prompt for an
	// EXISTING `hourly_health_check` schedule — we don't synthesize a new
	// schedule from env alone, because the cron expression + tool allowlist
	// belong in the baked config where the operator can review them.
	if p := strings.TrimSpace(os.Getenv("AGENT_OPS_HEALTH_CHECK_PROMPT")); p != "" {
		for i := range c.Schedules {
			if c.Schedules[i].Name == "hourly_health_check" {
				c.Schedules[i].Prompt = p
				break
			}
		}
	}
}

var validProviders = map[string]bool{
	"claude":     true, // direct REST API via x-api-key (separate API billing)
	"claude-cli": true, // subprocess `claude` binary, reads CLAUDE_CODE_OAUTH_TOKEN
	"codex":      true,
	"gemini":     true,
	"ollama":     true,
	// "none" = NO LLM agent. The daemon runs the front-proxy, health checks,
	// register-port and LLM-free (script-mode) installs, but never builds an
	// LLM driver and idles the scheduler (gated off via
	// AGENT_OPS_SCHEDULER_ENABLED=false). Used by Zibby "deploy without an
	// agent" — the app runs, there's just no AI agent and no credential.
	"none": true,
}

func (c *Config) validate() error {
	if !validProviders[c.Agent.Provider] {
		return fmt.Errorf("config: agent.provider %q is not one of none|claude|codex|gemini|ollama", c.Agent.Provider)
	}
	if c.Agent.Provider == "claude" && c.Agent.Model == "" {
		return errors.New("config: agent.model is required when provider=claude")
	}
	// claude-cli authenticates via CLAUDE_CODE_OAUTH_TOKEN env (read by the
	// CLI binary itself), so agent.api_key_env is not meaningful for it.
	// ollama is fully local. "none" runs no LLM at all. All other cloud
	// providers need an api_key_env.
	if c.Agent.APIKeyEnv == "" && c.Agent.Provider != "ollama" &&
		c.Agent.Provider != "claude-cli" && c.Agent.Provider != "none" {
		return errors.New("config: agent.api_key_env is required for cloud providers")
	}
	if c.Agent.MaxToolCallsPerTask < 1 {
		return errors.New("config: agent.max_tool_calls_per_task must be >= 1")
	}

	names := make(map[string]struct{}, len(c.Schedules))
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	for i, s := range c.Schedules {
		if s.Name == "" {
			return fmt.Errorf("config: schedules[%d].name is required", i)
		}
		if _, dup := names[s.Name]; dup {
			return fmt.Errorf("config: schedules[%d].name %q is a duplicate", i, s.Name)
		}
		names[s.Name] = struct{}{}
		if s.Cron == "" {
			return fmt.Errorf("config: schedules[%d].cron is required", i)
		}
		if _, err := parser.Parse(s.Cron); err != nil {
			return fmt.Errorf("config: schedules[%d].cron %q: %w", i, s.Cron, err)
		}
		if strings.TrimSpace(s.Prompt) == "" {
			return fmt.Errorf("config: schedules[%d].prompt is required", i)
		}
	}

	if c.Bootstrap != nil {
		if c.Bootstrap.Name == "" {
			c.Bootstrap.Name = "bootstrap"
		}
		if strings.TrimSpace(c.Bootstrap.Prompt) == "" {
			return errors.New("config: bootstrap.prompt is required")
		}
	}

	// MCPClients validation — defence against typo'd transport or missing
	// transport-required fields. We do NOT validate that AuthEnv resolves
	// at parse time, because env may legitimately be loaded after the
	// daemon process starts (systemd EnvironmentFile=…).
	clientNames := map[string]struct{}{}
	for i, mc := range c.MCPClients {
		if strings.TrimSpace(mc.Name) == "" {
			return fmt.Errorf("config: mcp_clients[%d].name is required", i)
		}
		if _, dup := clientNames[mc.Name]; dup {
			return fmt.Errorf("config: mcp_clients[%d].name %q is a duplicate", i, mc.Name)
		}
		clientNames[mc.Name] = struct{}{}
		switch mc.Transport {
		case "http":
			if strings.TrimSpace(mc.URL) == "" {
				return fmt.Errorf("config: mcp_clients[%d] (%s): http transport requires url", i, mc.Name)
			}
		case "stdio":
			if strings.TrimSpace(mc.Command) == "" {
				return fmt.Errorf("config: mcp_clients[%d] (%s): stdio transport requires command", i, mc.Name)
			}
		default:
			return fmt.Errorf("config: mcp_clients[%d] (%s): transport %q is not one of http|stdio", i, mc.Name, mc.Transport)
		}
	}
	return nil
}

// SchedulesEnabled returns the subset of Schedules whose Enabled flag is unset
// (default) or true. Convenience for the scheduler boot path.
func (c *Config) SchedulesEnabled() []Schedule {
	out := make([]Schedule, 0, len(c.Schedules))
	for _, s := range c.Schedules {
		if s.Enabled == nil || *s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

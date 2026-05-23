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
	"os"
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
	Tools  []string `yaml:"tools"`  // allowlist; empty = all registered tools
	Enabled *bool   `yaml:"enabled,omitempty"`
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
		c.Agent.TaskTimeout = 10 * time.Minute
	}
	if c.MCP.ListenAddr == "" {
		c.MCP.ListenAddr = ":7842"
	}
	if c.MCP.TokenEnv == "" {
		c.MCP.TokenEnv = "AGENT_OPS_TOKEN"
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
}

var validProviders = map[string]bool{
	"claude":     true, // direct REST API via x-api-key (separate API billing)
	"claude-cli": true, // subprocess `claude` binary, reads CLAUDE_CODE_OAUTH_TOKEN
	"codex":      true,
	"gemini":     true,
	"ollama":     true,
}

func (c *Config) validate() error {
	if !validProviders[c.Agent.Provider] {
		return fmt.Errorf("config: agent.provider %q is not one of claude|codex|gemini|ollama", c.Agent.Provider)
	}
	if c.Agent.Provider == "claude" && c.Agent.Model == "" {
		return errors.New("config: agent.model is required when provider=claude")
	}
	// claude-cli authenticates via CLAUDE_CODE_OAUTH_TOKEN env (read by the
	// CLI binary itself), so agent.api_key_env is not meaningful for it.
	// ollama is fully local. All other cloud providers need an api_key_env.
	if c.Agent.APIKeyEnv == "" && c.Agent.Provider != "ollama" && c.Agent.Provider != "claude-cli" {
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

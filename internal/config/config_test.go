package config

import (
	"strings"
	"testing"
)

const validYAML = `
state_dir: /tmp/ao
agent:
  provider: claude
  model: claude-sonnet-4-6
  api_key_env: ANTHROPIC_API_KEY
schedules:
  - name: weekly_upgrade
    cron: "0 9 * * 1"
    prompt: "Check for upstream updates"
    tools: [shell, http]
mcp:
  listen_addr: ":7842"
  token_env: AGENT_OPS_TOKEN
`

func TestParse_HappyPath(t *testing.T) {
	c, err := Parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.StateDir != "/tmp/ao" {
		t.Errorf("StateDir = %q", c.StateDir)
	}
	if c.Agent.Provider != "claude" {
		t.Errorf("Agent.Provider = %q", c.Agent.Provider)
	}
	if c.Agent.MaxToolCallsPerTask != 25 {
		t.Errorf("default max_tool_calls_per_task not applied: %d", c.Agent.MaxToolCallsPerTask)
	}
	if len(c.Schedules) != 1 || c.Schedules[0].Name != "weekly_upgrade" {
		t.Errorf("schedule parse mismatch: %+v", c.Schedules)
	}
}

func TestParse_RejectsBadProvider(t *testing.T) {
	bad := strings.Replace(validYAML, "provider: claude", "provider: openai", 1)
	if _, err := Parse(strings.NewReader(bad)); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestParse_RejectsBadCron(t *testing.T) {
	bad := strings.Replace(validYAML, "0 9 * * 1", "not a cron", 1)
	if _, err := Parse(strings.NewReader(bad)); err == nil {
		t.Fatal("expected error for malformed cron")
	}
}

func TestParse_RejectsDuplicateScheduleNames(t *testing.T) {
	yaml := `
agent:
  provider: claude
  model: c
  api_key_env: K
schedules:
  - name: dup
    cron: "@hourly"
    prompt: "a"
  - name: dup
    cron: "@daily"
    prompt: "b"
`
	if _, err := Parse(strings.NewReader(yaml)); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestSchedulesEnabled_DefaultsToEnabled(t *testing.T) {
	c, _ := Parse(strings.NewReader(validYAML))
	if len(c.SchedulesEnabled()) != 1 {
		t.Fatalf("expected 1 enabled schedule, got %d", len(c.SchedulesEnabled()))
	}
}

func TestSchedulesEnabled_ExplicitFalse(t *testing.T) {
	yaml := `
agent:
  provider: claude
  model: c
  api_key_env: K
schedules:
  - name: on
    cron: "@hourly"
    prompt: "a"
  - name: off
    cron: "@hourly"
    prompt: "b"
    enabled: false
`
	c, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SchedulesEnabled()) != 1 {
		t.Fatalf("expected 1 enabled schedule, got %d", len(c.SchedulesEnabled()))
	}
}

func TestParse_BootstrapPromptEnvOverride(t *testing.T) {
	// AGENT_OPS_BOOTSTRAP_PROMPT is how the Zibby control plane injects a
	// per-instance goal into a Fargate task without baking a new config.yaml
	// per task. Two paths: synthesize a Bootstrap when config has none, and
	// override an existing Bootstrap.Prompt when it does.
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install n8n on port 5678")

	t.Run("synthesizes Bootstrap when config has none", func(t *testing.T) {
		c, err := Parse(strings.NewReader(validYAML))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if c.Bootstrap == nil {
			t.Fatal("Bootstrap should be synthesized from env var")
		}
		if c.Bootstrap.Prompt != "install n8n on port 5678" {
			t.Errorf("Bootstrap.Prompt = %q", c.Bootstrap.Prompt)
		}
		if c.Bootstrap.Name != "bootstrap" {
			t.Errorf("Bootstrap.Name = %q, want default 'bootstrap'", c.Bootstrap.Name)
		}
	})

	t.Run("overrides existing Bootstrap.Prompt", func(t *testing.T) {
		yaml := validYAML + `
bootstrap:
  name: initial_setup
  prompt: "this should get overridden"
  tools: [shell]
`
		c, err := Parse(strings.NewReader(yaml))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if c.Bootstrap.Prompt != "install n8n on port 5678" {
			t.Errorf("env should win, got %q", c.Bootstrap.Prompt)
		}
		if c.Bootstrap.Name != "initial_setup" {
			t.Errorf("existing Name should be preserved, got %q", c.Bootstrap.Name)
		}
	})
}

func TestParse_BootstrapModelOverrideParses(t *testing.T) {
	// Per-task `model:` overrides agent.model so the operator can route
	// install/upgrade tasks (which need reasoning) to Sonnet while leaving
	// routine cron on a cheaper default. This test pins the YAML-tag wiring
	// so a refactor of the Schedule struct can't silently drop the field.
	yaml := validYAML + `
bootstrap:
  name: initial_setup
  prompt: "install the thing"
  model: claude-sonnet-4-6
  tools: [shell]
`
	c, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Bootstrap == nil {
		t.Fatal("Bootstrap missing")
	}
	if c.Bootstrap.Model != "claude-sonnet-4-6" {
		t.Errorf("Bootstrap.Model = %q, want claude-sonnet-4-6", c.Bootstrap.Model)
	}
}

func TestParse_ScheduleModelOverrideParses(t *testing.T) {
	// Same wiring check for the recurring-schedule path: per-schedule model
	// overrides the daemon-wide agent.model default.
	yaml := `
agent:
  provider: claude
  model: claude-haiku-4-5-20251001
  api_key_env: K
schedules:
  - name: weekly_upgrade
    cron: "0 9 * * 1"
    prompt: "upgrade"
    model: claude-sonnet-4-6
    tools: [shell]
  - name: hourly_health
    cron: "@hourly"
    prompt: "check"
    tools: [shell]
`
	c, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Schedules) != 2 {
		t.Fatalf("want 2 schedules, got %d", len(c.Schedules))
	}
	if c.Schedules[0].Model != "claude-sonnet-4-6" {
		t.Errorf("weekly_upgrade.Model = %q", c.Schedules[0].Model)
	}
	if c.Schedules[1].Model != "" {
		t.Errorf("hourly_health.Model should be empty (inherits agent.model), got %q", c.Schedules[1].Model)
	}
}

// TestParse_HealthCheckPromptEnvOverride_Unset pins the backward-compat path:
// when AGENT_OPS_HEALTH_CHECK_PROMPT is empty, the baked config.yaml prompt
// for hourly_health_check survives untouched. Required so existing deploys
// without the new env var see zero behavior change.
func TestParse_HealthCheckPromptEnvOverride_Unset(t *testing.T) {
	t.Setenv("AGENT_OPS_HEALTH_CHECK_PROMPT", "")
	yaml := `
agent:
  provider: claude
  model: c
  api_key_env: K
schedules:
  - name: hourly_health_check
    cron: "@hourly"
    prompt: "baked check prompt"
    tools: [shell]
`
	c, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Schedules) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(c.Schedules))
	}
	if c.Schedules[0].Prompt != "baked check prompt" {
		t.Errorf("Prompt = %q, want unchanged baked prompt", c.Schedules[0].Prompt)
	}
}

// TestParse_HealthCheckPromptEnvOverride_Set pins the override path: when
// AGENT_OPS_HEALTH_CHECK_PROMPT is non-empty, the baked prompt for the
// hourly_health_check schedule is replaced with the env value. This is the
// hook the control plane uses to ship a per-app concrete prompt ("Verify
// n8n on port 5678…") without re-baking config.yaml.
func TestParse_HealthCheckPromptEnvOverride_Set(t *testing.T) {
	t.Setenv("AGENT_OPS_HEALTH_CHECK_PROMPT", "Verify n8n on port 5678 is responding.")
	yaml := `
agent:
  provider: claude
  model: c
  api_key_env: K
schedules:
  - name: hourly_health_check
    cron: "@hourly"
    prompt: "baked check prompt"
    tools: [shell]
  - name: weekly_upgrade
    cron: "@weekly"
    prompt: "upgrade prompt"
    tools: [shell]
`
	c, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Schedules) != 2 {
		t.Fatalf("want 2 schedules, got %d", len(c.Schedules))
	}
	if c.Schedules[0].Prompt != "Verify n8n on port 5678 is responding." {
		t.Errorf("hourly_health_check.Prompt = %q, want env override", c.Schedules[0].Prompt)
	}
	// weekly_upgrade must be left alone — env only targets hourly_health_check.
	if c.Schedules[1].Prompt != "upgrade prompt" {
		t.Errorf("weekly_upgrade.Prompt = %q, want untouched", c.Schedules[1].Prompt)
	}
}

// TestParse_BootstrapModelEnvOverride pins the per-deploy model override
// path: when AGENT_OPS_BOOTSTRAP_MODEL is non-empty, it wins over the
// baked config.yaml `bootstrap.model:` value. The Zibby control plane
// sets this on Fargate task definitions so a per-deploy `--model` flag
// (CLI / MCP / POST /apps body field) can re-route the agent-ops
// bootstrap to a different Claude model without re-baking config.
//
// Table-driven: empty env → config value survives; non-empty env wins;
// env synthesizes a Bootstrap when config has none.
func TestParse_BootstrapModelEnvOverride(t *testing.T) {
	cases := []struct {
		name      string
		envValue  string
		yaml      string
		wantModel string
		wantNil   bool // true → expect c.Bootstrap == nil
	}{
		{
			name:     "empty env: config value used (back-compat)",
			envValue: "",
			yaml: validYAML + `
bootstrap:
  name: bootstrap
  prompt: "install thing"
  model: claude-haiku-4-5-20251001
`,
			wantModel: "claude-haiku-4-5-20251001",
		},
		{
			name:     "non-empty env: wins over config",
			envValue: "claude-opus-4-5",
			yaml: validYAML + `
bootstrap:
  name: bootstrap
  prompt: "install thing"
  model: claude-haiku-4-5-20251001
`,
			wantModel: "claude-opus-4-5",
		},
		{
			name:     "non-empty env, no config bootstrap: synthesizes one",
			envValue: "claude-sonnet-4-6",
			yaml:     validYAML, // no `bootstrap:` block at all
			// PROMPT env is unset in this test, so without a prompt the
			// synthesized Bootstrap would fail validation ("bootstrap.prompt is
			// required"). We provide the prompt here so the synthesis path is
			// reachable.
			wantModel: "claude-sonnet-4-6",
		},
		{
			name:     "empty env, no config bootstrap: stays nil",
			envValue: "",
			yaml:     validYAML,
			wantNil:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_OPS_BOOTSTRAP_MODEL", tc.envValue)
			// The synthesis path also needs AGENT_OPS_BOOTSTRAP_PROMPT to
			// satisfy validate()'s prompt-required check when config has no
			// bootstrap block. Set it for that subcase.
			if tc.envValue != "" && !strings.Contains(tc.yaml, "bootstrap:") {
				t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "synthesized prompt for test")
			}
			c, err := Parse(strings.NewReader(tc.yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tc.wantNil {
				if c.Bootstrap != nil {
					t.Fatalf("Bootstrap = %+v, want nil", c.Bootstrap)
				}
				return
			}
			if c.Bootstrap == nil {
				t.Fatal("Bootstrap is nil, want a value")
			}
			if c.Bootstrap.Model != tc.wantModel {
				t.Errorf("Bootstrap.Model = %q, want %q", c.Bootstrap.Model, tc.wantModel)
			}
		})
	}
}

func TestParse_AllowsReservedTopLevelKeys(t *testing.T) {
	// Future v0.x features should not break v0.1 config files.
	yaml := validYAML + `
cluster:
  raft_peers: ["a", "b"]
telemetry:
  otel_endpoint: "http://collector:4317"
connectors:
  zibby:
    url: "https://api.zibby.app"
`
	c, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("reserved keys should not fail: %v", err)
	}
	if len(c.Cluster) == 0 {
		t.Error("expected reserved Cluster section to parse")
	}
}

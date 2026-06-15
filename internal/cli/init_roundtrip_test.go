// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"strings"
	"testing"

	"github.com/ZibbyDev/agent-ops/internal/config"
)

// TestRenderConfigYAML_RoundTrip pins the contract between
// `agent-ops init` (which emits YAML) and `config.Load` (which parses it).
// The wizard MUST always produce a file that the daemon will accept; a
// regression here means a fresh init leaves the user with a broken daemon.
func TestRenderConfigYAML_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		ans  initAnswers
	}{
		{
			name: "claude-cli defaults",
			ans: initAnswers{
				Provider:  "claude-cli",
				APIKeyEnv: "CLAUDE_CODE_OAUTH_TOKEN",
				Model:     "claude-sonnet-4-6",
				StateDir:  "/var/lib/agent-ops",
			},
		},
		{
			name: "claude api with bootstrap goal",
			ans: initAnswers{
				Provider:        "claude",
				APIKeyEnv:       "ANTHROPIC_API_KEY",
				Model:           "claude-sonnet-4-6",
				StateDir:        "/tmp/ao-state",
				BootstrapPrompt: "Install n8n on port 5678 and verify it responds.",
				NotifyWorkflow:  "wf-notify-down",
			},
		},
		{
			name: "codex",
			ans: initAnswers{
				Provider:  "codex",
				APIKeyEnv: "OPENAI_API_KEY",
				Model:     "gpt-5-codex",
				StateDir:  "/var/lib/agent-ops",
			},
		},
		{
			// macOS default state dir has a space ("Application Support"). YAML
			// accepts unquoted scalars with embedded spaces in flow-out form,
			// but trailing-comment combinations have bitten us before — pin
			// the round trip explicitly.
			name: "darwin default state dir with spaces",
			ans: initAnswers{
				Provider:  "claude-cli",
				APIKeyEnv: "CLAUDE_CODE_OAUTH_TOKEN",
				Model:     "claude-sonnet-4-6",
				StateDir:  "/Users/test/Library/Application Support/agent-ops",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			yaml := renderConfigYAML(c.ans)
			cfg, err := config.Parse(strings.NewReader(yaml))
			if err != nil {
				t.Fatalf("init-emitted YAML failed to re-parse: %v\n--- YAML ---\n%s", err, yaml)
			}
			if cfg.Agent.Provider != c.ans.Provider {
				t.Errorf("provider drift: got %q, want %q", cfg.Agent.Provider, c.ans.Provider)
			}
			if cfg.Agent.Model != c.ans.Model {
				t.Errorf("model drift: got %q, want %q", cfg.Agent.Model, c.ans.Model)
			}
			if cfg.Agent.APIKeyEnv != c.ans.APIKeyEnv {
				t.Errorf("api_key_env drift: got %q, want %q", cfg.Agent.APIKeyEnv, c.ans.APIKeyEnv)
			}
			if cfg.StateDir != c.ans.StateDir {
				t.Errorf("state_dir drift: got %q, want %q", cfg.StateDir, c.ans.StateDir)
			}
			if len(cfg.Schedules) == 0 {
				t.Errorf("expected at least one schedule emitted, got 0")
			}
			if c.ans.BootstrapPrompt != "" && cfg.Bootstrap == nil {
				t.Errorf("BootstrapPrompt was set but cfg.Bootstrap is nil")
			}
		})
	}
}

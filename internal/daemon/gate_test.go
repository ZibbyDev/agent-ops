package daemon

import (
	"testing"

	"github.com/ZibbyDev/agent-ops/internal/config"
)

// schedulerGate must idle the autonomous scheduler when the app has agent-ops
// turned off OR when there's no Claude token — without ever blocking the
// daemon from coming up. These tests pin that contract.
func TestSchedulerGate(t *testing.T) {
	cfg := &config.Config{}

	t.Run("idles when AGENT_OPS_SCHEDULER_ENABLED is falsey", func(t *testing.T) {
		for _, v := range []string{"false", "0", "off", "no", "disabled", "FALSE", "Off"} {
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok") // token present, so only the flag matters
			t.Setenv("AGENT_OPS_SCHEDULER_ENABLED", v)
			if on, reason := schedulerGate(cfg); on {
				t.Fatalf("expected idle for AGENT_OPS_SCHEDULER_ENABLED=%q, got run (reason=%q)", v, reason)
			}
		}
	})

	t.Run("idles when no Claude token is present", func(t *testing.T) {
		t.Setenv("AGENT_OPS_SCHEDULER_ENABLED", "")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
		t.Setenv("ANTHROPIC_API_KEY", "")
		if on, reason := schedulerGate(cfg); on {
			t.Fatalf("expected idle with no token, got run (reason=%q)", reason)
		}
	})

	t.Run("runs when enabled (unset) and an OAuth token is present", func(t *testing.T) {
		t.Setenv("AGENT_OPS_SCHEDULER_ENABLED", "")
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-oauth-xyz")
		if on, reason := schedulerGate(cfg); !on {
			t.Fatalf("expected run with token + flag unset, got idle (reason=%q)", reason)
		}
	})

	t.Run("runs when explicitly enabled with a token", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-oauth-xyz")
		t.Setenv("AGENT_OPS_SCHEDULER_ENABLED", "true")
		if on, _ := schedulerGate(cfg); !on {
			t.Fatal("expected run when explicitly enabled with a token")
		}
	})

	t.Run("flag-off beats token-present (explicit pause wins)", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-oauth-xyz")
		t.Setenv("AGENT_OPS_SCHEDULER_ENABLED", "false")
		if on, _ := schedulerGate(cfg); on {
			t.Fatal("expected idle: an explicit off must win even when a token is present")
		}
	})

	t.Run("honours the config-named api key env", func(t *testing.T) {
		t.Setenv("AGENT_OPS_SCHEDULER_ENABLED", "")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("MY_CUSTOM_KEY", "v")
		c := &config.Config{}
		c.Agent.APIKeyEnv = "MY_CUSTOM_KEY"
		if on, reason := schedulerGate(c); !on {
			t.Fatalf("expected run when the config-named key env is set, got idle (reason=%q)", reason)
		}
	})
}

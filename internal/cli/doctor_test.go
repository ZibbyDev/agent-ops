// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// doctorFixture writes a minimal config.yaml + an isolated state_dir +
// returns the path to the config. It also sets PATH so binary lookups are
// deterministic — call sites pass `withBinary=...` to drop a stub binary
// into the temp dir before doctor runs.
//
// Why isolate PATH? `exec.LookPath` consults $PATH; if the test runner
// happens to have `claude` or `codex` actually installed (which a Zibby
// dev box probably does — see managed-apps-debian-base memory) the doctor
// "claude NOT on PATH" assertion would silently flip to pass and we'd ship
// a regression. Setting PATH=<temp> on entry guarantees the binary checks
// see only what the test put there.
func doctorFixture(t *testing.T, provider, model, apiKeyEnv string, withBinary string) (cfgPath string, restore func()) {
	t.Helper()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	cfgPath = filepath.Join(tmp, "config.yaml")

	apiKeyLine := ""
	if apiKeyEnv != "" {
		apiKeyLine = "  api_key_env: " + apiKeyEnv + "\n"
	}
	body := "" +
		"state_dir: " + stateDir + "\n" +
		"agent:\n" +
		"  provider: " + provider + "\n" +
		"  model: " + model + "\n" +
		apiKeyLine +
		"  max_tool_calls_per_task: 25\n" +
		"  task_timeout: 10m\n" +
		"schedules:\n" +
		"  - name: ping\n" +
		"    cron: '@hourly'\n" +
		"    prompt: 'ping'\n" +
		"mcp:\n" +
		// Use port 0 so we never collide with the real MCP daemon if one
		// happens to be running on the dev box.
		"  listen_addr: 127.0.0.1:0\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	// Isolate PATH. On windows exec.LookPath behaves slightly differently
	// (needs .exe / .bat); the doctor tests run linux/darwin only.
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	if withBinary != "" {
		// A 1-byte executable file is enough — exec.LookPath only checks
		// existence + mode bits, not contents.
		stub := filepath.Join(binDir, withBinary)
		if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write stub: %v", err)
		}
	}
	prevPath, hadPath := os.LookupEnv("PATH")
	if err := os.Setenv("PATH", binDir); err != nil {
		t.Fatalf("set PATH: %v", err)
	}

	restore = func() {
		if hadPath {
			_ = os.Setenv("PATH", prevPath)
		} else {
			_ = os.Unsetenv("PATH")
		}
	}
	return cfgPath, restore
}

// unsetEnv temporarily clears an env var, restoring its prior value on
// cleanup. Safer than os.Unsetenv + manual restore in every test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		}
	})
}

func setEnv(t *testing.T, key, val string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Setenv(key, val); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// TestDoctor_ClaudeCLI_HappyPath — provider=claude-cli with both
// CLAUDE_CODE_OAUTH_TOKEN set and `claude` on PATH should report zero
// auth/binary failures.
func TestDoctor_ClaudeCLI_HappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on windows")
	}
	cfgPath, restore := doctorFixture(t, "claude-cli", "claude-haiku-4-5-20251001", "CLAUDE_CODE_OAUTH_TOKEN", "claude")
	defer restore()
	setEnv(t, "CLAUDE_CODE_OAUTH_TOKEN", "stub-token-value-1234567890")
	// Ensure stale ANTHROPIC_API_KEY in the dev shell doesn't make the test
	// pass for the wrong reason.
	unsetEnv(t, "ANTHROPIC_API_KEY")

	out := &bytes.Buffer{}
	fails := runDoctor(context.Background(), out, cfgPath)
	body := out.String()

	if strings.Contains(body, "ANTHROPIC_API_KEY") {
		t.Errorf("claude-cli doctor must not mention ANTHROPIC_API_KEY. Got:\n%s", body)
	}
	if !strings.Contains(body, "[ok]   env CLAUDE_CODE_OAUTH_TOKEN is set") {
		t.Errorf("expected CLAUDE_CODE_OAUTH_TOKEN [ok] line. Got:\n%s", body)
	}
	if !strings.Contains(body, "[ok]   claude on PATH") {
		t.Errorf("expected claude [ok] PATH line. Got:\n%s", body)
	}
	// Auth + binary should both be pass — overall failures from those two
	// checks must be zero. (State dir / network may still warn but those
	// are not [fail] in a tmpdir host.)
	for _, badLine := range []string{
		"[fail] env CLAUDE_CODE_OAUTH_TOKEN",
		"[fail] claude NOT on PATH",
	} {
		if strings.Contains(body, badLine) {
			t.Errorf("unexpected failure line %q. Got:\n%s", badLine, body)
		}
	}
	if !strings.Contains(body, "[summary]") {
		t.Errorf("expected summary line. Got:\n%s", body)
	}
	_ = fails
}

// TestDoctor_ClaudeCLI_BinaryMissing — provider=claude-cli + token set but
// no `claude` on PATH must surface BOTH the [fail] line AND the exact
// npm install hint so the operator/agent has a copy-pasteable remediation.
func TestDoctor_ClaudeCLI_BinaryMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on windows")
	}
	cfgPath, restore := doctorFixture(t, "claude-cli", "claude-haiku-4-5-20251001", "CLAUDE_CODE_OAUTH_TOKEN", "")
	defer restore()
	setEnv(t, "CLAUDE_CODE_OAUTH_TOKEN", "stub-token-value-1234567890")

	out := &bytes.Buffer{}
	fails := runDoctor(context.Background(), out, cfgPath)
	body := out.String()

	if !strings.Contains(body, "[fail] claude NOT on PATH") {
		t.Errorf("expected [fail] claude NOT on PATH line. Got:\n%s", body)
	}
	if !strings.Contains(body, "sudo npm install -g @anthropic-ai/claude-code") {
		t.Errorf("expected npm install hint. Got:\n%s", body)
	}
	if !strings.Contains(body, "Node.js 20+") {
		t.Errorf("expected Node.js version hint. Got:\n%s", body)
	}
	if !strings.Contains(body, "deb.nodesource.com/setup_20.x") {
		t.Errorf("expected NodeSource one-liner. Got:\n%s", body)
	}
	if fails < 1 {
		t.Errorf("expected at least 1 failure; got %d. Body:\n%s", fails, body)
	}
}

// TestDoctor_ClaudeCLI_TokenMissing — provider=claude-cli + token UNSET
// must [fail] with a CLAUDE_CODE_OAUTH_TOKEN-specific message, and must NOT
// mention ANTHROPIC_API_KEY anywhere (the bug we're fixing).
func TestDoctor_ClaudeCLI_TokenMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on windows")
	}
	cfgPath, restore := doctorFixture(t, "claude-cli", "claude-haiku-4-5-20251001", "CLAUDE_CODE_OAUTH_TOKEN", "claude")
	defer restore()
	unsetEnv(t, "CLAUDE_CODE_OAUTH_TOKEN")
	unsetEnv(t, "ANTHROPIC_API_KEY")

	out := &bytes.Buffer{}
	fails := runDoctor(context.Background(), out, cfgPath)
	body := out.String()

	if !strings.Contains(body, "[fail] env CLAUDE_CODE_OAUTH_TOKEN is unset") {
		t.Errorf("expected [fail] CLAUDE_CODE_OAUTH_TOKEN unset line. Got:\n%s", body)
	}
	if strings.Contains(body, "ANTHROPIC_API_KEY") {
		t.Errorf("claude-cli must NOT mention ANTHROPIC_API_KEY. Got:\n%s", body)
	}
	if fails < 1 {
		t.Errorf("expected at least 1 failure; got %d. Body:\n%s", fails, body)
	}
}

// TestDoctor_Codex_MissingBoth — provider=codex + OPENAI_API_KEY unset +
// codex NOT on PATH should produce TWO failures, both pointing the
// operator at the codex-specific npm install command + Node 22+ hint.
func TestDoctor_Codex_MissingBoth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on windows")
	}
	cfgPath, restore := doctorFixture(t, "codex", "o4", "OPENAI_API_KEY", "")
	defer restore()
	unsetEnv(t, "OPENAI_API_KEY")
	unsetEnv(t, "ANTHROPIC_API_KEY")

	out := &bytes.Buffer{}
	fails := runDoctor(context.Background(), out, cfgPath)
	body := out.String()

	if !strings.Contains(body, "[fail] env OPENAI_API_KEY is unset") {
		t.Errorf("expected [fail] OPENAI_API_KEY unset. Got:\n%s", body)
	}
	if !strings.Contains(body, "[fail] codex NOT on PATH") {
		t.Errorf("expected [fail] codex NOT on PATH. Got:\n%s", body)
	}
	if !strings.Contains(body, "sudo npm install -g @openai/codex") {
		t.Errorf("expected codex npm install hint. Got:\n%s", body)
	}
	if !strings.Contains(body, "Node.js 22+") {
		t.Errorf("expected codex Node.js 22+ hint. Got:\n%s", body)
	}
	if !strings.Contains(body, "deb.nodesource.com/setup_22.x") {
		t.Errorf("expected codex NodeSource one-liner. Got:\n%s", body)
	}
	// Codex must not be told to ping anthropic.com.
	if strings.Contains(body, "api.anthropic.com") {
		t.Errorf("codex doctor must not check api.anthropic.com. Got:\n%s", body)
	}
	if fails < 2 {
		t.Errorf("expected >=2 failures for codex with both missing; got %d. Body:\n%s", fails, body)
	}
}

// TestDoctor_Claude_NoBinaryCheck — provider=claude (the REST path) only
// needs ANTHROPIC_API_KEY; doctor must NOT try to LookPath any provider
// binary for this config.
func TestDoctor_Claude_NoBinaryCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on windows")
	}
	cfgPath, restore := doctorFixture(t, "claude", "claude-sonnet-4-6", "ANTHROPIC_API_KEY", "")
	defer restore()
	setEnv(t, "ANTHROPIC_API_KEY", "sk-ant-stub-1234567890")

	out := &bytes.Buffer{}
	_ = runDoctor(context.Background(), out, cfgPath)
	body := out.String()

	if !strings.Contains(body, "[ok]   env ANTHROPIC_API_KEY is set") {
		t.Errorf("expected ANTHROPIC_API_KEY [ok] line. Got:\n%s", body)
	}
	for _, badLine := range []string{
		"claude NOT on PATH",
		"codex NOT on PATH",
		"@anthropic-ai/claude-code", // install hint only renders on binary fail
		"@openai/codex",
	} {
		if strings.Contains(body, badLine) {
			t.Errorf("provider=claude (REST) must not trigger binary install hints. Got %q in:\n%s", badLine, body)
		}
	}
}

// TestDoctor_SummaryAndCounts — sanity-check the summary line: counts must
// sum to >= 1, and the format is the documented "[summary] N pass, M fail".
// Release notes / install runbook grep for this exact prefix.
func TestDoctor_SummaryAndCounts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on windows")
	}
	cfgPath, restore := doctorFixture(t, "claude-cli", "claude-haiku-4-5-20251001", "CLAUDE_CODE_OAUTH_TOKEN", "claude")
	defer restore()
	setEnv(t, "CLAUDE_CODE_OAUTH_TOKEN", "stub")

	out := &bytes.Buffer{}
	_ = runDoctor(context.Background(), out, cfgPath)
	body := out.String()

	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "[summary] ") {
		t.Errorf("expected last line to start with '[summary] '. Got %q. Full body:\n%s", last, body)
	}
	var pass, fail int
	if _, err := fmt.Sscanf(last, "[summary] %d pass, %d fail", &pass, &fail); err != nil {
		t.Errorf("summary line not in 'N pass, M fail' shape: %q (err %v)", last, err)
	}
	if pass < 1 {
		t.Errorf("expected at least 1 pass in happy-path summary. Got %q", last)
	}
}

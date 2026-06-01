// Copyright 2026 Zibby Lab. Apache-2.0.

package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/config"
	"github.com/ZibbyHQ/agent-ops/internal/state"
)

// stubPlanner is the test seam swapped in via activePlanner. fn is invoked
// every iteration; it writes whatever script string the test wants to the
// PlanInput.ScriptPath and returns PlanOutput. Iteration-aware via a
// counter so the same test can simulate v1 failing → v2 succeeding.
type stubPlanner struct {
	calls int
	fn    func(iter int, in PlanInput) (string, error)
}

func (p *stubPlanner) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	p.calls++
	script, err := p.fn(p.calls, in)
	if err != nil {
		return PlanOutput{}, err
	}
	if script != "" {
		if werr := os.WriteFile(in.ScriptPath, []byte(script), 0o700); werr != nil {
			return PlanOutput{}, werr
		}
	}
	return PlanOutput{RawScript: script, NumTurns: 3}, nil
}

// withStubPlanner sets the package-level planner for the duration of the
// test and restores it on cleanup. Mirrors t.Setenv's idiom.
func withStubPlanner(t *testing.T, p Planner) {
	t.Helper()
	prev := activePlanner
	activePlanner = p
	t.Cleanup(func() { activePlanner = prev })
}

// withScratchScriptPath swaps /tmp/install.sh for a tempdir copy so
// parallel test runs don't trample each other.
func withScratchScriptPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	// Make sure stale state from a prior test never shows through
	_ = os.Remove(path)
	return path
}

// TestPlannerAllowedToolsIsWriteRead is the load-bearing safety test:
// production planner construction MUST NOT expose Bash. Adding Bash to
// the allow-list reintroduces the auto-background bug this mode exists
// to fix. Test reads the field directly off the constructed driver so
// no refactor can quietly elevate it.
func TestPlannerAllowedToolsIsWriteRead(t *testing.T) {
	p := newClaudecliPlanner("claude-sonnet-4-5")
	if p.Driver == nil {
		t.Fatal("planner constructed without driver")
	}
	if p.Driver.AllowedTools != "Write,Read" {
		t.Fatalf("AllowedTools = %q, want %q (Bash MUST be excluded from Phase 1)",
			p.Driver.AllowedTools, "Write,Read")
	}
	if strings.Contains(p.Driver.AllowedTools, "Bash") {
		t.Fatalf("AllowedTools contains Bash — Phase 1 MUST NOT expose Bash, got %q",
			p.Driver.AllowedTools)
	}
	if strings.Contains(p.Driver.AllowedTools, "Edit") {
		t.Fatalf("AllowedTools contains Edit — Phase 1 MUST NOT expose Edit, got %q",
			p.Driver.AllowedTools)
	}
	if p.Driver.PermissionMode != "acceptEdits" {
		t.Fatalf("PermissionMode = %q, want acceptEdits (OAuth rejects bypassPermissions)",
			p.Driver.PermissionMode)
	}
	if p.Driver.Model != "claude-sonnet-4-5" {
		t.Fatalf("Model = %q, want claude-sonnet-4-5", p.Driver.Model)
	}
}

// TestBuildPlannerPrompts verifies prompt template stability across
// iteration 1 vs N+1. Dashboard/code-review needs to substring-grep
// against these strings, so they are part of the contract.
func TestBuildPlannerPrompts(t *testing.T) {
	t.Run("iteration 1 has no prior-stderr block", func(t *testing.T) {
		system, user := buildPlannerPrompts(PlanInput{
			Goal:       "install n8n on port 5678",
			Iteration:  1,
			ScriptPath: "/tmp/install.sh",
		})
		if !strings.Contains(system, "You DO NOT have access to the Bash tool") {
			t.Fatal("system prompt must declare Bash is disabled")
		}
		if !strings.Contains(system, "/tmp/install.sh") {
			t.Fatal("system prompt must name the script path")
		}
		if !strings.Contains(user, "install n8n on port 5678") {
			t.Fatalf("user prompt must include the goal verbatim, got %q", user)
		}
		if strings.Contains(user, "Previous attempt failed") {
			t.Fatal("iteration 1 must NOT include 'Previous attempt failed'")
		}
	})

	t.Run("iteration 2 includes prior stderr/stdout/exit", func(t *testing.T) {
		_, user := buildPlannerPrompts(PlanInput{
			Goal:         "install n8n on port 5678",
			Iteration:    2,
			ScriptPath:   "/tmp/install.sh",
			PrevExitCode: 42,
			PrevStderr:   "npm ERR! E404 not found",
			PrevStdout:   "added 117 packages",
		})
		if !strings.Contains(user, "Previous attempt failed (iteration 2)") {
			t.Fatal("retry prompt must announce iteration N>1")
		}
		if !strings.Contains(user, "npm ERR! E404") {
			t.Fatal("retry prompt must include previous stderr tail")
		}
		if !strings.Contains(user, "added 117 packages") {
			t.Fatal("retry prompt must include previous stdout tail")
		}
		if !strings.Contains(user, "42") {
			t.Fatal("retry prompt must include the previous exit code")
		}
	})

	t.Run("verify port adds curl health-check clause", func(t *testing.T) {
		system, _ := buildPlannerPrompts(PlanInput{
			Goal:       "install grafana",
			Iteration:  1,
			ScriptPath: "/tmp/install.sh",
			VerifyPort: 3000,
		})
		if !strings.Contains(system, "curl -sf") || !strings.Contains(system, "3000") {
			t.Fatalf("verify clause missing or wrong port, got system=%q", system)
		}
	})

	t.Run("house rules block prepended when set", func(t *testing.T) {
		system, _ := buildPlannerPrompts(PlanInput{
			Goal:       "install x",
			Iteration:  1,
			ScriptPath: "/tmp/install.sh",
			HouseRules: "1. always use apt before snap.",
		})
		if !strings.Contains(system, "==== HOUSE RULES ====") {
			t.Fatal("house rules block missing")
		}
		if !strings.Contains(system, "always use apt before snap") {
			t.Fatal("house rules content missing")
		}
	})
}

// TestRunAgentScriptBootstrap_HappyPath: iteration 1 writes a script that
// exits 0; loop terminates immediately with success.
func TestRunAgentScriptBootstrap_HappyPath(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	marker := filepath.Join(dir, "happy-ran")

	stub := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		if in.ScriptPath == "" {
			t.Fatalf("PlanInput.ScriptPath empty")
		}
		return fmt.Sprintf("#!/bin/bash\ntouch %s\nexit 0\n", marker), nil
	}}
	withStubPlanner(t, stub)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install something")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "3")
	t.Setenv("AGENT_OPS_BOOTSTRAP_VERIFY_PORT", "") // none
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := runAgentScriptBootstrap(ctx, cfg, store); err != nil {
		t.Fatalf("runAgentScriptBootstrap: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("script did not run (marker missing): %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("planner called %d times, want 1", stub.calls)
	}

	// final-status.json should record success
	raw, err := os.ReadFile(filepath.Join(dir, "agent_script-state", "final-status.json"))
	if err != nil {
		t.Fatalf("final-status.json missing: %v", err)
	}
	var fs finalStatus
	if err := json.Unmarshal(raw, &fs); err != nil {
		t.Fatalf("final-status.json malformed: %v", err)
	}
	if !fs.Success {
		t.Fatalf("final-status success=false, want true: %+v", fs)
	}
	if fs.Iterations != 1 {
		t.Fatalf("final-status iterations=%d, want 1", fs.Iterations)
	}

	// script-v1.sh audit file must exist
	if _, err := os.Stat(filepath.Join(dir, "agent_script-state", "script-v1.sh")); err != nil {
		t.Fatalf("script-v1.sh audit file missing: %v", err)
	}
}

// TestRunAgentScriptBootstrap_SecondIterationRecovers: iteration 1 exits
// 1; planner receives the stderr in iteration 2 and writes a succeeding
// script.
func TestRunAgentScriptBootstrap_SecondIterationRecovers(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	marker := filepath.Join(dir, "v2-ran")
	var sawPrevStderr bool

	stub := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		if iter == 1 {
			return "#!/bin/bash\necho 'about to fail' >&2\nexit 7\n", nil
		}
		// iter 2: assert the planner received the previous attempt's
		// stderr in PlanInput.PrevStderr.
		if !strings.Contains(in.PrevStderr, "about to fail") {
			t.Errorf("iter 2 missing prev stderr, got %q", in.PrevStderr)
		} else {
			sawPrevStderr = true
		}
		if in.PrevExitCode != 7 {
			t.Errorf("iter 2 PrevExitCode = %d, want 7", in.PrevExitCode)
		}
		return fmt.Sprintf("#!/bin/bash\ntouch %s\nexit 0\n", marker), nil
	}}
	withStubPlanner(t, stub)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install something")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "5")
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := runAgentScriptBootstrap(ctx, cfg, store); err != nil {
		t.Fatalf("runAgentScriptBootstrap: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("planner called %d times, want 2", stub.calls)
	}
	if !sawPrevStderr {
		t.Fatal("iter 2 planner never saw the iter 1 stderr")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("iter 2 script did not run: %v", err)
	}
}

// TestRunAgentScriptBootstrap_MaxIterExhaustion: every iteration fails;
// loop returns error after N attempts and writes failed final-status.
func TestRunAgentScriptBootstrap_MaxIterExhaustion(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	stub := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		return "#!/bin/bash\necho 'never works' >&2\nexit 99\n", nil
	}}
	withStubPlanner(t, stub)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install impossible thing")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "3")
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runAgentScriptBootstrap(ctx, cfg, store)
	if err == nil {
		t.Fatal("expected error after iteration exhaustion")
	}
	if !strings.Contains(err.Error(), "3 iterations") {
		t.Fatalf("error should mention iteration count, got %v", err)
	}
	if stub.calls != 3 {
		t.Fatalf("planner called %d times, want 3", stub.calls)
	}

	// final-status.json must record success=false
	raw, err := os.ReadFile(filepath.Join(dir, "agent_script-state", "final-status.json"))
	if err != nil {
		t.Fatalf("final-status.json missing: %v", err)
	}
	var fs finalStatus
	if err := json.Unmarshal(raw, &fs); err != nil {
		t.Fatalf("final-status.json malformed: %v", err)
	}
	if fs.Success {
		t.Fatalf("final-status success=true, want false: %+v", fs)
	}
	if fs.ExitCode != 99 {
		t.Fatalf("final-status exit_code=%d, want 99", fs.ExitCode)
	}

	// script-v1, v2, v3 stderr files must exist
	for i := 1; i <= 3; i++ {
		path := filepath.Join(dir, "agent_script-state", fmt.Sprintf("script-v%d.stderr", i))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("audit file %s missing: %v", path, err)
		}
	}
}

// TestRunAgentScriptBootstrap_VerifyPort_OK: script exits 0 + verifier
// curl returns 200 → success.
func TestRunAgentScriptBootstrap_VerifyPort_OK(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	// Spin up a real HTTP server on an ephemeral port and point the
	// verifier at it. 200 → success.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()
	port := mustExtractPort(t, ts.URL)

	scriptPath := withScratchScriptPath(t)
	stub := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		if in.VerifyPort != port {
			t.Errorf("planner did not receive verify port, got %d want %d",
				in.VerifyPort, port)
		}
		return "#!/bin/bash\nexit 0\n", nil
	}}
	withStubPlanner(t, stub)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install something")
	t.Setenv("AGENT_OPS_BOOTSTRAP_VERIFY_PORT", strconv.Itoa(port))
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "2")
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := runAgentScriptBootstrap(ctx, cfg, store); err != nil {
		t.Fatalf("runAgentScriptBootstrap: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "agent_script-state", "final-status.json"))
	var fs finalStatus
	_ = json.Unmarshal(raw, &fs)
	if fs.FinalCurlCode != 200 {
		t.Fatalf("final_curl_code = %d, want 200: %+v", fs.FinalCurlCode, fs)
	}
}

// TestRunAgentScriptBootstrap_VerifyPort_5xxFails: script exits 0 but
// verifier returns 500 → bootstrap fails, exhausts iterations.
func TestRunAgentScriptBootstrap_VerifyPort_5xxFails(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()
	port := mustExtractPort(t, ts.URL)

	scriptPath := withScratchScriptPath(t)
	stub := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		return "#!/bin/bash\nexit 0\n", nil
	}}
	withStubPlanner(t, stub)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install something")
	t.Setenv("AGENT_OPS_BOOTSTRAP_VERIFY_PORT", strconv.Itoa(port))
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "2")
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	// Tight timeout — verifier is 6×5s = 30s per iteration, two iter =
	// 60s budget. Pad to 75s.
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()

	err := runAgentScriptBootstrap(ctx, cfg, store)
	if err == nil {
		t.Fatal("expected error — verifier should have rejected 500")
	}
	if !strings.Contains(err.Error(), "verify port") && !strings.Contains(err.Error(), "iterations exhausted") {
		t.Fatalf("error should mention verify failure or iteration exhaust, got %v", err)
	}
}

// TestRunAgentScriptBootstrap_EmptyGoalErrors: AGENT_OPS_BOOTSTRAP_PROMPT
// unset → immediate error, no planner invocation.
func TestRunAgentScriptBootstrap_EmptyGoalErrors(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	stub := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		t.Error("planner must not be called when goal is empty")
		return "", nil
	}}
	withStubPlanner(t, stub)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "")

	cfg := &config.Config{StateDir: dir}
	err := runAgentScriptBootstrap(context.Background(), cfg, store)
	if err == nil {
		t.Fatal("expected error for empty goal")
	}
}

// TestAgentScriptMaxIterations_DefaultAndClamp: env handling.
func TestAgentScriptMaxIterations_DefaultAndClamp(t *testing.T) {
	t.Run("default 5 when unset", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "")
		if n := agentScriptMaxIterations(); n != 5 {
			t.Fatalf("got %d, want 5", n)
		}
	})
	t.Run("explicit 3 honored", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "3")
		if n := agentScriptMaxIterations(); n != 3 {
			t.Fatalf("got %d, want 3", n)
		}
	})
	t.Run("clamps to 10 max", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "9999")
		if n := agentScriptMaxIterations(); n != 10 {
			t.Fatalf("got %d, want 10 (clamp)", n)
		}
	})
	t.Run("invalid falls back to default", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "garbage")
		if n := agentScriptMaxIterations(); n != 5 {
			t.Fatalf("got %d, want 5 (default fallback)", n)
		}
	})
}

// TestAgentScriptVerifyPort: env handling.
func TestAgentScriptVerifyPort(t *testing.T) {
	t.Run("unset → 0 (no verifier)", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_VERIFY_PORT", "")
		if n := agentScriptVerifyPort(); n != 0 {
			t.Fatalf("got %d, want 0", n)
		}
	})
	t.Run("valid 1..65535 honored", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_VERIFY_PORT", "5678")
		if n := agentScriptVerifyPort(); n != 5678 {
			t.Fatalf("got %d, want 5678", n)
		}
	})
	t.Run("out of range → 0", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_VERIFY_PORT", "99999")
		if n := agentScriptVerifyPort(); n != 0 {
			t.Fatalf("got %d, want 0", n)
		}
	})
}

// TestIsAgentScriptMode: dispatcher selector.
func TestIsAgentScriptMode(t *testing.T) {
	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	if !isAgentScriptMode() {
		t.Fatal("expected isAgentScriptMode=true for 'agent_script'")
	}
	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "AGENT_SCRIPT")
	if !isAgentScriptMode() {
		t.Fatal("expected case-insensitive match")
	}
	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent")
	if isAgentScriptMode() {
		t.Fatal("legacy 'agent' mode must not match agent_script")
	}
	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "")
	if isAgentScriptMode() {
		t.Fatal("empty must not match")
	}
}

// ─── helpers ──────────────────────────────────────────────────────────

// mustOpenStore opens a state.Store in the given temp dir or fails the test.
func mustOpenStore(t *testing.T, dir string) *state.Store {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// mustExtractPort pulls the port int out of a httptest.Server URL.
func mustExtractPort(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// overrideScriptPathForTest swaps the package-level scriptPath constant
// for the duration of the test. We achieve this by setting an env var
// the production code consults; if no such hook exists yet, the test
// helper redirects via a t.Cleanup that puts the constant back.
//
// Implementation detail: the production code uses a const, so we patch
// the package-level via the testScriptPathOverride var (declared in
// agent_script.go via build-tag-less indirection — see scriptPathFor()).
func overrideScriptPathForTest(t *testing.T, path string) {
	t.Helper()
	prev := testScriptPathOverride
	testScriptPathOverride = path
	t.Cleanup(func() { testScriptPathOverride = prev })
}

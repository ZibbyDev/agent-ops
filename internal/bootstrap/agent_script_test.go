// Copyright 2026 Zibby Lab. Apache-2.0.

package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZibbyDev/agent-ops/internal/config"
	"github.com/ZibbyDev/agent-ops/internal/state"
)

// stubPlanner is the test seam swapped in via activePlanner. fn is invoked
// every iteration; it writes whatever script string the test wants to the
// PlanInput.ScriptPath and returns PlanOutput.
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

// stubSupervisor is the test seam swapped in via activeSupervisor. fn
// returns a SupervisorOutput per call so tests can model continue→done,
// continue×5→done, malformed JSON, etc. without spawning the real CLI.
type stubSupervisor struct {
	calls int32
	fn    func(call int, in SupervisorInput) (SupervisorOutput, error)
}

func (s *stubSupervisor) Check(ctx context.Context, in SupervisorInput) (SupervisorOutput, error) {
	n := atomic.AddInt32(&s.calls, 1)
	return s.fn(int(n), in)
}

func (s *stubSupervisor) Calls() int { return int(atomic.LoadInt32(&s.calls)) }

// withStubPlanner sets the package-level planner for the duration of the
// test and restores it on cleanup.
func withStubPlanner(t *testing.T, p Planner) {
	t.Helper()
	prev := activePlanner
	activePlanner = p
	t.Cleanup(func() { activePlanner = prev })
}

// withStubSupervisor sets the package-level supervisor for the duration of
// the test and restores it on cleanup. EVERY test that drives
// runAgentScriptBootstrap MUST install a stub or the real claudecli
// driver will spawn during the test.
func withStubSupervisor(t *testing.T, s Supervisor) {
	t.Helper()
	prev := activeSupervisor
	activeSupervisor = s
	t.Cleanup(func() { activeSupervisor = prev })
}

// withScratchScriptPath swaps /tmp/install.sh for a tempdir copy so
// parallel test runs don't trample each other.
func withScratchScriptPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	_ = os.Remove(path)
	return path
}

// TestPlannerAllowedToolsIsWriteRead — load-bearing safety test:
// production planner construction MUST NOT expose Bash. Adding Bash to
// the allow-list reintroduces the auto-background bug this mode exists
// to fix.
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

// TestSupervisorAllowedToolsHasNoBashWriteOrEdit — symmetric safety net
// for Phase 2. The supervisor's job is to emit a JSON verdict, not to
// modify the system. claudecli falls back to "Bash,Read,Write,Edit"
// when AllowedTools is empty, so the constructor MUST set an explicit
// value that excludes every side-effect-having tool.
func TestSupervisorAllowedToolsHasNoBashWriteOrEdit(t *testing.T) {
	s := newClaudecliSupervisor("claude-sonnet-4-5")
	if s.Driver == nil {
		t.Fatal("supervisor constructed without driver")
	}
	if s.Driver.AllowedTools == "" {
		t.Fatalf("supervisor AllowedTools is empty — would inherit the claudecli "+
			"default Bash,Read,Write,Edit and let Claude run arbitrary commands. "+
			"Set an explicit non-empty value. got %q", s.Driver.AllowedTools)
	}
	for _, banned := range []string{"Bash", "Write", "Edit"} {
		if strings.Contains(s.Driver.AllowedTools, banned) {
			t.Fatalf("supervisor AllowedTools contains %q (full value %q) — "+
				"the supervisor must be tool-free (pure JSON text response)",
				banned, s.Driver.AllowedTools)
		}
	}
}

// TestBuildPlannerPrompts verifies prompt template stability.
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

	t.Run("iteration 2 with supervisor reason includes it", func(t *testing.T) {
		_, user := buildPlannerPrompts(PlanInput{
			Goal:         "install x",
			Iteration:    2,
			ScriptPath:   "/tmp/install.sh",
			PrevExitCode: -1,
			PrevReason:   "process stuck for 5min with no log activity",
		})
		if !strings.Contains(user, "SUPERVISOR REASON") {
			t.Fatal("retry prompt should label the supervisor reason block")
		}
		if !strings.Contains(user, "process stuck for 5min") {
			t.Fatal("retry prompt should include the supervisor's intervene reason")
		}
	})

	t.Run("verify port adds curl health-check clause as a hint", func(t *testing.T) {
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

// TestBuildSupervisorPrompts checks the supervisor system + user prompts
// carry the contract the supervise loop depends on.
func TestBuildSupervisorPrompts(t *testing.T) {
	exitCode := 0
	system, user := buildSupervisorPrompts(SupervisorInput{
		Goal:           "install n8n",
		IterationNum:   2,
		ScriptBody:     "#!/bin/bash\nfoo bar\n",
		IterStart:      time.Now().Add(-3 * time.Minute),
		SupervisorTurn: 4,
		ProcStatus:     "EXITED",
		ExitCode:       &exitCode,
		TailBytes:      "lots of npm output",
		BytesSinceWake: 1024,
		IdleDuration:   2 * time.Second,
		VerifyPortHint: 5678,
	})
	for _, want := range []string{
		`{"verdict":"continue"`,
		`{"verdict":"done"`,
		`{"verdict":"intervene"`,
		"Use 'done' ONLY if process EXITED with code 0",
		"App goal (from operator): install n8n",
		"Current iteration script (v2):",
		"5678",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing substring %q", want)
		}
	}
	for _, want := range []string{
		"ITERATION 2",
		"SUPERVISOR TURN 4",
		"PROC_STATUS: EXITED",
		"EXIT_CODE: 0",
		"lots of npm output",
		"VERDICT?",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing substring %q", want)
		}
	}
}

// TestParseSupervisorVerdict — verdict parsing incl. malformed-JSON path.
func TestParseSupervisorVerdict(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantVerdict string
		wantErr     bool
	}{
		{"clean done", `{"verdict":"done","note":"app responding"}`, "done", false},
		{"clean continue", `{"verdict":"continue","note":"npm installing"}`, "continue", false},
		{"clean intervene", `{"verdict":"intervene","reason":"OOM","note":"out of memory"}`, "intervene", false},
		{"prose around JSON", `Sure, here you go:\n{"verdict":"done","note":"ok"}\nThanks!`, "done", false},
		{"markdown fence", "```json\n{\"verdict\":\"done\",\"note\":\"ok\"}\n```", "done", false},
		{"no JSON at all", "totally not json", "continue", true},
		{"malformed JSON", `{"verdict":"done", note: bad}`, "continue", true},
		{"unknown verdict", `{"verdict":"yolo","note":"x"}`, "continue", true},
		{"empty verdict", `{"verdict":"","note":"x"}`, "continue", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := parseSupervisorVerdict(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if v.Verdict != c.wantVerdict {
				t.Fatalf("verdict=%q, want %q (raw=%q)", v.Verdict, c.wantVerdict, c.in)
			}
		})
	}
}

// TestRunAgentScriptBootstrap_HappyPath: iter 1 writes a script that
// exits 0; supervisor returns "done" → success after one supervisor turn.
func TestRunAgentScriptBootstrap_HappyPath(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	marker := filepath.Join(dir, "happy-ran")

	planner := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		if in.ScriptPath == "" {
			t.Fatalf("PlanInput.ScriptPath empty")
		}
		return fmt.Sprintf("#!/bin/bash\ntouch %s\nexit 0\n", marker), nil
	}}
	withStubPlanner(t, planner)

	sup := &stubSupervisor{fn: func(call int, in SupervisorInput) (SupervisorOutput, error) {
		return SupervisorOutput{
			Verdict: SupervisorVerdict{Verdict: "done", Note: "ok"},
		}, nil
	}}
	withStubSupervisor(t, sup)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install something")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "3")
	t.Setenv("AGENT_OPS_BOOTSTRAP_VERIFY_PORT", "")
	t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "5") // floor; only one tick anyway
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
	if planner.calls != 1 {
		t.Fatalf("planner called %d times, want 1", planner.calls)
	}
	if sup.Calls() < 1 {
		t.Fatalf("supervisor called %d times, want >= 1", sup.Calls())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "agent_script-state", "final-status.json"))
	if err != nil {
		t.Fatalf("final-status.json missing: %v", err)
	}
	// V2 reader.
	var fs2 struct {
		Success         bool   `json:"success"`
		Iterations      int    `json:"iterations"`
		SupervisorTurns int    `json:"supervisor_turns"`
		FinalVerdict    string `json:"final_verdict"`
	}
	if err := json.Unmarshal(raw, &fs2); err != nil {
		t.Fatalf("final-status.json malformed: %v", err)
	}
	if !fs2.Success {
		t.Fatalf("final-status success=false, want true: %+v", fs2)
	}
	if fs2.FinalVerdict != "done" {
		t.Fatalf("final_verdict=%q, want done", fs2.FinalVerdict)
	}
	if fs2.Iterations != 1 {
		t.Fatalf("iterations=%d, want 1", fs2.Iterations)
	}
	// Legacy v1 reader must still work (downstream dashboards).
	var fsLegacy finalStatus
	if err := json.Unmarshal(raw, &fsLegacy); err != nil {
		t.Fatalf("legacy finalStatus reader broke: %v", err)
	}
	if !fsLegacy.Success {
		t.Fatalf("legacy success field not set, want true")
	}

	// script-v1.sh + live log + supervisor-turns log must all exist.
	for _, fname := range []string{
		"script-v1.sh",
		"script-v1.live.log",
		"script-v1.supervisor-turns.log",
	} {
		if _, err := os.Stat(filepath.Join(dir, "agent_script-state", fname)); err != nil {
			t.Fatalf("expected audit file %s missing: %v", fname, err)
		}
	}
}

// TestRunAgentScriptBootstrap_ContinueThenDone: bash sleeps a bit, the
// supervisor returns "continue" 5x then "done". Loop should NOT
// short-circuit on the continues and MUST stop on the first done.
func TestRunAgentScriptBootstrap_ContinueThenDone(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	planner := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		// Sleep enough that the supervisor polls fire while the
		// process is still RUNNING. 6 × 50ms = 300ms wall-clock; the
		// process actually runs ~300ms.
		return "#!/bin/bash\nfor i in 1 2 3 4 5 6; do echo step $i; sleep 0.05; done\nexit 0\n", nil
	}}
	withStubPlanner(t, planner)

	sup := &stubSupervisor{fn: func(call int, in SupervisorInput) (SupervisorOutput, error) {
		if call < 6 {
			return SupervisorOutput{
				Verdict: SupervisorVerdict{Verdict: "continue", Note: fmt.Sprintf("step %d", call)},
			}, nil
		}
		return SupervisorOutput{
			Verdict: SupervisorVerdict{Verdict: "done", Note: "all done"},
		}, nil
	}}
	withStubSupervisor(t, sup)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install slow thing")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "2")
	t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "5") // floor — selects on waitDone fast
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := runAgentScriptBootstrap(ctx, cfg, store); err != nil {
		t.Fatalf("runAgentScriptBootstrap: %v", err)
	}
	if planner.calls != 1 {
		t.Fatalf("planner called %d times, want 1", planner.calls)
	}
	if sup.Calls() != 6 {
		t.Fatalf("supervisor called %d times, want exactly 6", sup.Calls())
	}
}

// TestRunAgentScriptBootstrap_InterveneThenSuccess: iter 1 supervisor
// says intervene → planner runs again → iter 2 supervisor says done.
func TestRunAgentScriptBootstrap_InterveneThenSuccess(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	var v2ReasonSeen string
	planner := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		if iter == 2 {
			v2ReasonSeen = in.PrevReason
		}
		return "#!/bin/bash\nsleep 10\n", nil
	}}
	withStubPlanner(t, planner)

	var iterOfCall int32 = 0
	sup := &stubSupervisor{fn: func(call int, in SupervisorInput) (SupervisorOutput, error) {
		if in.IterationNum == 1 {
			atomic.StoreInt32(&iterOfCall, 1)
			return SupervisorOutput{
				Verdict: SupervisorVerdict{
					Verdict: "intervene",
					Reason:  "stuck on apt mirror",
					Note:    "killing v1",
				},
			}, nil
		}
		return SupervisorOutput{
			Verdict: SupervisorVerdict{Verdict: "done", Note: "v2 ok"},
		}, nil
	}}
	withStubSupervisor(t, sup)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install retry thing")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "3")
	t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "5")
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := runAgentScriptBootstrap(ctx, cfg, store); err != nil {
		t.Fatalf("runAgentScriptBootstrap: %v", err)
	}
	if planner.calls != 2 {
		t.Fatalf("planner called %d times, want 2 (intervene → replan)", planner.calls)
	}
	if !strings.Contains(v2ReasonSeen, "stuck on apt mirror") {
		t.Fatalf("iter 2 planner did not receive supervisor reason, got %q", v2ReasonSeen)
	}
	// Both supervisor-turns logs must exist.
	for _, fname := range []string{
		"script-v1.supervisor-turns.log",
		"script-v2.supervisor-turns.log",
	} {
		if _, err := os.Stat(filepath.Join(dir, "agent_script-state", fname)); err != nil {
			t.Fatalf("audit %s missing: %v", fname, err)
		}
	}
}

// TestRunAgentScriptBootstrap_MalformedJSONContinues: when the supervisor
// stub returns a malformed-JSON SupervisorOutput, the loop must treat it
// as continue (parse_error logged in the audit) and keep polling.
func TestRunAgentScriptBootstrap_MalformedJSONContinues(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	planner := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		return "#!/bin/bash\nsleep 0.2\n", nil
	}}
	withStubPlanner(t, planner)

	sup := &stubSupervisor{fn: func(call int, in SupervisorInput) (SupervisorOutput, error) {
		if call < 3 {
			// The package's own parseSupervisorVerdict would map a
			// malformed reply to {Verdict:"continue"} + ParseError set;
			// simulate that exact path here.
			return SupervisorOutput{
				Verdict:    SupervisorVerdict{Verdict: "continue", Note: "supervisor JSON did not parse"},
				Raw:        "garbage from claude",
				ParseError: "no JSON object in supervisor reply",
			}, nil
		}
		return SupervisorOutput{
			Verdict: SupervisorVerdict{Verdict: "done", Note: "ok"},
		}, nil
	}}
	withStubSupervisor(t, sup)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install thing")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "2")
	t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "5")
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := runAgentScriptBootstrap(ctx, cfg, store); err != nil {
		t.Fatalf("runAgentScriptBootstrap: %v", err)
	}
	if sup.Calls() < 3 {
		t.Fatalf("supervisor called only %d times, expected >= 3 (malformed continues + done)", sup.Calls())
	}

	// The supervisor-turns audit log must include the parse_error entries.
	raw, err := os.ReadFile(filepath.Join(dir, "agent_script-state", "script-v1.supervisor-turns.log"))
	if err != nil {
		t.Fatalf("supervisor-turns log missing: %v", err)
	}
	if !strings.Contains(string(raw), "parse_error") {
		t.Fatalf("supervisor-turns log should contain parse_error entries, got %q", string(raw))
	}
}

// TestRunAgentScriptBootstrap_BudgetExceeded: iter 1 supervisor returns
// continue forever, but each call reports a high cost that exceeds the
// budget. Loop should kill iter 1 mid-flight OR refuse to start iter 2.
func TestRunAgentScriptBootstrap_BudgetExceeded(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	planner := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		return "#!/bin/bash\nfor i in $(seq 1 200); do echo $i; sleep 0.02; done\n", nil
	}}
	withStubPlanner(t, planner)

	// Each call costs $0.30 → after the 4th call we're at $1.20 > $1.00.
	sup := &stubSupervisor{fn: func(call int, in SupervisorInput) (SupervisorOutput, error) {
		return SupervisorOutput{
			Verdict:      SupervisorVerdict{Verdict: "continue", Note: "still going"},
			CostUSDMicro: 300_000, // $0.30 per call
		}, nil
	}}
	withStubSupervisor(t, sup)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install pricey thing")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "5")
	t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "5")
	t.Setenv("AGENT_OPS_BOOTSTRAP_TOKEN_BUDGET_USD", "1.00")
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := runAgentScriptBootstrap(ctx, cfg, store)
	if err == nil {
		t.Fatal("expected error — budget should have been exceeded")
	}
	// Budget exhaustion message MUST surface in the error somewhere down
	// the wrap chain.
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("error should mention budget, got %v", err)
	}
	// Only one iteration should have been planned — second iter refused.
	if planner.calls != 1 {
		t.Fatalf("planner called %d times, want exactly 1 (budget refusal)", planner.calls)
	}
}

// TestRunAgentScriptBootstrap_MaxIterExhaustion: every iteration ends in
// intervene → loop returns error after N attempts.
func TestRunAgentScriptBootstrap_MaxIterExhaustion(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	planner := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		return "#!/bin/bash\nsleep 5\n", nil
	}}
	withStubPlanner(t, planner)

	sup := &stubSupervisor{fn: func(call int, in SupervisorInput) (SupervisorOutput, error) {
		return SupervisorOutput{
			Verdict: SupervisorVerdict{
				Verdict: "intervene",
				Reason:  "never works",
				Note:    "killing",
			},
		}, nil
	}}
	withStubSupervisor(t, sup)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install impossible thing")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "3")
	t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "5")
	t.Setenv("AGENT_OPS_BOOTSTRAP_TOKEN_BUDGET_USD", "0") // disable budget
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := runAgentScriptBootstrap(ctx, cfg, store)
	if err == nil {
		t.Fatal("expected error after iteration exhaustion")
	}
	if !strings.Contains(err.Error(), "3 iterations") {
		t.Fatalf("error should mention iteration count, got %v", err)
	}
	if planner.calls != 3 {
		t.Fatalf("planner called %d times, want 3", planner.calls)
	}

	// final-status.json must record success=false.
	raw, err := os.ReadFile(filepath.Join(dir, "agent_script-state", "final-status.json"))
	if err != nil {
		t.Fatalf("final-status.json missing: %v", err)
	}
	var fs2 struct {
		Success      bool   `json:"success"`
		FinalVerdict string `json:"final_verdict"`
		Iterations   int    `json:"iterations"`
	}
	if err := json.Unmarshal(raw, &fs2); err != nil {
		t.Fatalf("final-status.json malformed: %v", err)
	}
	if fs2.Success {
		t.Fatalf("final-status success=true, want false: %+v", fs2)
	}
	if fs2.FinalVerdict != "intervene" {
		t.Fatalf("final_verdict=%q, want intervene", fs2.FinalVerdict)
	}
}

// TestRunAgentScriptBootstrap_ProcessKilledExternally: simulate a process
// that exits non-zero before any supervisor poll completes the loop. The
// supervisor's first observation should be PROC_STATUS=EXITED with the
// non-zero exit code, and an intervene verdict triggers the retry path.
func TestRunAgentScriptBootstrap_ProcessKilledExternally(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	scriptPath := withScratchScriptPath(t)
	planner := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		// Exit with code 137 to mimic an OOM kill.
		return "#!/bin/bash\necho 'about to die' >&2\nexit 137\n", nil
	}}
	withStubPlanner(t, planner)

	var sawExited137 atomic.Bool
	sup := &stubSupervisor{fn: func(call int, in SupervisorInput) (SupervisorOutput, error) {
		if in.ProcStatus == "EXITED" && in.ExitCode != nil && *in.ExitCode == 137 {
			sawExited137.Store(true)
		}
		return SupervisorOutput{
			Verdict: SupervisorVerdict{
				Verdict: "intervene",
				Reason:  "process exited 137 (OOM)",
				Note:    "OOM kill",
			},
		}, nil
	}}
	withStubSupervisor(t, sup)

	t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", "agent_script")
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install OOM thing")
	t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS", "2")
	t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "5")
	t.Setenv("AGENT_OPS_BOOTSTRAP_TOKEN_BUDGET_USD", "0")
	overrideScriptPathForTest(t, scriptPath)

	cfg := &config.Config{StateDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runAgentScriptBootstrap(ctx, cfg, store)
	if err == nil {
		t.Fatal("expected error — every iteration intervenes")
	}
	if !sawExited137.Load() {
		t.Fatal("supervisor never observed PROC_STATUS=EXITED + EXIT_CODE=137")
	}
}

// TestRunAgentScriptBootstrap_EmptyGoalErrors: AGENT_OPS_BOOTSTRAP_PROMPT
// unset → immediate error, no planner invocation.
func TestRunAgentScriptBootstrap_EmptyGoalErrors(t *testing.T) {
	dir := t.TempDir()
	store := mustOpenStore(t, dir)
	defer store.Close()

	planner := &stubPlanner{fn: func(iter int, in PlanInput) (string, error) {
		t.Error("planner must not be called when goal is empty")
		return "", nil
	}}
	withStubPlanner(t, planner)
	withStubSupervisor(t, &stubSupervisor{fn: func(call int, in SupervisorInput) (SupervisorOutput, error) {
		t.Error("supervisor must not be called when goal is empty")
		return SupervisorOutput{}, nil
	}})

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

// TestAgentScriptCheckInterval: env handling + clamp.
func TestAgentScriptCheckInterval(t *testing.T) {
	t.Run("default 30s when unset", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "")
		if d := agentScriptCheckInterval(); d != 30*time.Second {
			t.Fatalf("got %v, want 30s", d)
		}
	})
	t.Run("clamp floor at 5s", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "1")
		if d := agentScriptCheckInterval(); d != 5*time.Second {
			t.Fatalf("got %v, want 5s (clamp)", d)
		}
	})
	t.Run("clamp ceiling at 300s", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S", "99999")
		if d := agentScriptCheckInterval(); d != 300*time.Second {
			t.Fatalf("got %v, want 300s (clamp)", d)
		}
	})
}

// TestAgentScriptMaxSupervisorTurns: env handling + clamp.
func TestAgentScriptMaxSupervisorTurns(t *testing.T) {
	t.Run("default 60 when unset", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_SUPERVISOR_TURNS", "")
		if n := agentScriptMaxSupervisorTurns(); n != 60 {
			t.Fatalf("got %d, want 60", n)
		}
	})
	t.Run("clamp ceiling at 600", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_MAX_SUPERVISOR_TURNS", "9999")
		if n := agentScriptMaxSupervisorTurns(); n != 600 {
			t.Fatalf("got %d, want 600 (clamp)", n)
		}
	})
}

// TestAgentScriptTokenBudgetUSD: env handling.
func TestAgentScriptTokenBudgetUSD(t *testing.T) {
	t.Run("default $1.00 when unset", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_TOKEN_BUDGET_USD", "")
		if f := agentScriptTokenBudgetUSD(); f != 1.00 {
			t.Fatalf("got %v, want 1.00", f)
		}
	})
	t.Run("explicit 0 disables", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_TOKEN_BUDGET_USD", "0")
		if f := agentScriptTokenBudgetUSD(); f != 0 {
			t.Fatalf("got %v, want 0", f)
		}
	})
	t.Run("invalid falls back to default", func(t *testing.T) {
		t.Setenv("AGENT_OPS_BOOTSTRAP_TOKEN_BUDGET_USD", "not-a-number")
		if f := agentScriptTokenBudgetUSD(); f != 1.00 {
			t.Fatalf("got %v, want 1.00 (default)", f)
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

func mustOpenStore(t *testing.T, dir string) *state.Store {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// overrideScriptPathForTest swaps the package-level script path for the
// duration of the test.
func overrideScriptPathForTest(t *testing.T, path string) {
	t.Helper()
	prev := testScriptPathOverride
	testScriptPathOverride = path
	t.Cleanup(func() { testScriptPathOverride = prev })
}


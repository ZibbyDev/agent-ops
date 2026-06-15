package task

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZibbyDev/agent-ops/internal/driver"
	"github.com/ZibbyDev/agent-ops/internal/runreport"
	"github.com/ZibbyDev/agent-ops/internal/state"
	"github.com/ZibbyDev/agent-ops/internal/tool"
)

// fakeDriver runs a programmable callback per invocation.
type fakeDriver struct {
	name string
	run  func(ctx context.Context, req driver.Request) (driver.Result, error)
}

func (d *fakeDriver) Name() string { return d.name }
func (d *fakeDriver) Run(ctx context.Context, req driver.Request) (driver.Result, error) {
	return d.run(ctx, req)
}

// fakeReporter captures the last RunRecord it was handed and signals via done.
// reportErr, when non-nil, is returned from Report — used to prove a reporter
// failure never fails the run.
type fakeReporter struct {
	mu        sync.Mutex
	rec       runreport.RunRecord
	called    bool
	reportErr error
	done      chan struct{}
}

func newFakeReporter(err error) *fakeReporter {
	return &fakeReporter{reportErr: err, done: make(chan struct{}, 1)}
}

func (f *fakeReporter) Report(_ context.Context, rec runreport.RunRecord) error {
	f.mu.Lock()
	f.rec = rec
	f.called = true
	f.mu.Unlock()
	select {
	case f.done <- struct{}{}:
	default:
	}
	return f.reportErr
}

func (f *fakeReporter) record() (runreport.RunRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec, f.called
}

// waitReported blocks until Report fired (the runner reports in a goroutine)
// or the deadline elapses.
func (f *fakeReporter) waitReported(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("reporter was not called within 2s")
	}
}

func openState(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRunner_HappyPath(t *testing.T) {
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(ctx context.Context, req driver.Request) (driver.Result, error) {
			// Validate that runner threaded args through.
			if req.UserPrompt == "" {
				t.Error("UserPrompt empty")
			}
			if req.MaxToolCalls == 0 {
				t.Error("MaxToolCalls 0")
			}
			if req.SystemPrompt == "" {
				t.Error("SystemPrompt empty")
			}
			// Emit a couple of log lines via the sink to test persistence.
			if req.LogSink != nil {
				_ = req.LogSink.Log(ctx, "info", "first")
				_ = req.LogSink.Log(ctx, "info", "second")
			}
			return driver.Result{FinalMessage: "all done", ToolCalls: 2}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)

	run, res, err := r.Run(context.Background(), Spec{
		Name:    "task-a",
		Trigger: TriggerManual,
		Prompt:  "do thing",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != state.StatusCompleted {
		t.Fatalf("status = %q, want completed", run.Status)
	}
	if !strings.Contains(run.Summary, "all done") {
		t.Fatalf("summary = %q", run.Summary)
	}
	if run.ToolCalls != 2 {
		t.Fatalf("ToolCalls = %d", run.ToolCalls)
	}
	if res.FinalMessage != "all done" {
		t.Fatalf("driver result not passed through: %+v", res)
	}
	logs, _ := st.LogsForRun(context.Background(), run.ID, 10)
	if len(logs) != 2 {
		t.Fatalf("expected 2 log rows, got %d", len(logs))
	}
}

func TestRunner_DriverError_MarkedFailed(t *testing.T) {
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(context.Context, driver.Request) (driver.Result, error) {
			return driver.Result{}, errors.New("boom")
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)

	run, _, err := r.Run(context.Background(), Spec{
		Name:    "task-fail",
		Trigger: TriggerManual,
		Prompt:  "doomed",
	})
	if err != nil {
		t.Fatalf("Run unexpectedly errored: %v", err)
	}
	if run.Status != state.StatusFailed {
		t.Fatalf("status = %q, want failed", run.Status)
	}
	if !strings.Contains(run.Error, "boom") {
		t.Fatalf("error not surfaced: %q", run.Error)
	}
}

func TestRunner_DriverInternalError_MarkedFailed(t *testing.T) {
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(context.Context, driver.Request) (driver.Result, error) {
			return driver.Result{Error: "max_tool_calls reached"}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)
	run, _, _ := r.Run(context.Background(), Spec{
		Name: "task-overrun", Trigger: TriggerManual, Prompt: "hi",
	})
	if run.Status != state.StatusFailed {
		t.Fatalf("status = %q, want failed", run.Status)
	}
}

func TestRunner_Timeout_MarkedTimeout(t *testing.T) {
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(ctx context.Context, req driver.Request) (driver.Result, error) {
			<-ctx.Done() // wait for runner's timeout
			return driver.Result{}, ctx.Err()
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)
	r.TaskTimeout = 50 * time.Millisecond
	run, _, _ := r.Run(context.Background(), Spec{
		Name: "task-slow", Trigger: TriggerManual, Prompt: "wait",
	})
	if run.Status != state.StatusTimeout {
		t.Fatalf("status = %q, want timeout", run.Status)
	}
}

func TestRunner_RefusesConcurrentSameTask(t *testing.T) {
	st := openState(t)
	started := make(chan struct{})
	finish := make(chan struct{})
	d := &fakeDriver{
		name: "fake",
		run: func(ctx context.Context, req driver.Request) (driver.Result, error) {
			close(started)
			<-finish
			return driver.Result{FinalMessage: "ok"}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = r.Run(context.Background(), Spec{Name: "same", Trigger: TriggerManual, Prompt: "p"})
	}()
	<-started

	_, _, err := r.Run(context.Background(), Spec{Name: "same", Trigger: TriggerManual, Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("expected already-running error, got: %v", err)
	}

	close(finish)
	wg.Wait()
}

func TestRunner_MissionPrependedToSystemPrompt(t *testing.T) {
	ctx := context.Background()
	st := openState(t)

	// Seed a mission so the runner has something to inject.
	if err := st.SetStatement(ctx, "I steward the OpenDesign instance. Always dry-run upgrades."); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddFact(ctx, "bootstrap", "OpenDesign installed via apt at /usr/local/bin/opendesign"); err != nil {
		t.Fatal(err)
	}

	var capturedSystem string
	d := &fakeDriver{
		name: "fake",
		run: func(_ context.Context, req driver.Request) (driver.Result, error) {
			capturedSystem = req.SystemPrompt
			return driver.Result{FinalMessage: "ok"}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)
	_, _, err := r.Run(ctx, Spec{Name: "weekly", Trigger: TriggerManual, Prompt: "check upstream"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capturedSystem, "I steward the OpenDesign instance") {
		t.Fatalf("system prompt missing mission statement:\n%s", capturedSystem)
	}
	if !strings.Contains(capturedSystem, "OpenDesign installed via apt") {
		t.Fatalf("system prompt missing facts:\n%s", capturedSystem)
	}
	if !strings.Contains(capturedSystem, "== MISSION ==") {
		t.Fatalf("system prompt missing MISSION header:\n%s", capturedSystem)
	}
	if !strings.Contains(capturedSystem, "== BASE GUIDANCE ==") {
		t.Fatalf("system prompt missing BASE GUIDANCE separator:\n%s", capturedSystem)
	}
}

// TestDefaultSystemPrompt_HasAutonomousModeClause pins the imperative
// "never ask the operator for clarification" wording in the baked-in system
// prompt. A regression here means the agent could revert to asking follow-up
// questions on a cron tick where no human is listening.
func TestDefaultSystemPrompt_HasAutonomousModeClause(t *testing.T) {
	required := []string{
		"running autonomously",
		"NO human in the",
		"Never ask the operator for clarification",
		"DISCOVER it via shell",
		"printenv",
		"unrecoverable",
	}
	for _, needle := range required {
		if !strings.Contains(defaultSystemPrompt, needle) {
			t.Fatalf("defaultSystemPrompt missing required clause %q", needle)
		}
	}
}

func TestRunner_NoMission_UsesBaseSystemPromptOnly(t *testing.T) {
	ctx := context.Background()
	st := openState(t)

	var capturedSystem string
	d := &fakeDriver{
		name: "fake",
		run: func(_ context.Context, req driver.Request) (driver.Result, error) {
			capturedSystem = req.SystemPrompt
			return driver.Result{FinalMessage: "ok"}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)
	_, _, err := r.Run(ctx, Spec{Name: "t", Trigger: TriggerManual, Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capturedSystem, "== MISSION ==") {
		t.Fatalf("MISSION header should not appear when no mission is set:\n%s", capturedSystem)
	}
	if !strings.Contains(capturedSystem, defaultSystemPrompt) {
		t.Fatal("base system prompt should still be present when no mission set")
	}
}

// ─── filterFactForPrompt ───────────────────────────────────────────────────

func TestFilterFactForPrompt_DropsNpmWarnDeprecated(t *testing.T) {
	in := strings.Join([]string{
		"bootstrap failed",
		"npm warn deprecated inflight@1.0.6: This module is not supported, and leaks memory",
		"npm warn deprecated rimraf@2.7.1: Rimraf versions prior to v4 are no longer supported",
		"installed 0 packages",
	}, "\n")
	out, hidden := filterFactForPrompt(in, 0)
	if hidden != 2 {
		t.Fatalf("hidden = %d, want 2", hidden)
	}
	if strings.Contains(out, "inflight") || strings.Contains(out, "rimraf") {
		t.Fatalf("dropped lines leaked into output: %s", out)
	}
	if !strings.Contains(out, "bootstrap failed") {
		t.Fatalf("non-noise line was dropped: %s", out)
	}
	if !strings.Contains(out, "installed 0 packages") {
		t.Fatalf("benign line dropped: %s", out)
	}
}

func TestFilterFactForPrompt_KeepsRealErrorsAmongWarns(t *testing.T) {
	// ERESOLVE / ENOENT / "exit code" — these are signal, not noise. They
	// often come prefixed with "npm WARN" so we MUST keep them even though
	// the line looks like an npm warn at first glance.
	in := strings.Join([]string{
		"npm warn deprecated some-old-pkg",             // drop
		"npm WARN ERESOLVE overriding peer dependency", // keep (eresolve)
		"npm warn config production not recognized",    // drop (plain npm-warn noise)
		"command failed: exit code 7",                  // keep (failed + exit code)
		"ENOENT: no such file or directory",            // keep (enoent)
	}, "\n")
	out, hidden := filterFactForPrompt(in, 3)
	if !strings.Contains(out, "ERESOLVE") {
		t.Fatalf("ERESOLVE line was dropped — keep-list broken: %s", out)
	}
	if !strings.Contains(out, "exit code 7") {
		t.Fatalf("exit-code line dropped: %s", out)
	}
	if !strings.Contains(out, "ENOENT") {
		t.Fatalf("ENOENT dropped: %s", out)
	}
	if strings.Contains(out, "some-old-pkg") {
		t.Fatalf("deprecated noise leaked: %s", out)
	}
	if strings.Contains(out, "config production") {
		t.Fatalf("plain npm-warn noise leaked: %s", out)
	}
	if hidden != 2 {
		t.Fatalf("hidden = %d, want 2", hidden)
	}
}

func TestFilterFactForPrompt_AppendsHintOnFilter(t *testing.T) {
	in := "ok line\nnpm warn deprecated foo@1.2.3: stop using this"
	out, hidden := filterFactForPrompt(in, 7)
	if hidden != 1 {
		t.Fatalf("hidden = %d, want 1", hidden)
	}
	if !strings.Contains(out, "1 lines filtered as npm-warn noise") {
		t.Fatalf("hint missing: %s", out)
	}
	if !strings.Contains(out, "fact_inspect(7)") {
		t.Fatalf("hint did not carry the index: %s", out)
	}
}

func TestFilterFactForPrompt_NoHintWhenNothingFiltered(t *testing.T) {
	in := "all good\nnothing to filter here\nexit code 0"
	out, hidden := filterFactForPrompt(in, 0)
	if hidden != 0 {
		t.Fatalf("hidden = %d, want 0", hidden)
	}
	if strings.Contains(out, "lines filtered") {
		t.Fatalf("hint should not appear when nothing was filtered: %s", out)
	}
	if strings.Contains(out, "fact_inspect") {
		t.Fatalf("fact_inspect hint should not appear: %s", out)
	}
}

func TestComposeSystemPrompt_PassesIndexThroughCorrectly(t *testing.T) {
	ctx := context.Background()
	st := openState(t)

	// Three facts: only the middle one carries npm-warn noise. After render,
	// it should be index=1 (since 0 == newest == "fact-three").
	if _, err := st.AddFact(ctx, "auto", "fact-one (oldest)"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddFact(ctx, "bootstrap",
		"middle fact had a problem\nnpm warn deprecated foo@1.0.0: drop me",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddFact(ctx, "auto", "fact-three (newest)"); err != nil {
		t.Fatal(err)
	}

	m, err := st.GetMission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := composeSystemPrompt(defaultSystemPrompt, m)

	// Only the middle fact should have the hint.
	if !strings.Contains(out, "fact_inspect(1)") {
		t.Fatalf("expected fact_inspect(1) hint on middle fact, got:\n%s", out)
	}
	if strings.Contains(out, "fact_inspect(0)") {
		t.Fatalf("newest fact should NOT have a hint: %s", out)
	}
	if strings.Contains(out, "fact_inspect(2)") {
		t.Fatalf("oldest fact should NOT have a hint: %s", out)
	}
	// And the noise line itself must not appear in the rendered prompt.
	if strings.Contains(out, "deprecated foo@1.0.0") {
		t.Fatalf("noise line leaked into prompt: %s", out)
	}
}

func TestRunner_RejectsEmptyName(t *testing.T) {
	st := openState(t)
	r := NewRunner(&fakeDriver{name: "fake", run: func(context.Context, driver.Request) (driver.Result, error) {
		return driver.Result{}, nil
	}}, tool.NewRegistry(), st)
	if _, _, err := r.Run(context.Background(), Spec{Name: "", Prompt: "x"}); err == nil {
		t.Fatal("expected error for empty name")
	}
}

// ─── RunReporter wiring ─────────────────────────────────────────────────────

func TestRunner_ReporterFiresWithCorrectFields(t *testing.T) {
	ctx := context.Background()
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(_ context.Context, _ driver.Request) (driver.Result, error) {
			return driver.Result{FinalMessage: "all done", ToolCalls: 3, CostUSDMicro: 12345}, nil
		},
	}
	rep := newFakeReporter(nil)
	r := NewRunner(d, tool.NewRegistry(), st)
	r.Reporter = rep

	_, _, err := r.Run(ctx, Spec{
		Name:    "task-report",
		Trigger: TriggerSchedule,
		Prompt:  "do the thing",
		Model:   "claude-haiku",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rep.waitReported(t)

	rec, called := rep.record()
	if !called {
		t.Fatal("reporter was not called")
	}
	if rec.RunID == "" || !strings.HasPrefix(rec.RunID, "run-") {
		t.Fatalf("RunID = %q", rec.RunID)
	}
	if rec.TaskName != "task-report" {
		t.Fatalf("TaskName = %q", rec.TaskName)
	}
	if rec.Trigger != "schedule" {
		t.Fatalf("Trigger = %q", rec.Trigger)
	}
	if rec.Status != "completed" {
		t.Fatalf("Status = %q, want completed", rec.Status)
	}
	if rec.ToolCalls != 3 {
		t.Fatalf("ToolCalls = %d", rec.ToolCalls)
	}
	if rec.NumTurns != 3 {
		t.Fatalf("NumTurns = %d, want 3 (reuses ToolCalls)", rec.NumTurns)
	}
	if rec.CostUSDMicro != 12345 {
		t.Fatalf("CostUSDMicro = %d", rec.CostUSDMicro)
	}
	if rec.Model != "claude-haiku" {
		t.Fatalf("Model = %q", rec.Model)
	}
	if rec.Result != "all done" {
		t.Fatalf("Result = %q", rec.Result)
	}
	if !strings.Contains(rec.Summary, "all done") {
		t.Fatalf("Summary = %q", rec.Summary)
	}
	if rec.UserPrompt != "do the thing" {
		t.Fatalf("UserPrompt = %q", rec.UserPrompt)
	}
	if rec.SystemPrompt == "" {
		t.Fatal("SystemPrompt empty — composed prompt should be reported")
	}
	if rec.StartedAt == "" || rec.EndedAt == "" {
		t.Fatalf("timestamps not set: started=%q ended=%q", rec.StartedAt, rec.EndedAt)
	}
	if rec.Error != "" {
		t.Fatalf("Error = %q, want empty on success", rec.Error)
	}
}

func TestRunner_NilReporter_NoError(t *testing.T) {
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(context.Context, driver.Request) (driver.Result, error) {
			return driver.Result{FinalMessage: "ok"}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st) // Reporter left nil
	run, _, err := r.Run(context.Background(), Spec{Name: "t", Trigger: TriggerManual, Prompt: "p"})
	if err != nil {
		t.Fatalf("Run with nil reporter errored: %v", err)
	}
	if run.Status != state.StatusCompleted {
		t.Fatalf("status = %q, want completed", run.Status)
	}
}

func TestRunner_ReporterError_DoesNotFailRun(t *testing.T) {
	ctx := context.Background()
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(context.Context, driver.Request) (driver.Result, error) {
			return driver.Result{FinalMessage: "ok", ToolCalls: 1}, nil
		},
	}
	rep := newFakeReporter(errors.New("backend unreachable"))
	r := NewRunner(d, tool.NewRegistry(), st)
	r.Reporter = rep

	run, res, err := r.Run(ctx, Spec{Name: "t", Trigger: TriggerManual, Prompt: "p"})
	if err != nil {
		t.Fatalf("Run returned error despite reporter failure: %v", err)
	}
	if run.Status != state.StatusCompleted {
		t.Fatalf("status = %q, want completed", run.Status)
	}
	if res.FinalMessage != "ok" {
		t.Fatalf("driver result not passed through: %+v", res)
	}
	rep.waitReported(t) // it was still attempted
}

func TestRunner_ReporterReceivesFailedStatus(t *testing.T) {
	ctx := context.Background()
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(context.Context, driver.Request) (driver.Result, error) {
			return driver.Result{}, errors.New("boom")
		},
	}
	rep := newFakeReporter(nil)
	r := NewRunner(d, tool.NewRegistry(), st)
	r.Reporter = rep

	if _, _, err := r.Run(ctx, Spec{Name: "t", Trigger: TriggerManual, Prompt: "p"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rep.waitReported(t)
	rec, _ := rep.record()
	if rec.Status != "failed" {
		t.Fatalf("Status = %q, want failed", rec.Status)
	}
	if !strings.Contains(rec.Error, "boom") {
		t.Fatalf("Error not surfaced in record: %q", rec.Error)
	}
}

// TestApplyBootstrapHouseRules pins the env-driven prepend contract used
// by the backend's buildAgentOpsContainerEnv splice. Three cases mirror
// the splice modes: explicit rules present, env unset, env whitespace-only.
//
// Table-driven so future rule-shape evolutions (e.g. a JSON envelope
// instead of free-form text) can be added without restructuring.
func TestApplyBootstrapHouseRules(t *testing.T) {
	const base = "BASE SYSTEM PROMPT"

	cases := []struct {
		name        string
		envValue    string
		envUnset    bool
		wantPrefix  string // empty => expect output == base (no prepend)
		wantUnchanged bool
	}{
		{
			name:       "rules set → prepended with blank-line separator",
			envValue:   "1. don't background installs",
			wantPrefix: "1. don't background installs\n\n",
		},
		{
			name:          "env unset → prompt unchanged",
			envUnset:      true,
			wantUnchanged: true,
		},
		{
			name:          "env empty string → prompt unchanged",
			envValue:      "",
			wantUnchanged: true,
		},
		{
			name:          "env whitespace-only → prompt unchanged",
			envValue:      "  \n\t  \n",
			wantUnchanged: true,
		},
		{
			name:       "rules surrounded by whitespace → trimmed before prepend",
			envValue:   "  \n  RULE A\n  ",
			wantPrefix: "RULE A\n\n", // TrimSpace strips outer ws but inner survives
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.envUnset {
				if err := os.Unsetenv(houseRulesEnvVar); err != nil {
					t.Fatalf("Unsetenv: %v", err)
				}
			} else {
				if err := os.Setenv(houseRulesEnvVar, c.envValue); err != nil {
					t.Fatalf("Setenv: %v", err)
				}
				t.Cleanup(func() { _ = os.Unsetenv(houseRulesEnvVar) })
			}
			got := applyBootstrapHouseRules(base)
			if c.wantUnchanged {
				if got != base {
					t.Fatalf("expected unchanged base, got %q", got)
				}
				return
			}
			if !strings.HasPrefix(got, c.wantPrefix) {
				t.Fatalf("output missing expected prefix\nwant prefix: %q\ngot:         %q", c.wantPrefix, got)
			}
			if !strings.HasSuffix(got, base) {
				t.Fatalf("output should end with base prompt; got %q", got)
			}
		})
	}
}

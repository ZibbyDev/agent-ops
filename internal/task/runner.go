// Copyright 2026 Zibby Lab. Apache-2.0.

// Package task is the bridge between a Schedule (or ad-hoc invocation) and
// the LLM Driver. It owns:
//   - assembling the system+user prompt
//   - cutting a TaskRun row in state
//   - calling the Driver
//   - persisting the final status + logs
//
// Runner.Run is goroutine-safe; concurrent runs of different tasks may proceed
// in parallel, but per-task serialization (one run at a time per Task) is
// enforced by the scheduler, not here.
package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ZibbyDev/agent-ops/internal/driver"
	"github.com/ZibbyDev/agent-ops/internal/runreport"
	"github.com/ZibbyDev/agent-ops/internal/state"
	"github.com/ZibbyDev/agent-ops/internal/tool"
)

// Runner ties together driver, tool registry, and state store.
type Runner struct {
	Driver       driver.Driver
	Tools        *tool.Registry
	State        *state.Store
	MaxToolCalls int
	TaskTimeout  time.Duration
	SystemPrompt string

	// Reporter, when non-nil, receives a structured record of each finished
	// run (fire-and-forget). Optional dep — a nil Reporter skips reporting.
	// Set from main.go with an HTTP reporter pointed at the control plane.
	Reporter runreport.RunReporter

	// In-flight protects against duplicate concurrent runs of the same task
	// (e.g. a slow run still going when the next cron tick fires). v0.1
	// policy: skip the new tick + log "skipped".
	mu       sync.Mutex
	inflight map[string]struct{}
}

// NewRunner returns a Runner with sensible defaults.
func NewRunner(d driver.Driver, tools *tool.Registry, st *state.Store) *Runner {
	return &Runner{
		Driver:       d,
		Tools:        tools,
		State:        st,
		MaxToolCalls: 25,
		TaskTimeout:  10 * time.Minute,
		SystemPrompt: defaultSystemPrompt,
		inflight:     map[string]struct{}{},
	}
}

const defaultSystemPrompt = `You are agent-ops, an autonomous DevOps operator running on a single
host. You receive instructions and have a small set of tools (shell, etc.)
to satisfy them. Be concise. Prefer read-only inspection before mutating.
When a tool fails, recover or report — do not retry blindly. End the run
when the user instruction is satisfied OR when you genuinely cannot make
further progress; explain either outcome in plain English.
If a KNOWN FACT shows "(N lines filtered as npm-warn noise — call fact_inspect(<index>) to see full)", call that tool only when a fact's exit code or error message warrants deeper inspection; routine npm-warn-only facts can be ignored.

You are running autonomously on a scheduled cron tick. There is NO human in the
loop to answer follow-up questions. Never ask the operator for clarification.
If you need a value (instance id, app type, port, process name, config path),
DISCOVER it via shell — ` + "`printenv`" + `, ` + "`ps`" + `, ` + "`ss -tlnp`" + `, ` + "`cat /etc/agent-ops/...`" + `,
` + "`curl 127.0.0.1:<port>`" + ` — then act. Only call the notification tool (if one
is configured — see USER PROMPT) when you have ACTED and the issue is
unrecoverable.`

// composeSystemPrompt prepends the instance Mission (charter + known facts)
// to the runner's base system prompt. This is what gives the agent its
// "I know who I am and what I've done" sense across restarts — every task
// run reads the current Mission so the prompt is always fresh.
//
// Format kept human-readable so a sysadmin debugging an agent run can paste
// the exact prompt into a Claude UI and reproduce the behavior.
func composeSystemPrompt(base string, m state.Mission) string {
	if m.Statement == "" && len(m.Facts) == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString("You are agent-ops stewarding one specific instance.\n\n")
	if m.Statement != "" {
		b.WriteString("== MISSION ==\n")
		b.WriteString(m.Statement)
		b.WriteString("\n\n")
	}
	if len(m.Facts) > 0 {
		b.WriteString("== KNOWN FACTS (oldest first) ==\n")
		// Trim to last 40 facts in-prompt — full list lives in DB; this
		// keeps the prompt context-bounded for cheaper models too.
		facts := m.Facts
		if len(facts) > 40 {
			facts = facts[len(facts)-40:]
		}
		// `index` is the recent-facts index from the model's POV:
		// 0 == most recent (== last element of `facts`). This matches the
		// addressing scheme of the `fact_inspect` MCP tool, which subtracts
		// from len(Facts)-1 so the model can call it with the hint's number.
		for i, f := range facts {
			index := len(facts) - 1 - i
			filtered, hidden := filterFactForPrompt(f.Fact, index)
			b.WriteString("- [")
			b.WriteString(f.TS.Format("2006-01-02"))
			b.WriteString("] (")
			b.WriteString(f.Source)
			b.WriteString(") ")
			b.WriteString(filtered)
			b.WriteString("\n")
			_ = hidden // count surfaced inline by filterFactForPrompt's synthetic line
		}
		b.WriteString("\n")
	}
	b.WriteString("== BASE GUIDANCE ==\n")
	b.WriteString(base)
	return b.String()
}

// houseRulesEnvVar is the env key the operator/control-plane sets to
// PREPEND a free-form rules block to the system prompt. The intent is
// to let the BACKEND (which builds the container env at task-def time)
// iterate prompt language without forcing an agent-ops version bump +
// ECR rebuild every time. Empty / whitespace-only → no change.
//
// Lives at runner-level because both bootstrap modes (goal + cheatsheet)
// route through Runner.Run → composeSystemPrompt; injecting here is a
// single chokepoint and the env is only populated by buildAgentOpsContainerEnv
// for the bootstrap container, so no surprises for non-Apps deployments.
const houseRulesEnvVar = "AGENT_OPS_BOOTSTRAP_SYSTEM_RULES"

// applyBootstrapHouseRules prepends $AGENT_OPS_BOOTSTRAP_SYSTEM_RULES to
// systemPrompt when the env var is set to a non-whitespace value. Logs a
// one-line INFO record with the byte count so operators can confirm the
// rules were honored from CloudWatch without re-spawning a container.
func applyBootstrapHouseRules(systemPrompt string) string {
	raw, _ := os.LookupEnv(houseRulesEnvVar)
	rules := strings.TrimSpace(raw)
	if rules == "" {
		return systemPrompt
	}
	slog.Info("runner: prepending bootstrap house rules",
		"env", houseRulesEnvVar,
		"rules_bytes", len(rules),
	)
	return rules + "\n\n" + systemPrompt
}

// Trigger is the entry point — schedule | manual | bootstrap. Always returns
// the run id (even on failure) so callers can subscribe to logs.
type Trigger string

const (
	TriggerSchedule  Trigger = "schedule"
	TriggerManual    Trigger = "manual"
	TriggerBootstrap Trigger = "bootstrap"
)

// Spec describes one task invocation.
type Spec struct {
	Name    string   // Task name; "ad-hoc" if not from a Schedule
	Trigger Trigger  // why we're running
	Prompt  string   // user prompt
	Tools   []string // tool allowlist; nil/empty → all
	// Model overrides the driver's default model for THIS task (cost
	// lever — Haiku for routine cron, Sonnet for install/upgrade/
	// incident-response). Empty → driver default applies.
	Model string
}

// Run executes one task. Returns the persisted TaskRun (terminal state) and
// the driver's Result for callers that want the assistant's final message
// inline (e.g. MCP synchronous run_now).
func (r *Runner) Run(ctx context.Context, spec Spec) (state.TaskRun, driver.Result, error) {
	if spec.Name == "" {
		return state.TaskRun{}, driver.Result{}, errors.New("task.Run: spec.Name required")
	}
	if spec.Prompt == "" {
		return state.TaskRun{}, driver.Result{}, errors.New("task.Run: spec.Prompt required")
	}

	// Single-flight per task: skip if a run is already in-flight.
	if !r.markInflight(spec.Name) {
		return state.TaskRun{}, driver.Result{}, fmt.Errorf("task.Run: %q is already running, skipping", spec.Name)
	}
	defer r.releaseInflight(spec.Name)

	runID, err := generateRunID()
	if err != nil {
		return state.TaskRun{}, driver.Result{}, err
	}
	run := state.TaskRun{
		ID:        runID,
		TaskName:  spec.Name,
		Trigger:   string(spec.Trigger),
		StartedAt: time.Now().UTC(),
	}
	if err := r.State.StartRun(ctx, run); err != nil {
		return state.TaskRun{}, driver.Result{}, fmt.Errorf("task.Run: start: %w", err)
	}

	cctx, cancel := context.WithTimeout(ctx, r.TaskTimeout)
	defer cancel()

	sink := &runLogSink{state: r.State, runID: runID}

	// Read mission fresh on every run — user / earlier ticks may have changed
	// it. Failures here degrade gracefully: missing mission == no extra
	// prompt context, not a fatal run failure.
	mission, missionErr := r.State.GetMission(ctx)
	if missionErr != nil {
		_ = sink.Log(ctx, "warn", "could not read instance mission: "+missionErr.Error())
	}
	systemPrompt := applyBootstrapHouseRules(composeSystemPrompt(r.SystemPrompt, mission))

	dReq := driver.Request{
		SystemPrompt: systemPrompt,
		UserPrompt:   spec.Prompt,
		Tools:        r.Tools.Subset(spec.Tools),
		MaxToolCalls: r.MaxToolCalls,
		Model:        spec.Model,
		LogSink:      sink,
	}

	dRes, dErr := r.Driver.Run(cctx, dReq)

	finalStatus := state.StatusCompleted
	summary := truncate(dRes.FinalMessage, 2000)
	errMsg := ""
	// Order matters: context-derived states beat driver errors, because a
	// driver call that returned `ctx.Err()` is really a timeout/cancel, not
	// a bug. Cancellation likewise outranks "ran-but-failed".
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		finalStatus = state.StatusTimeout
		errMsg = "task exceeded configured timeout"
	case ctx.Err() == context.Canceled || cctx.Err() == context.Canceled:
		finalStatus = state.StatusCanceled
		errMsg = "task canceled"
	case dErr != nil:
		finalStatus = state.StatusFailed
		errMsg = dErr.Error()
	case dRes.Error != "":
		// Driver itself reported a partial-failure (e.g. max_tool_calls).
		finalStatus = state.StatusFailed
		errMsg = dRes.Error
	}

	// Finish in a fresh context — the parent may be canceled, but we still
	// want to persist the terminal status.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	if err := r.State.FinishRun(finishCtx, runID, finalStatus, summary, errMsg, dRes.ToolCalls); err != nil {
		return run, dRes, fmt.Errorf("task.Run: finish: %w", err)
	}

	// Report the finished run to the control plane (fire-and-forget). The
	// terminal state is already durably persisted above, so reporting can run
	// async in its own goroutine + context — a slow or failing report must
	// never block or fail the run.
	if r.Reporter != nil {
		runEndedAt := time.Now().UTC()
		rec := runreport.RunRecord{
			RunID:     runID,
			TaskName:  spec.Name,
			Trigger:   string(spec.Trigger),
			Status:    string(finalStatus),
			StartedAt: run.StartedAt.Format(time.RFC3339Nano),
			EndedAt:   runEndedAt.Format(time.RFC3339Nano),
			ToolCalls: dRes.ToolCalls,
			// driver.Result has no separate turn count, so reuse ToolCalls as
			// numTurns (each tool-use iteration is ~one assistant turn).
			NumTurns:     dRes.ToolCalls,
			CostUSDMicro: dRes.CostUSDMicro,
			Model:        spec.Model,
			SystemPrompt: systemPrompt,
			UserPrompt:   spec.Prompt,
			Result:       dRes.FinalMessage,
			Summary:      summary,
			Error:        errMsg,
		}
		go func() {
			repCtx, repCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer repCancel()
			_ = r.Reporter.Report(repCtx, rec)
		}()
	}

	out, _ := r.State.GetRun(finishCtx, runID)
	return out, dRes, nil
}

func (r *Runner) markInflight(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.inflight[name]; busy {
		return false
	}
	r.inflight[name] = struct{}{}
	return true
}

func (r *Runner) releaseInflight(name string) {
	r.mu.Lock()
	delete(r.inflight, name)
	r.mu.Unlock()
}

// runLogSink writes one log row per agent-loop event for later replay.
type runLogSink struct {
	state *state.Store
	runID string

	mu  sync.Mutex
	seq int
}

func (s *runLogSink) Log(ctx context.Context, level, message string) error {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	return s.state.AppendRunLog(ctx, s.runID, seq, level, message)
}

func generateRunID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "run-" + hex.EncodeToString(buf), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// filterFactForPrompt strips routine npm-warn noise from a fact's text before
// it lands in the system prompt. Lines matching the drop-list are removed
// UNLESS they also match the keep-list (so `npm WARN ERESOLVE …` survives).
//
// `index` is the position of this fact in the recent-facts list as the model
// sees it: 0 == most recent. It's surfaced in a synthetic trailing line so
// the agent can call `fact_inspect(<index>)` to retrieve the unfiltered text
// when something looks suspicious.
//
// On-disk facts are untouched — this is render-only.
func filterFactForPrompt(s string, index int) (filtered string, hiddenLines int) {
	// Keep-list wins. Real errors must survive even when wrapped in an
	// npm-warn-ish line prefix.
	keepNeedles := []string{
		"error", "failed", "fatal", "eresolve", "enoent", "eaccess",
		"etimedout", "exit code", "exit_code", "exit status",
	}
	// Drop-list: lines starting (after trimming leading whitespace) with
	// these prefixes are noise. Case-insensitive.
	dropPrefixes := []string{
		"npm warn deprecated",
		"npm warn ",
		"npm WARN", // belt + suspenders for mixed-case logs; ToLower covers it
	}

	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	hidden := 0
	for _, ln := range lines {
		lower := strings.ToLower(strings.TrimLeft(ln, " \t"))
		isKeep := false
		for _, k := range keepNeedles {
			if strings.Contains(lower, k) {
				isKeep = true
				break
			}
		}
		if isKeep {
			kept = append(kept, ln)
			continue
		}
		isDrop := false
		for _, p := range dropPrefixes {
			if strings.HasPrefix(lower, strings.ToLower(p)) {
				isDrop = true
				break
			}
		}
		if isDrop {
			hidden++
			continue
		}
		kept = append(kept, ln)
	}
	out := strings.Join(kept, "\n")
	if hidden > 0 {
		hint := fmt.Sprintf("(%d lines filtered as npm-warn noise — call `fact_inspect(%d)` to see full)", hidden, index)
		if out == "" {
			out = hint
		} else {
			out = out + "\n" + hint
		}
	}
	return out, hidden
}

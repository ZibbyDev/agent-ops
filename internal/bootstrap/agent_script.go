// Copyright 2026 Zibby Lab. Apache-2.0.

// agent_script.go — bootstrap mode #5: supervised plan/supervise/intervene.
//
// PHASES
//
//   Phase 1 (PLAN, unchanged) — claudecli subprocess writes /tmp/install.sh.
//     Tools: Write,Read (NO Bash). Returns the script body for audit; runner
//     reads the actual file off disk.
//
//   Phase 2 (SUPERVISE, new in 0.3.9) — agent-ops execs the script in the
//     background, redirects combined stdout+stderr to a live tail file, and
//     periodically invokes Claude (the supervisor) with a snapshot of the
//     tail + process status. Supervisor emits a single JSON verdict:
//        {"verdict":"continue","note":"..."}  (keep watching)
//        {"verdict":"done","note":"..."}      (success — script finished
//                                              AND app appears to be up)
//        {"verdict":"intervene","reason":"...","note":"..."}
//                                             (kill the bash pgroup and
//                                              go back to Phase 1 with the
//                                              failure context)
//
//   Phase 3 (FINALIZE) — write final-status.json with success/iterations/
//     supervisor_turns/cost.
//
// WHY (vs. the old plan-and-walk-away path)
//
// Pre-0.3.9 the runner blocked on `bash /tmp/install.sh` and only saw the
// outcome AFTER exit. Real operators don't walk away — they tail the log,
// spot OOM/ENOSPC/hang signals as they happen, and intervene mid-install
// rather than retrying from a half-built node_modules. The supervised
// loop gives the agent the same observation/intervention loop.
//
// State files (under <stateDir>/agent_script-state/):
//
//   iteration                          — current attempt number (text int)
//   script-v<N>.sh                     — every script the planner wrote
//   script-v<N>.live.log               — combined stdout+stderr for run N
//   script-v<N>.supervisor-turns.log   — JSONL of each supervisor call
//   final-status.json                  — terminal summary
//
// ENV VARS
//
//   AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS       (default 5, clamp [1,10])
//   AGENT_OPS_BOOTSTRAP_VERIFY_PORT          (hint to supervisor only)
//   AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S     (default 30)
//   AGENT_OPS_BOOTSTRAP_MAX_SUPERVISOR_TURNS (default 60 per iter)
//   AGENT_OPS_BOOTSTRAP_TOKEN_BUDGET_USD     (default 1.00)
//   AGENT_OPS_BOOTSTRAP_SUPERVISOR_MODEL     (default = planner model)

package bootstrap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/config"
	"github.com/ZibbyHQ/agent-ops/internal/driver"
	"github.com/ZibbyHQ/agent-ops/internal/driver/claudecli"
	"github.com/ZibbyHQ/agent-ops/internal/state"
)

// agentScriptModeName is the AGENT_OPS_BOOTSTRAP_MODE value that selects
// this path. Kept as a constant so tests + backend env-splice both refer
// to the same string.
const agentScriptModeName = "agent_script"

// defaultMaxIterations bounds the plan→supervise→retry loop. 5 is enough to
// absorb 1-2 typo-grade failures + a real env-mismatch retry; beyond that
// the model is probably stuck in a loop and we should fail loud.
const defaultMaxIterations = 5

// phase1MaxTurns caps the planner subprocess. Phase 1 just writes a file;
// 8 turns is generous.
const phase1MaxTurns = 8

// defaultCheckIntervalS is how often the supervisor wakes up to look at
// the live log. 30s balances "intervene fast on a stuck process" against
// "$0.005 per supervisor call × N apps × N iterations".
const defaultCheckIntervalS = 30

// defaultMaxSupervisorTurns caps a single iteration's supervisor loop.
// 60 × 30s = 30min — matches the parent ctx default (cfg.Agent.TaskTimeout
// 30m), so the per-iter cap never strictly dominates the outer cap.
const defaultMaxSupervisorTurns = 60

// defaultTokenBudgetUSD caps total Claude spend across planner +
// supervisor calls for one bootstrap run. $1.00 = roughly 200 supervisor
// calls at Sonnet 4.6 rates, well above any realistic single-app install.
// On budget exceed we still let the in-flight iteration finish but refuse
// to start new ones.
const defaultTokenBudgetUSD = 1.00

// stuckThresholdSeconds — if the live log hasn't grown in this long and
// the process is still RUNNING, the supervisor prompt nudges the model to
// consider 'intervene'. Operator analogue: "if the npm install has shown
// no output for 3 minutes it's stuck".
const stuckThresholdSeconds = 180

// defaultScriptPath is where the planner writes + the runner reads in
// production. Kept stable across iterations so the planner's prompt
// always names the same path. Tests override via testScriptPathOverride.
const defaultScriptPath = "/tmp/install.sh"

// liveTailBytes is how many bytes of the tail we feed the supervisor each
// call. 2KB is enough for ~30 lines of npm output and stays well under
// the per-event CloudWatch budget when the call is logged.
const liveTailBytes = 2048

// testScriptPathOverride lets unit tests redirect the script path to a
// per-test tempdir so parallel runs don't clobber a shared /tmp file.
var testScriptPathOverride string

func scriptPathFor() string {
	if testScriptPathOverride != "" {
		return testScriptPathOverride
	}
	return defaultScriptPath
}

// isAgentScriptMode reports whether AGENT_OPS_BOOTSTRAP_MODE=agent_script.
// Case-insensitive to match the cheatsheet/script mode probes.
func isAgentScriptMode() bool {
	return strings.EqualFold(
		strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_MODE")),
		agentScriptModeName,
	)
}

// ─── Planner (Phase 1, unchanged in spirit) ────────────────────────────

// Planner is the Phase 1 abstraction. Implementations MUST write the
// chosen bash script to PlanInput.ScriptPath.
type Planner interface {
	Plan(ctx context.Context, in PlanInput) (PlanOutput, error)
}

// PlanInput is what Phase 1 receives per iteration.
type PlanInput struct {
	Goal         string // user's deploy goal (AGENT_OPS_BOOTSTRAP_PROMPT)
	Model        string // optional model override (AGENT_OPS_BOOTSTRAP_MODEL)
	HouseRules   string // AGENT_OPS_BOOTSTRAP_SYSTEM_RULES verbatim
	Iteration    int    // 1-indexed; > 1 means PreviousStderr/Stdout are set
	PrevStderr   string // last ~2KB of previous attempt's combined output
	PrevStdout   string // (kept separate field for back-compat with tests)
	PrevExitCode int    // exit code of previous attempt (iter > 1)
	PrevReason   string // supervisor's intervene-reason for iter > 1, if any
	ScriptPath   string // absolute path Claude must write the script to
	VerifyPort   int    // 0 = none; >0 = a hint passed to the planner prompt
}

// PlanOutput is what Phase 1 returns. RawScript is the planner's view of
// the file it wrote — informational for state-file audit; the runner
// reads the actual script from disk regardless.
type PlanOutput struct {
	RawScript    string // what the planner believes it wrote (audit only)
	CostUSDMicro int64  // for cost accounting
	NumTurns     int    // for diagnostics
}

// claudecliPlanner is the production Planner.
type claudecliPlanner struct {
	// Driver is the claudecli driver to call. Constructor pins
	// AllowedTools="Write,Read" — DO NOT rebuild this driver with Bash
	// in the allow-list; that reintroduces the auto-background bug
	// this whole mode exists to fix.
	Driver *claudecli.Driver
}

// newClaudecliPlanner constructs the production planner.
func newClaudecliPlanner(model string) *claudecliPlanner {
	return &claudecliPlanner{
		Driver: &claudecli.Driver{
			Model: model,
			// "Write,Read" — NO Bash, NO Edit. The planner writes a script
			// to disk and that's it. Adding Bash here is a regression.
			AllowedTools:   "Write,Read",
			PermissionMode: "acceptEdits",
		},
	}
}

// Plan implements Planner against the real claudecli driver.
func (p *claudecliPlanner) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	system, user := buildPlannerPrompts(in)
	res, err := p.Driver.Run(ctx, driver.Request{
		SystemPrompt: system,
		UserPrompt:   user,
		MaxToolCalls: phase1MaxTurns,
		Model:        in.Model,
	})
	if err != nil {
		return PlanOutput{}, fmt.Errorf("agent_script: planner subprocess: %w", err)
	}
	if res.Error != "" {
		return PlanOutput{
			CostUSDMicro: res.CostUSDMicro,
			NumTurns:     res.ToolCalls,
		}, fmt.Errorf("agent_script: planner CLI error: %s", res.Error)
	}
	return PlanOutput{
		RawScript:    res.FinalMessage,
		CostUSDMicro: res.CostUSDMicro,
		NumTurns:     res.ToolCalls,
	}, nil
}

// activePlanner is the package-level seam tests use to substitute a stub.
var activePlanner Planner

// ─── Supervisor (Phase 2, new in 0.3.9) ────────────────────────────────

// SupervisorVerdict is the parsed output of one supervisor call.
type SupervisorVerdict struct {
	Verdict string `json:"verdict"` // "continue" | "done" | "intervene"
	Note    string `json:"note"`    // one-line user-facing progress
	Reason  string `json:"reason"`  // intervene only — root cause
}

// SupervisorInput is what the supervisor sees on every poll.
type SupervisorInput struct {
	Goal            string
	IterationNum    int
	ScriptBody      string        // the v<N> script being watched
	IterStart       time.Time     // when this iteration's bash started
	SupervisorTurn  int           // 1-indexed
	ProcStatus      string        // RUNNING | EXITED | ZOMBIE | KILLED
	ExitCode        *int          // nil while still running
	TailBytes       string        // last ~2KB of combined stdout+stderr
	BytesSinceWake  int64         // how many new bytes since previous poll
	IdleDuration    time.Duration // wall-clock since the live log last grew
	VerifyPortHint  int           // catalog hint — NOT enforced; supervisor decides
	Model           string        // model the supervisor should use
}

// SupervisorOutput captures both the parsed verdict + cost info.
type SupervisorOutput struct {
	Verdict      SupervisorVerdict
	Raw          string // raw text reply (for the JSONL audit)
	CostUSDMicro int64
	ParseError   string // non-empty when Verdict is the malformed-JSON fallback
}

// Supervisor is the test seam for Phase 2.
type Supervisor interface {
	Check(ctx context.Context, in SupervisorInput) (SupervisorOutput, error)
}

// claudecliSupervisor calls claudecli with NO write/edit/bash tools and
// asks for a JSON verdict. Reusing claudecli (vs. a Messages-API driver)
// keeps the OAuth-token path consistent with the planner so one set of
// auth diagnostics covers both phases.
type claudecliSupervisor struct {
	Driver *claudecli.Driver
}

// newClaudecliSupervisor constructs the production supervisor.
//
// AllowedTools="Read": the supervisor doesn't NEED any tool to emit JSON,
// but claudecli falls back to "Bash,Read,Write,Edit" when the field is
// empty. Picking the smallest read-only allowlist disables every
// side-effect-having tool (Bash/Write/Edit) without depending on changes
// to claudecli. Tests guard the invariant — see
// TestSupervisorAllowedToolsHasNoBashWriteOrEdit.
func newClaudecliSupervisor(model string) *claudecliSupervisor {
	return &claudecliSupervisor{
		Driver: &claudecli.Driver{
			Model:          model,
			AllowedTools:   "Read",
			PermissionMode: "acceptEdits",
		},
	}
}

// Check implements Supervisor against the real claudecli driver.
func (s *claudecliSupervisor) Check(ctx context.Context, in SupervisorInput) (SupervisorOutput, error) {
	system, user := buildSupervisorPrompts(in)
	res, err := s.Driver.Run(ctx, driver.Request{
		SystemPrompt: system,
		UserPrompt:   user,
		// 2 turns is plenty — the supervisor should emit ONE JSON line.
		// We cap above zero so the CLI's auto-default (25) doesn't burn
		// budget if the model decides to thrash.
		MaxToolCalls: 2,
		Model:        in.Model,
	})
	if err != nil {
		return SupervisorOutput{}, fmt.Errorf("agent_script: supervisor subprocess: %w", err)
	}
	if res.Error != "" {
		// Treat CLI-level errors as a transient "continue" — the loop
		// keeps watching the live log and retries on the next interval.
		// Surface the raw error in the audit log so operators can
		// distinguish CLI flakes from real planner failures.
		return SupervisorOutput{
			Verdict:      SupervisorVerdict{Verdict: "continue", Note: "supervisor CLI error, will retry: " + truncate(res.Error, 200)},
			Raw:          res.FinalMessage,
			CostUSDMicro: res.CostUSDMicro,
			ParseError:   res.Error,
		}, nil
	}
	verdict, parseErr := parseSupervisorVerdict(res.FinalMessage)
	out := SupervisorOutput{
		Verdict:      verdict,
		Raw:          res.FinalMessage,
		CostUSDMicro: res.CostUSDMicro,
	}
	if parseErr != nil {
		out.ParseError = parseErr.Error()
	}
	return out, nil
}

// activeSupervisor is the package-level seam tests use to substitute a stub.
var activeSupervisor Supervisor

// ─── Top-level orchestration ───────────────────────────────────────────

// runAgentScriptBootstrap is the entry point invoked by MaybeRunFirstRun
// when AGENT_OPS_BOOTSTRAP_MODE=agent_script. Loops:
//
//	Phase 1: planner writes /tmp/install.sh
//	Phase 2: supervised exec — bash runs in background while a Claude
//	         supervisor polls the live log every CHECK_INTERVAL_S and
//	         emits continue/done/intervene
//	Phase 3 (on intervene): record outcome, kill pgroup, retry from
//	         Phase 1 — up to MaxIterations
//
// Returns nil on supervisor "done", error on intervention exhaustion or
// budget exceed.
func runAgentScriptBootstrap(
	ctx context.Context,
	cfg *config.Config,
	store *state.Store,
) error {
	goal := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_PROMPT"))
	if goal == "" {
		return errors.New("agent_script: AGENT_OPS_BOOTSTRAP_PROMPT is empty")
	}
	houseRules := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_SYSTEM_RULES"))
	plannerModel := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_MODEL"))
	supervisorModel := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_SUPERVISOR_MODEL"))
	if supervisorModel == "" {
		supervisorModel = plannerModel
	}
	maxIter := agentScriptMaxIterations()
	verifyPort := agentScriptVerifyPort()
	checkInterval := agentScriptCheckInterval()
	maxSupervisorTurns := agentScriptMaxSupervisorTurns()
	budgetUSD := agentScriptTokenBudgetUSD()
	budgetMicro := int64(budgetUSD * 1_000_000)

	auditDir := filepath.Join(cfg.StateDir, "agent_script-state")
	if err := os.MkdirAll(auditDir, 0o700); err != nil {
		return fmt.Errorf("agent_script: mkdir audit dir: %w", err)
	}

	planner := activePlanner
	if planner == nil {
		planner = newClaudecliPlanner(plannerModel)
	}
	supervisor := activeSupervisor
	if supervisor == nil {
		supervisor = newClaudecliSupervisor(supervisorModel)
	}

	slog.Info("agent_script: starting supervised plan/supervise loop",
		"goal_bytes", len(goal),
		"max_iterations", maxIter,
		"verify_port_hint", verifyPort,
		"check_interval_s", int(checkInterval/time.Second),
		"max_supervisor_turns", maxSupervisorTurns,
		"budget_usd", budgetUSD,
		"planner_model", plannerModel,
		"supervisor_model", supervisorModel,
		"audit_dir", auditDir,
	)

	var (
		prevCombined       string
		prevExit           int
		prevReason         string
		totalCostMicro     int64
		totalSupervisorTrn int
		lastError          error
		finalVerdict       string
	)
	scrPath := scriptPathFor()

	for iter := 1; iter <= maxIter; iter++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("agent_script: context cancelled at iteration %d: %w",
				iter, ctxErr)
		}

		// Budget gate — refuse to start a new iteration if we've already
		// exceeded the budget. Already-in-flight iterations finish.
		if budgetMicro > 0 && totalCostMicro >= budgetMicro {
			slog.Warn("agent_script: token budget exceeded, refusing new iteration",
				"total_cost_usd_micro", totalCostMicro,
				"budget_usd_micro", budgetMicro,
				"iteration_skipped", iter,
			)
			lastError = fmt.Errorf("token budget $%.4f exceeded at iteration %d",
				budgetUSD, iter)
			break
		}

		_ = os.WriteFile(filepath.Join(auditDir, "iteration"),
			[]byte(strconv.Itoa(iter)), 0o600)
		slog.Info("agent_script: iteration",
			"iteration", iter, "max", maxIter, "phase", "plan")

		// Remove any stale script from a previous iteration.
		_ = os.Remove(scrPath)

		planOut, planErr := planner.Plan(ctx, PlanInput{
			Goal:         goal,
			Model:        plannerModel,
			HouseRules:   houseRules,
			Iteration:    iter,
			PrevStderr:   prevCombined, // back-compat: tests assert on PrevStderr
			PrevStdout:   "",           // we now combine streams; keep stdout slot empty
			PrevExitCode: prevExit,
			PrevReason:   prevReason,
			ScriptPath:   scrPath,
			VerifyPort:   verifyPort,
		})
		totalCostMicro += planOut.CostUSDMicro
		if planErr != nil {
			slog.Warn("agent_script: planner failed",
				"iteration", iter, "err", planErr.Error())
			lastError = planErr
			prevCombined = "planner failed: " + planErr.Error()
			prevExit = -1
			prevReason = "planner subprocess failure"
			continue
		}

		scriptBytes, readErr := os.ReadFile(scrPath)
		if readErr != nil {
			slog.Warn("agent_script: planner did not write script file",
				"iteration", iter, "path", scrPath, "err", readErr.Error())
			lastError = fmt.Errorf("planner did not write %s: %w", scrPath, readErr)
			prevCombined = "planner did not write " + scrPath + ": " + readErr.Error()
			prevExit = -1
			prevReason = "planner emitted no script file"
			continue
		}

		// Audit copy of every version.
		_ = os.WriteFile(filepath.Join(auditDir,
			fmt.Sprintf("script-v%d.sh", iter)), scriptBytes, 0o600)

		slog.Info("agent_script: iteration",
			"iteration", iter, "max", maxIter, "phase", "supervise",
			"script_bytes", len(scriptBytes),
			"planner_cost_usd_micro", planOut.CostUSDMicro,
			"planner_turns", planOut.NumTurns,
		)

		// Supervise the script. superviseScript returns:
		//   - verdict "done" with nil err on success
		//   - verdict "intervene" with non-nil err when the supervisor
		//     decided to abort (also returns combined-tail + exit code
		//     via outparams)
		//   - non-nil err on operational failure (couldn't start bash,
		//     budget mid-iter, etc.)
		supervisorRes, supErr := superviseScript(ctx, superviseConfig{
			Iteration:          iter,
			AuditDir:           auditDir,
			ScriptPath:         scrPath,
			ScriptBody:         string(scriptBytes),
			Goal:               goal,
			Model:              supervisorModel,
			Supervisor:         supervisor,
			CheckInterval:      checkInterval,
			MaxSupervisorTurns: maxSupervisorTurns,
			VerifyPortHint:     verifyPort,
			BudgetRemainingFn: func() int64 {
				if budgetMicro <= 0 {
					return -1 // disabled
				}
				return budgetMicro - totalCostMicro
			},
		})
		totalCostMicro += supervisorRes.CostUSDMicro
		totalSupervisorTrn += supervisorRes.SupervisorTurns
		finalVerdict = supervisorRes.FinalVerdict

		_, _ = store.AddFact(ctx, "agent_script",
			fmt.Sprintf("iter=%d verdict=%s exit=%d at %s; tail: %s",
				iter, supervisorRes.FinalVerdict, supervisorRes.ExitCode,
				time.Now().UTC().Format(time.RFC3339),
				truncate(strings.TrimSpace(supervisorRes.TailCombined), 400)))

		if supErr == nil && supervisorRes.FinalVerdict == "done" {
			// Success.
			slog.Info("agent_script: success",
				"iteration", iter,
				"exit_code", supervisorRes.ExitCode,
				"supervisor_turns", supervisorRes.SupervisorTurns,
				"final_note", supervisorRes.FinalNote,
				"total_cost_usd_micro", totalCostMicro,
			)
			writeFinalStatusV2(auditDir, finalStatusV2{
				Success:         true,
				Iterations:      iter,
				SupervisorTurns: totalSupervisorTrn,
				TotalCostUSD:    float64(totalCostMicro) / 1_000_000,
				FinalVerdict:    "done",
				FinalNote:       supervisorRes.FinalNote,
			})
			return nil
		}

		// Either supervisor returned "intervene" or some operational
		// failure occurred. Either way set up the next iteration's
		// retry context with the tail + reason.
		if supErr != nil {
			slog.Warn("agent_script: supervise loop returned operational error",
				"iteration", iter, "err", supErr.Error())
			lastError = supErr
		}
		slog.Warn("agent_script: iteration interrupted",
			"iteration", iter,
			"final_verdict", supervisorRes.FinalVerdict,
			"reason", supervisorRes.InterveneReason,
			"exit_code", supervisorRes.ExitCode,
			"supervisor_turns", supervisorRes.SupervisorTurns,
			"tail_tail", truncate(supervisorRes.TailCombined, 400),
		)
		prevCombined = supervisorRes.TailCombined
		prevExit = supervisorRes.ExitCode
		prevReason = supervisorRes.InterveneReason
		if lastError == nil {
			lastError = fmt.Errorf("iter %d ended with verdict=%s: %s",
				iter, supervisorRes.FinalVerdict, supervisorRes.InterveneReason)
		}
	}

	writeFinalStatusV2(auditDir, finalStatusV2{
		Success:         false,
		Iterations:      maxIter,
		SupervisorTurns: totalSupervisorTrn,
		TotalCostUSD:    float64(totalCostMicro) / 1_000_000,
		FinalVerdict:    finalVerdict,
		FinalNote:       "",
	})
	if lastError == nil {
		lastError = fmt.Errorf("agent_script: %d iterations exhausted with no success", maxIter)
	}
	return fmt.Errorf("agent_script: bootstrap failed after %d iterations: %w",
		maxIter, lastError)
}

// ─── superviseScript: Phase 2 inner loop ───────────────────────────────

// superviseConfig is the per-iteration inputs to superviseScript.
type superviseConfig struct {
	Iteration          int
	AuditDir           string
	ScriptPath         string
	ScriptBody         string
	Goal               string
	Model              string
	Supervisor         Supervisor
	CheckInterval      time.Duration
	MaxSupervisorTurns int
	VerifyPortHint     int
	// BudgetRemainingFn returns the remaining budget in micro-USD, or -1
	// when budget tracking is disabled. The supervise loop calls this
	// before each Claude poll; <=0 → stop polling (intervene with
	// "budget exhausted").
	BudgetRemainingFn func() int64
}

// superviseResult is what superviseScript returns to the outer loop.
type superviseResult struct {
	FinalVerdict    string // "done" | "intervene" | "continue" (timeout case)
	FinalNote       string // supervisor's last user-facing note
	InterveneReason string // populated when FinalVerdict == "intervene"
	ExitCode        int    // bash exit code; -1 if process never exited
	TailCombined    string // last ~32KB of combined output for next iter
	SupervisorTurns int    // how many supervisor calls we made
	CostUSDMicro    int64  // sum of all supervisor calls this iter
}

// superviseScript runs the script in the background while polling Claude
// for verdicts. Returns when the supervisor says done/intervene, the
// process exits without a "done" verdict (treated as intervene), the
// supervisor-turn cap is reached, the parent ctx is cancelled, or the
// budget runs out.
//
// Named returns are load-bearing: the defer below mutates `res` to
// snapshot the bash exit code + final tail, and those fields need to
// surface to the CALLER (not just the local copy).
func superviseScript(ctx context.Context, in superviseConfig) (res superviseResult, retErr error) {
	res.ExitCode = -1
	auditDir := in.AuditDir
	iter := in.Iteration

	// Open the per-iteration live-tail file. Combined stdout+stderr go
	// here so the supervisor sees one unified stream (operators don't
	// reason about stderr vs stdout when watching `npm install`).
	livePath := filepath.Join(auditDir, fmt.Sprintf("script-v%d.live.log", iter))
	liveFile, err := os.OpenFile(livePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return res, fmt.Errorf("open live log: %w", err)
	}
	defer liveFile.Close()

	turnsPath := filepath.Join(auditDir, fmt.Sprintf("script-v%d.supervisor-turns.log", iter))
	turnsFile, err := os.OpenFile(turnsPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return res, fmt.Errorf("open supervisor-turns log: %w", err)
	}
	defer turnsFile.Close()

	// Start bash in the background. Setpgid so a final `nohup APP &` in
	// the script survives any kill we send to the bash pgroup on
	// intervene — we want to kill the INSTALLER, not the app it just
	// launched. Wait — actually for intervene we DO want to kill the
	// whole pgroup, because a half-installed app is just garbage. We
	// kill the pgroup on intervene; the supervisor's "done" path never
	// kills, leaving the nohup'd app process alive.
	cmd := exec.Command("/bin/bash", in.ScriptPath)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return res, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return res, fmt.Errorf("stderr pipe: %w", err)
	}
	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("start bash: %w", err)
	}
	pgid := -cmd.Process.Pid // negative pid → kill the whole process group

	var (
		tail      tailBuffer
		streamMu  sync.Mutex
		streamWG  sync.WaitGroup
		liveBytes int64 // total bytes written to liveFile (monotonic)
		lastGrow  = time.Now()
	)
	writeLine := func(stream, line string) {
		streamMu.Lock()
		defer streamMu.Unlock()
		// Prepend a millisecond timestamp so the supervisor can reason
		// about pacing from the tail snapshot alone.
		stamped := fmt.Sprintf("[%s] %s\n",
			time.Now().UTC().Format("15:04:05.000"), line)
		tail.Write(stamped)
		if n, _ := liveFile.WriteString(stamped); n > 0 {
			liveBytes += int64(n)
			lastGrow = time.Now()
		}
		slog.Info("agent_script-apply",
			"stream", stream, "iter", iter, "line", line)
	}
	streamWG.Add(2)
	go func() {
		defer streamWG.Done()
		defer stdoutPipe.Close()
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			writeLine("stdout", sc.Text())
		}
	}()
	go func() {
		defer streamWG.Done()
		defer stderrPipe.Close()
		sc := bufio.NewScanner(stderrPipe)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			writeLine("stderr", sc.Text())
		}
	}()

	// Wait for the process in a goroutine so the main supervisor loop
	// can poll status independently. We don't need the wait error —
	// cmd.ProcessState.ExitCode() is the dispositive signal.
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		_ = cmd.Wait()
	}()

	// killPgroup is the SIGTERM→5s→SIGKILL escalation we send on
	// intervene or ctx cancel. Operates on the negative pid so the WHOLE
	// pgroup (including any setsid/nohup'd children the script
	// launched) dies. This is the *intervene* semantic — a half-built
	// app is garbage; clean it up.
	killPgroup := func() {
		if cmd.Process == nil {
			return
		}
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		select {
		case <-waitDone:
			return
		case <-time.After(5 * time.Second):
		}
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}

	// Inner supervisor loop.
	var prevLiveBytes int64
	defer func() {
		// Always reap children, even on error/panic paths.
		streamWG.Wait()
		<-waitDone
		if cmd.ProcessState != nil {
			res.ExitCode = cmd.ProcessState.ExitCode()
		}
		res.TailCombined = tail.String()
	}()

	for turn := 1; turn <= in.MaxSupervisorTurns; turn++ {
		// Sleep CHECK_INTERVAL or until process exit / ctx cancel.
		select {
		case <-ctx.Done():
			killPgroup()
			res.FinalVerdict = "intervene"
			res.InterveneReason = "parent context cancelled: " + ctx.Err().Error()
			return res, ctx.Err()
		case <-waitDone:
			// Process exited on its own — fall through to the poll
			// below so the supervisor can decide done/intervene based
			// on exit code + tail. Don't kill, don't sleep.
		case <-time.After(in.CheckInterval):
		}

		// Budget check before each Claude call.
		if in.BudgetRemainingFn != nil {
			rem := in.BudgetRemainingFn() - res.CostUSDMicro
			if rem >= 0 && rem <= 0 {
				killPgroup()
				res.FinalVerdict = "intervene"
				res.InterveneReason = "token budget exhausted mid-iteration"
				return res, errors.New("token budget exhausted")
			}
		}

		// Snapshot process status + tail.
		var procStatus string
		var exitPtr *int
		select {
		case <-waitDone:
			procStatus = "EXITED"
			if cmd.ProcessState != nil {
				ec := cmd.ProcessState.ExitCode()
				exitPtr = &ec
			}
		default:
			procStatus = "RUNNING"
		}
		tailNow := tailBytesFrom(&tail, liveTailBytes)
		// liveBytes and lastGrow are mutated by the stream goroutines under
		// streamMu (see writeLine); take the same lock to read them so the
		// race detector stays quiet when a kill races a final stream write.
		streamMu.Lock()
		curLiveBytes := liveBytes
		curLastGrow := lastGrow
		streamMu.Unlock()
		bytesSince := curLiveBytes - prevLiveBytes
		prevLiveBytes = curLiveBytes
		idle := time.Since(curLastGrow)

		// Short-circuit: process exited 0 AND verify port already serving
		// is a definitive success — don't burn supervisor turns asking
		// Claude to confirm what we can verify ourselves. The n8n smoke
		// (2026-06-01) showed the supervisor false-positive-intervening
		// while looping waiting for "startup logs" that never reach
		// stdout (apps log to their own files post-`nohup ... &`). The
		// loopback HTTP probe is ground truth: if the port responds, the
		// app is up, regardless of what stdout says.
		if procStatus == "EXITED" && exitPtr != nil && *exitPtr == 0 && in.VerifyPortHint > 0 {
			if cc := probeLocalPort(ctx, in.VerifyPortHint); cc >= 200 && cc < 500 {
				slog.Info("agent_script: auto-done (exit 0 + verify port serving)",
					"iter", iter, "turn", turn,
					"verify_port", in.VerifyPortHint, "curl_code", cc,
				)
				res.FinalVerdict = "done"
				res.FinalNote = fmt.Sprintf("script exited 0; port %d returned http %d (agent-ops short-circuit, supervisor not consulted)", in.VerifyPortHint, cc)
				return res, nil
			}
		}

		// Invoke supervisor.
		out, err := in.Supervisor.Check(ctx, SupervisorInput{
			Goal:           in.Goal,
			IterationNum:   iter,
			ScriptBody:     in.ScriptBody,
			IterStart:      startedAt,
			SupervisorTurn: turn,
			ProcStatus:     procStatus,
			ExitCode:       exitPtr,
			TailBytes:      tailNow,
			BytesSinceWake: bytesSince,
			IdleDuration:   idle,
			VerifyPortHint: in.VerifyPortHint,
			Model:          in.Model,
		})
		if err != nil {
			// Operational Claude error (timeout, OAuth blip). Don't
			// kill — log + try again next interval.
			slog.Warn("agent_script: supervisor call errored, treating as continue",
				"iter", iter, "turn", turn, "err", err.Error())
			appendSupervisorTurn(turnsFile, turn, procStatus, exitPtr, idle, SupervisorOutput{
				Verdict:    SupervisorVerdict{Verdict: "continue", Note: "supervisor call errored, retrying"},
				ParseError: err.Error(),
			})
			res.SupervisorTurns = turn
			if procStatus == "EXITED" {
				// Process already gone AND supervisor unreachable. Bail
				// out so we don't loop forever calling a broken model.
				res.FinalVerdict = "intervene"
				res.InterveneReason = "supervisor unreachable + bash exited"
				return res, err
			}
			continue
		}
		res.CostUSDMicro += out.CostUSDMicro
		res.SupervisorTurns = turn
		appendSupervisorTurn(turnsFile, turn, procStatus, exitPtr, idle, out)

		// Emit a structured user-visible progress line per turn.
		elapsedS := int(time.Since(startedAt).Seconds())
		slog.Info("agent_script: progress",
			"iter", iter,
			"turn", turn,
			"elapsed_s", elapsedS,
			"proc_status", procStatus,
			"verdict", out.Verdict.Verdict,
			"note", out.Verdict.Note,
		)
		if out.ParseError != "" {
			slog.Warn("agent_script: supervisor returned malformed JSON, treated as continue",
				"iter", iter, "turn", turn,
				"parse_error", out.ParseError,
				"raw_tail", truncate(out.Raw, 200),
			)
		}

		switch out.Verdict.Verdict {
		case "done":
			res.FinalVerdict = "done"
			res.FinalNote = out.Verdict.Note
			// Do NOT kill — supervisor declared the app up. If bash is
			// still running (e.g. tailing a log file), we leave it; in
			// practice catalog scripts background the app and exit, so
			// this is rare.
			return res, nil
		case "intervene":
			res.FinalVerdict = "intervene"
			res.InterveneReason = out.Verdict.Reason
			if res.InterveneReason == "" {
				res.InterveneReason = out.Verdict.Note
			}
			killPgroup()
			return res, nil
		case "continue":
			// keep looping
		default:
			// Unknown verdict — log + treat as continue. The malformed-
			// JSON path also lands here (parseSupervisorVerdict
			// returns "continue" on parse error).
			slog.Warn("agent_script: unknown supervisor verdict, treating as continue",
				"iter", iter, "turn", turn, "verdict", out.Verdict.Verdict)
		}

		// If the process exited and the supervisor said continue, give
		// it one more turn to render a verdict — but if we're already
		// at max turns, bail out.
		if procStatus == "EXITED" && turn >= in.MaxSupervisorTurns {
			res.FinalVerdict = "intervene"
			res.InterveneReason = "bash exited but supervisor never reached 'done'"
			return res, nil
		}
	}

	// Hit the per-iter supervisor-turn cap.
	killPgroup()
	res.FinalVerdict = "intervene"
	res.InterveneReason = fmt.Sprintf("supervisor-turn cap %d reached without 'done'",
		in.MaxSupervisorTurns)
	return res, nil
}

// ─── helpers ───────────────────────────────────────────────────────────

// probeLocalPort issues GET http://127.0.0.1:<port>/ with a 5-second
// timeout and returns the HTTP status code. Network / timeout errors
// return 0. Used by the supervise loop's short-circuit path: when the
// script exits 0 and this port responds 2xx-499, agent-ops declares
// success without burning more supervisor turns.
//
// Single-shot — no retry loop. The caller polls this every
// supervise-tick if needed, so a per-call retry inside would compound.
func probeLocalPort(ctx context.Context, port int) int {
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", url, nil)
	if err != nil {
		return 0
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// tailBytesFrom returns the last n bytes of buf.String(), preferring to
// cut on a newline boundary so the supervisor doesn't see a partial line
// at the front.
func tailBytesFrom(buf *tailBuffer, n int) string {
	s := buf.String()
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	// Skip leading partial line.
	if idx := strings.IndexByte(cut, '\n'); idx >= 0 && idx < len(cut)-1 {
		cut = cut[idx+1:]
	}
	return cut
}

// parseSupervisorVerdict locates the first JSON object in s and unmarshals
// it. On any parse failure returns a "continue" verdict + the error, so
// the caller can transparently keep polling without crashing.
func parseSupervisorVerdict(s string) (SupervisorVerdict, error) {
	raw, ok := extractJSONObject(s)
	if !ok {
		return SupervisorVerdict{
			Verdict: "continue",
			Note:    "supervisor returned no JSON; treating as continue",
		}, errors.New("no JSON object in supervisor reply")
	}
	var v SupervisorVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return SupervisorVerdict{
			Verdict: "continue",
			Note:    "supervisor JSON did not parse; treating as continue",
		}, fmt.Errorf("unmarshal: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
	case "done", "continue", "intervene":
		v.Verdict = strings.ToLower(strings.TrimSpace(v.Verdict))
		return v, nil
	default:
		return SupervisorVerdict{
			Verdict: "continue",
			Note:    "supervisor verdict unrecognised (" + v.Verdict + "); treating as continue",
		}, fmt.Errorf("unrecognised verdict: %q", v.Verdict)
	}
}

// appendSupervisorTurn writes one JSON line per supervisor poll to the
// per-iteration turns audit file. Best-effort — failure to write the
// audit line MUST NOT abort the supervise loop.
func appendSupervisorTurn(
	w io.Writer,
	turn int,
	procStatus string,
	exit *int,
	idle time.Duration,
	out SupervisorOutput,
) {
	rec := map[string]any{
		"ts":               time.Now().UTC().Format(time.RFC3339Nano),
		"turn":             turn,
		"proc_status":      procStatus,
		"idle_seconds":     int(idle.Seconds()),
		"verdict":          out.Verdict.Verdict,
		"note":             out.Verdict.Note,
		"reason":           out.Verdict.Reason,
		"cost_usd_micro":   out.CostUSDMicro,
		"raw_truncated":    truncate(out.Raw, 1024),
	}
	if exit != nil {
		rec["exit_code"] = *exit
	}
	if out.ParseError != "" {
		rec["parse_error"] = out.ParseError
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = w.Write(append(b, '\n'))
}

// ─── env-var helpers ───────────────────────────────────────────────────

// agentScriptMaxIterations reads AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS with a
// default of 5. Clamps to [1, 10] — single-digit so a misconfigured value
// can't accidentally fund a $10 Claude bill on retry loops.
func agentScriptMaxIterations() int {
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_MAX_ITERATIONS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 10 {
				return 10
			}
			return n
		}
	}
	return defaultMaxIterations
}

// agentScriptVerifyPort reads AGENT_OPS_BOOTSTRAP_VERIFY_PORT. Returns 0
// when unset/invalid — caller passes 0 to the supervisor as "no hint".
//
// NB: in 0.3.9 this is a HINT to the supervisor, not an enforced gate.
// The supervisor decides done/intervene based on the live log; the port
// is supplied only so the supervisor can look for "listening on :PORT"
// in the tail.
func agentScriptVerifyPort() int {
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_VERIFY_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return 0
}

// agentScriptCheckInterval reads AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S.
// Default 30s. Clamped to [5s, 300s] so a misconfigured value can't pin
// CPU (5s floor) or stall observation past usefulness (5min ceiling).
func agentScriptCheckInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_CHECK_INTERVAL_S")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n < 5 {
				n = 5
			}
			if n > 300 {
				n = 300
			}
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(defaultCheckIntervalS) * time.Second
}

// agentScriptMaxSupervisorTurns reads
// AGENT_OPS_BOOTSTRAP_MAX_SUPERVISOR_TURNS. Default 60. Clamped to
// [1, 600] so a runaway env value can't burn the whole task budget on
// one iteration.
func agentScriptMaxSupervisorTurns() int {
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_MAX_SUPERVISOR_TURNS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 600 {
				return 600
			}
			return n
		}
	}
	return defaultMaxSupervisorTurns
}

// agentScriptTokenBudgetUSD reads AGENT_OPS_BOOTSTRAP_TOKEN_BUDGET_USD.
// Default $1.00. Returns 0 when explicitly set to "0" (disables budget
// gate entirely — operator-overridable for soak tests).
func agentScriptTokenBudgetUSD() float64 {
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_TOKEN_BUDGET_USD")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return defaultTokenBudgetUSD
}

// finalStatusV2 is the 0.3.9+ shape final-status.json takes. Backward
// compatible with consumers that only read .success — added fields are
// purely additive.
type finalStatusV2 struct {
	Success         bool    `json:"success"`
	Iterations      int     `json:"iterations"`
	SupervisorTurns int     `json:"supervisor_turns"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	FinalVerdict    string  `json:"final_verdict"`
	FinalNote       string  `json:"final_note,omitempty"`
}

// finalStatus is preserved for the existing tests + downstream consumers
// that read the legacy fields. The 0.3.8 fields (exit_code, iterations,
// final_curl_code) are subsetted by finalStatusV2.
type finalStatus struct {
	Success       bool `json:"success"`
	ExitCode      int  `json:"exit_code"`
	Iterations    int  `json:"iterations"`
	FinalCurlCode int  `json:"final_curl_code"`
}

// writeFinalStatusV2 emits both the v2 fields AND the legacy v1 fields
// into one JSON object so existing dashboard / test consumers don't
// break on the upgrade. Same on-disk path.
func writeFinalStatusV2(dir string, v finalStatusV2) {
	merged := map[string]any{
		"success":          v.Success,
		"iterations":       v.Iterations,
		"supervisor_turns": v.SupervisorTurns,
		"total_cost_usd":   v.TotalCostUSD,
		"final_verdict":    v.FinalVerdict,
		// Legacy keys — preserved so 0.3.8 dashboards keep working.
		// exit_code is 0 on success and -1 otherwise (we no longer
		// surface a single dispositive exit code under supervisor mode).
		"exit_code":       func() int { if v.Success { return 0 }; return -1 }(),
		"final_curl_code": 0,
	}
	if v.FinalNote != "" {
		merged["final_note"] = v.FinalNote
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "final-status.json"), raw, 0o600)
}

// ─── prompt templates ──────────────────────────────────────────────────

// buildPlannerPrompts assembles the (system, user) prompt pair for the
// Phase 1 claudecli subprocess. Stable across iterations so dashboard /
// test assertions can substring-grep.
func buildPlannerPrompts(in PlanInput) (system, user string) {
	verifyClause := ""
	if in.VerifyPort > 0 {
		// 0.3.9: this is a HINT, not an enforced gate. Tell the planner
		// the port so its script can probe / log "listening on :PORT".
		// Keep the curl loop in the script so even a no-supervisor dry
		// run still self-tests.
		verifyClause = fmt.Sprintf(
			"\n\nThe script's final lines MUST verify the app is listening on port %d. End the script with:\n"+
				"  for i in $(seq 1 6); do curl -sf -o /dev/null -m 5 http://127.0.0.1:%d/ && exit 0; sleep 5; done; exit 1\n",
			in.VerifyPort, in.VerifyPort)
	}

	rulesBlock := ""
	if in.HouseRules != "" {
		rulesBlock = "\n\n==== HOUSE RULES ====\n" + in.HouseRules + "\n==== END HOUSE RULES ====\n"
	}

	system = fmt.Sprintf(`You are the PLANNER phase of agent-ops's plan/apply install loop.

Your ONLY job: write a complete bash install script to %s using the Write tool.
You DO NOT have access to the Bash tool. You CANNOT execute commands. You CAN read files (Read tool) if you need to inspect /etc/os-release, /tmp state from prior attempts, etc.

After Write, output a one-line summary (e.g. "wrote install.sh, %d lines"). Do not echo the script back — it's already on disk.

The script will be executed by the APPLY phase via:  /bin/bash %s
Exit code 0 = success. Non-zero = failure → planner re-invoked with stderr/stdout for retry.%s%s`,
		in.ScriptPath,
		42,
		in.ScriptPath,
		verifyClause,
		rulesBlock,
	)

	if in.Iteration <= 1 {
		user = fmt.Sprintf(`Goal: %s

Write a complete bash script to %s that installs and starts this app. The script must:
  * run all installs in the foreground (no & on apt/npm/pip/etc.)
  * background ONLY the final long-running app process (setsid nohup APP > /tmp/app.log 2>&1 & disown)
  * end with a health-check loop that exits 0 only when the app responds

After calling Write, reply with ONE LINE confirming the file was written. Do not paste the script back.`,
			in.Goal, in.ScriptPath)
	} else {
		// 0.3.9: include supervisor's intervene-reason if present so
		// the planner sees WHY the previous attempt was killed, not
		// just the raw stderr tail.
		reasonBlock := ""
		if strings.TrimSpace(in.PrevReason) != "" {
			reasonBlock = fmt.Sprintf("\n==== SUPERVISOR REASON ====\n%s\n", in.PrevReason)
		}
		user = fmt.Sprintf(`Previous attempt failed (iteration %d). stderr/stdout below. Improve the script and write a new %s.

==== PREVIOUS EXIT CODE ====
%d
%s
==== PREVIOUS STDERR (tail) ====
%s

==== PREVIOUS STDOUT (tail) ====
%s

==== ORIGINAL GOAL ====
%s

Rewrite the WHOLE script with the fix baked in. Don't patch — replace. After Write, reply with ONE LINE confirming what changed.`,
			in.Iteration,
			in.ScriptPath,
			in.PrevExitCode,
			reasonBlock,
			truncate(strings.TrimSpace(in.PrevStderr), 2048),
			truncate(strings.TrimSpace(in.PrevStdout), 2048),
			in.Goal,
		)
	}
	return system, user
}

// buildSupervisorPrompts assembles the (system, user) prompt pair for one
// supervisor poll. The system prompt encodes the verdict contract; the
// user prompt rotates per turn with proc status + tail.
//
// Kept verbatim-stable so dashboard / test assertions can substring-
// match without churn.
func buildSupervisorPrompts(in SupervisorInput) (system, user string) {
	verifyHint := ""
	if in.VerifyPortHint > 0 {
		verifyHint = fmt.Sprintf(
			"\nVerify hint (from catalog): the app should be listening on port %d. "+
				"Look for `listening on :%d` or similar in the tail before declaring 'done'.",
			in.VerifyPortHint, in.VerifyPortHint)
	}

	system = fmt.Sprintf(`You are the SUPERVISOR phase of agent-ops's plan/supervise/intervene loop.
The PLANNER wrote a bash install script for this app. agent-ops is
running it in the background and feeding you periodic snapshots.

Your ONLY job: emit ONE JSON line, no prose, no markdown fence.

Verdicts:
  {"verdict":"continue","note":"<one-line user-facing progress>"}
  {"verdict":"done","note":"<one-line, e.g. 'app responding on :5678'>"}
  {"verdict":"intervene","reason":"<root cause>","note":"<user-line>"}

Use 'done' ONLY if process EXITED with code 0 AND the output tail
indicates the app actually started serving (curl 2xx/3xx in script's
own verify, or "listening on :PORT" log line). The script's exit code
alone isn't sufficient — apps lie about being ready.

Use 'intervene' if:
  - process EXITED with non-zero code
  - process RUNNING but no log activity for >3 minutes (idle_seconds in user prompt)
  - log shows OOM kill, ENOSPC, FATAL, panic, segfault, etc
  - log shows the app refused to start (port conflict, missing dep)
  - you predict the current attempt won't succeed in remaining time

Otherwise 'continue'. Be conservative on 'done' — false positives
leave broken instances live for users.%s

App goal (from operator): %s
Current iteration script (v%d):
===
%s
===`,
		verifyHint,
		in.Goal,
		in.IterationNum,
		truncate(in.ScriptBody, 4000),
	)

	exitStr := "null"
	if in.ExitCode != nil {
		exitStr = strconv.Itoa(*in.ExitCode)
	}
	elapsedMin := int(time.Since(in.IterStart).Minutes())
	user = fmt.Sprintf(`ITERATION %d (script v%d), SUPERVISOR TURN %d:
ELAPSED: %dmin
PROC_STATUS: %s
EXIT_CODE: %s
IDLE_SECONDS: %d  (seconds since the live log last grew; threshold for 'stuck' is %d)
BYTES_SINCE_LAST_POLL: %d
OUTPUT TAIL (last %d bytes):
===
%s
===

VERDICT?`,
		in.IterationNum,
		in.IterationNum,
		in.SupervisorTurn,
		elapsedMin,
		in.ProcStatus,
		exitStr,
		int(in.IdleDuration.Seconds()),
		stuckThresholdSeconds,
		in.BytesSinceWake,
		liveTailBytes,
		in.TailBytes,
	)
	return system, user
}

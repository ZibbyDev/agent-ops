// Copyright 2026 Zibby Lab. Apache-2.0.

// agent_script.go — bootstrap mode #5: phase-split plan/apply. Claude is
// invoked WITHOUT the Bash tool (Write,Read only) in Phase 1, writes a
// complete bash script to /tmp/install.sh, then Phase 2 execs that script
// directly via os/exec — no LLM in the install loop. On non-zero exit
// Phase 3 captures stderr/stdout, feeds it back to a fresh Phase 1, and
// rewrites the script for another attempt. Loops up to MAX_ITERATIONS.
//
// WHY: Mode #3 (goal-mode `agent`) hands Claude the Bash tool, and the
// claude-code CLI auto-backgrounds any Bash call after ~2 minutes (hard
// cap ~10 min). Heavy installs (`npm install -g n8n`, ~7 min) trip that
// cap; Claude then burns turns polling Monitor/TaskOutput. House rules
// can't override CLI behavior. Splitting Plan (LLM) from Apply (native
// exec) means the LLM never invokes Bash, so the auto-background problem
// can't surface. agent-mode (#3) stays available behind LEGACY_GOAL_MODE
// for rollback.
//
// State files (under <stateDir>/agent_script-state/):
//   iteration            — current attempt number (0-indexed during loop)
//   script-v<N>.sh       — every script the planner wrote, for audit
//   script-v<N>.stdout   — captured stdout from phase-2 exec
//   script-v<N>.stderr   — captured stderr from phase-2 exec
//   final-status.json    — terminal summary {success, exit_code, iterations,
//                          final_curl_code}

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

// defaultMaxIterations bounds the plan→apply→retry loop. 5 is enough to
// absorb 1-2 typo-grade failures + a real env-mismatch retry; beyond
// that the model is probably stuck in a loop and we should fail loud.
const defaultMaxIterations = 5

// phase1MaxTurns caps the planner subprocess. Phase 1 just writes a file;
// 8 turns is generous (Read goal + Read prior stderr + Write script = 3,
// the rest is slack for stray status messages).
const phase1MaxTurns = 8

// defaultScriptPath is where the planner writes + the runner reads in
// production. Kept stable across iterations so the planner's prompt
// always names the same path. Tests override via testScriptPathOverride.
const defaultScriptPath = "/tmp/install.sh"

// testScriptPathOverride lets unit tests redirect the script path to a
// per-test tempdir so parallel runs don't clobber a shared /tmp file.
// Production code reads it via scriptPathFor(); when empty the constant
// defaultScriptPath wins. NEVER read in a hot path — only at planner-
// invocation and bash-exec boundaries (~once per iteration).
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

// Planner is the Phase 1 abstraction. The real implementation shells out
// to claudecli with allowed_tools="Write,Read"; tests swap in a stub
// that returns a controlled script string.
//
// Plan is called once per iteration. The implementation MUST write the
// chosen bash script to /tmp/install.sh (or whatever scriptPath is) —
// the returned string is captured into the audit state file but the
// runner reads the actual script off disk so the contract matches what
// the real CLI does (Claude calls the Write tool; we don't trust an
// in-memory return).
type Planner interface {
	Plan(ctx context.Context, in PlanInput) (PlanOutput, error)
}

// PlanInput is what Phase 1 receives per iteration.
type PlanInput struct {
	Goal         string // user's deploy goal (AGENT_OPS_BOOTSTRAP_PROMPT)
	Model        string // optional model override (AGENT_OPS_BOOTSTRAP_MODEL)
	HouseRules   string // AGENT_OPS_BOOTSTRAP_SYSTEM_RULES verbatim
	Iteration    int    // 1-indexed; > 1 means PreviousStderr/Stdout are set
	PrevStderr   string // last ~2KB of previous attempt's stderr (iter > 1)
	PrevStdout   string // last ~2KB of previous attempt's stdout (iter > 1)
	PrevExitCode int    // exit code of previous attempt (iter > 1)
	ScriptPath   string // absolute path Claude must write the script to
	VerifyPort   int    // 0 = no verifier; >0 = the prompt includes a curl gate
}

// PlanOutput is what Phase 1 returns. RawScript is the planner's view of
// the file it wrote — informational for state-file audit; the runner
// reads the actual script from disk regardless.
type PlanOutput struct {
	RawScript    string  // what the planner believes it wrote (audit only)
	CostUSDMicro int64   // for cost accounting
	NumTurns     int     // for diagnostics
}

// claudecliPlanner is the production Planner. It assembles the iteration
// prompt + invokes claudecli with the Bash tool DISABLED. Single chokepoint
// for the "no Bash in Phase 1" invariant — unit tests assert against the
// AllowedTools field directly (see agent_script_test.go).
type claudecliPlanner struct {
	// Driver is the claudecli driver to call. Constructor pins
	// AllowedTools="Write,Read" — DO NOT rebuild this driver with Bash
	// in the allow-list; that reintroduces the auto-background bug
	// this whole mode exists to fix.
	Driver *claudecli.Driver
}

// newClaudecliPlanner constructs the production planner. Centralizes the
// AllowedTools value so any caller (daemon, future re-runs from MCP)
// can't accidentally elevate the planner subprocess.
func newClaudecliPlanner(model string) *claudecliPlanner {
	return &claudecliPlanner{
		Driver: &claudecli.Driver{
			Model: model,
			// "Write,Read" — NO Bash, NO Edit. The planner writes a script
			// to disk and that's it. Adding Bash here is a regression.
			AllowedTools: "Write,Read",
			// acceptEdits is the only mode OAuth tokens accept; even
			// though we forbid Edit via AllowedTools, the CLI rejects
			// bypassPermissions at spawn so we keep acceptEdits.
			PermissionMode: "acceptEdits",
		},
	}
}

// Plan implements Planner against the real claudecli driver. Builds the
// per-iteration system + user prompt and shells out.
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
// Production: nil → runAgentScriptBootstrap constructs claudecliPlanner.
// Tests: set to a stub before calling, then defer-reset.
var activePlanner Planner

// runAgentScriptBootstrap is the entry point invoked by MaybeRunFirstRun
// when AGENT_OPS_BOOTSTRAP_MODE=agent_script. Loops:
//
//	Phase 1: planner writes /tmp/install.sh
//	Phase 2: native bash /tmp/install.sh; capture stdout/stderr/exit
//	Phase 3 (if exit != 0 OR verifier port closed): record outcome,
//	        retry — up to MaxIterations
//
// Returns nil on success (script exited 0 AND, if VerifyPort is set, the
// curl probe got 2xx/3xx). Returns an error wrapping the final exit code
// + tail of the last stderr on exhaustion.
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
	model := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_MODEL"))
	maxIter := agentScriptMaxIterations()
	verifyPort := agentScriptVerifyPort()

	// State dir for per-iteration audit. <stateDir>/agent_script-state/
	// — under the daemon's existing state directory so it inherits EFS
	// persistence + dump-on-destroy semantics.
	auditDir := filepath.Join(cfg.StateDir, "agent_script-state")
	if err := os.MkdirAll(auditDir, 0o700); err != nil {
		return fmt.Errorf("agent_script: mkdir audit dir: %w", err)
	}

	// Lazy planner construction — tests inject via activePlanner.
	planner := activePlanner
	if planner == nil {
		planner = newClaudecliPlanner(model)
	}

	slog.Info("agent_script: starting plan/apply loop",
		"goal_bytes", len(goal),
		"max_iterations", maxIter,
		"verify_port", verifyPort,
		"model", model,
		"audit_dir", auditDir,
	)

	var (
		prevStderr   string
		prevStdout   string
		prevExit     int
		finalExit    int
		finalCurlCC  int
		lastError    error
	)

	// Resolve the script path ONCE per bootstrap run. Tests inject via
	// testScriptPathOverride; production resolves to /tmp/install.sh.
	scrPath := scriptPathFor()

	for iter := 1; iter <= maxIter; iter++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("agent_script: context cancelled at iteration %d: %w", iter, ctxErr)
		}

		// Persist the iteration counter for audit / external reads.
		_ = os.WriteFile(filepath.Join(auditDir, "iteration"),
			[]byte(strconv.Itoa(iter)), 0o600)

		slog.Info("agent_script: iteration", "iteration", iter, "max", maxIter, "phase", "plan")

		// Remove any stale script from a previous iteration so the
		// "planner failed to write" detector below can't be fooled by a
		// leftover file on the same path.
		_ = os.Remove(scrPath)

		planOut, planErr := planner.Plan(ctx, PlanInput{
			Goal:         goal,
			Model:        model,
			HouseRules:   houseRules,
			Iteration:    iter,
			PrevStderr:   prevStderr,
			PrevStdout:   prevStdout,
			PrevExitCode: prevExit,
			ScriptPath:   scrPath,
			VerifyPort:   verifyPort,
		})
		if planErr != nil {
			// Planner subprocess failed (timeout, OAuth error, etc.). Record
			// + retry — the next iteration may succeed if it was transient.
			slog.Warn("agent_script: planner failed", "iteration", iter, "err", planErr.Error())
			lastError = planErr
			prevStderr = "planner failed: " + planErr.Error()
			prevStdout = ""
			prevExit = -1
			continue
		}

		// Read the script the planner actually wrote. We trust the file on
		// disk (the Write tool's side-effect) over the model's textual
		// echo because the textual echo can omit chunks under truncation.
		scriptBytes, readErr := os.ReadFile(scrPath)
		if readErr != nil {
			slog.Warn("agent_script: planner did not write script file",
				"iteration", iter, "path", scrPath, "err", readErr.Error())
			lastError = fmt.Errorf("planner did not write %s: %w", scrPath, readErr)
			prevStderr = "planner did not write " + scrPath + ": " + readErr.Error()
			prevStdout = planOut.RawScript // surface the model's intent for next prompt
			prevExit = -1
			continue
		}

		// Audit copy. Best-effort — failure here is not fatal.
		_ = os.WriteFile(filepath.Join(auditDir,
			fmt.Sprintf("script-v%d.sh", iter)), scriptBytes, 0o600)

		slog.Info("agent_script: iteration",
			"iteration", iter, "max", maxIter, "phase", "apply",
			"script_bytes", len(scriptBytes),
			"planner_cost_usd_micro", planOut.CostUSDMicro,
			"planner_turns", planOut.NumTurns,
		)

		exit, stdout, stderr, runErr := runBashScript(ctx, scrPath)
		finalExit = exit

		// Persist outputs every iteration. truncate keeps disk bounded;
		// 32KB tail is plenty for grepping a failure signal.
		writeAudit(auditDir, iter, "stdout", stdout)
		writeAudit(auditDir, iter, "stderr", stderr)

		_, _ = store.AddFact(ctx, "agent_script",
			fmt.Sprintf("iter=%d exit=%d at %s; stderr_tail: %s",
				iter, exit, time.Now().UTC().Format(time.RFC3339),
				truncate(strings.TrimSpace(stderr), 400)))

		if runErr != nil && exit == 0 {
			// Process couldn't even start (e.g. /tmp/install.sh missing
			// chmod, bash not on PATH). Treat as a generic failure.
			slog.Warn("agent_script: bash failed to start",
				"iteration", iter, "err", runErr.Error())
			prevStderr = "bash start error: " + runErr.Error()
			prevStdout = stdout
			prevExit = -1
			lastError = runErr
			continue
		}

		if exit != 0 {
			slog.Warn("agent_script: iteration failed",
				"iteration", iter, "exit_code", exit,
				"stderr_tail", truncate(strings.TrimSpace(stderr), 400),
			)
			prevStderr = stderr
			prevStdout = stdout
			prevExit = exit
			lastError = fmt.Errorf("script exit %d on iteration %d", exit, iter)
			continue
		}

		// exit == 0. If a verifier port was configured, gate success on a
		// curl probe. Matches cheatsheet-mode's "model lies, the open
		// socket is truth" rule.
		if verifyPort > 0 {
			slog.Info("agent_script: iteration",
				"iteration", iter, "max", maxIter,
				"phase", "verify", "port", verifyPort,
			)
			cc := pollVerifyPort(ctx, verifyPort)
			finalCurlCC = cc
			if cc < 200 || cc >= 400 {
				slog.Warn("agent_script: verify failed",
					"iteration", iter, "port", verifyPort, "curl_code", cc,
				)
				prevStderr = fmt.Sprintf("verify port %d returned http %d (script exited 0 but app not serving 2xx/3xx)", verifyPort, cc)
				prevStdout = stdout
				prevExit = 0
				lastError = fmt.Errorf("verify port %d returned http %d", verifyPort, cc)
				continue
			}
		}

		// Success.
		slog.Info("agent_script: success",
			"iteration", iter, "exit_code", 0, "verify_curl_code", finalCurlCC,
		)
		writeFinalStatus(auditDir, true, finalExit, iter, finalCurlCC)
		return nil
	}

	writeFinalStatus(auditDir, false, finalExit, maxIter, finalCurlCC)
	if lastError == nil {
		lastError = fmt.Errorf("agent_script: %d iterations exhausted with no success", maxIter)
	}
	return fmt.Errorf("agent_script: bootstrap failed after %d iterations: %w",
		maxIter, lastError)
}

// runBashScript execs `/bin/bash <path>` and returns (exit, stdout, stderr,
// startErr). No timeout cap applied here — npm install -g n8n runs 5-10
// min and that's fine. The surrounding cfg.Agent.TaskTimeout / context
// cancellation is the real upper bound (typically 30 min wall-clock).
//
// Streams BOTH pipes to slog so operators see real-time progress in
// CloudWatch (this is the whole point of native exec vs. shelling
// through Claude — we get raw stdout, not summarized turns).
func runBashScript(ctx context.Context, path string) (int, string, string, error) {
	cmd := exec.CommandContext(ctx, "/bin/bash", path)
	cmd.Env = os.Environ()
	// Same SIGTERM-then-SIGKILL escalation as runScriptBootstrap. Detach
	// from the daemon's pgrp so a `nohup ... &` final line keeps the app
	// alive after bash exits.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return 0, "", "", fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return 0, "", "", fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, "", "", fmt.Errorf("start: %w", err)
	}

	var (
		stdoutTail tailBuffer
		stderrTail tailBuffer
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go streamPipeToTail(&wg, stdoutPipe, "stdout", &stdoutTail)
	go streamPipeToTail(&wg, stderrPipe, "stderr", &stderrTail)

	waitErr := cmd.Wait()
	wg.Wait()

	exit := cmd.ProcessState.ExitCode()
	return exit, stdoutTail.String(), stderrTail.String(), waitErr
}

// streamPipeToTail copies a pipe to slog line-by-line and mirrors a
// bounded tail into buf. Mirrors streamPipe in bootstrap.go but writes
// to a CALLER-OWNED tailBuffer so caller can inspect each stream
// independently (script-vN.stdout vs .stderr audit files).
func streamPipeToTail(wg *sync.WaitGroup, r io.ReadCloser, stream string, buf *tailBuffer) {
	defer wg.Done()
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		buf.Write(line + "\n")
		slog.Info("agent_script-apply", "stream", stream, "line", line)
	}
	if err := sc.Err(); err != nil {
		slog.Warn("agent_script-apply: scan error", "stream", stream, "err", err.Error())
	}
}

// pollVerifyPort hits http://127.0.0.1:<port>/ up to 6 times with 5s
// between attempts. Returns the first 2xx/3xx response code observed, or
// the last code seen on full exhaustion. 6×5s = 30s budget — matches the
// cheatsheet-mode verifier window.
//
// Any 2xx OR 3xx counts as success. A redirect-to-login (302) is just
// as good evidence that the app started; we're verifying the server is
// up, not that the user can log in.
func pollVerifyPort(ctx context.Context, port int) int {
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	var last int
	for i := 0; i < 6; i++ {
		if ctx.Err() != nil {
			return last
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			last = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return resp.StatusCode
			}
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(5 * time.Second):
		}
	}
	return last
}

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
// when unset/invalid — caller treats 0 as "skip the curl gate".
func agentScriptVerifyPort() int {
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_VERIFY_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return 0
}

// writeAudit persists per-iteration stdout/stderr to the audit dir. Best-
// effort — caller doesn't check the error (audit isn't load-bearing).
func writeAudit(dir string, iter int, stream, content string) {
	path := filepath.Join(dir, fmt.Sprintf("script-v%d.%s", iter, stream))
	_ = os.WriteFile(path, []byte(content), 0o600)
}

// finalStatus is the shape final-status.json takes. Stable across versions
// — external tools (zibby agent logs, the dashboard) tail this file.
type finalStatus struct {
	Success       bool `json:"success"`
	ExitCode      int  `json:"exit_code"`
	Iterations    int  `json:"iterations"`
	FinalCurlCode int  `json:"final_curl_code"`
}

func writeFinalStatus(dir string, success bool, exit, iters, curl int) {
	out := finalStatus{
		Success:       success,
		ExitCode:      exit,
		Iterations:    iters,
		FinalCurlCode: curl,
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "final-status.json"), raw, 0o600)
}

// buildPlannerPrompts assembles the (system, user) prompt pair for the
// Phase 1 claudecli subprocess. The system prompt is fact-dense and
// pinned across iterations; the user prompt rotates per iteration to
// inject the previous attempt's stderr/stdout.
//
// Kept verbatim-stable so dashboard / test assertions can substring-
// match without churn.
func buildPlannerPrompts(in PlanInput) (system, user string) {
	verifyClause := ""
	if in.VerifyPort > 0 {
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
		// %d for line count is a hint to the model; it doesn't have to be exact.
		42,
		in.ScriptPath,
		verifyClause,
		rulesBlock,
	)

	// User prompt — iteration 1 vs iteration N+1. Verbatim templates so
	// tests can substring-match.
	if in.Iteration <= 1 {
		user = fmt.Sprintf(`Goal: %s

Write a complete bash script to %s that installs and starts this app. The script must:
  * run all installs in the foreground (no & on apt/npm/pip/etc.)
  * background ONLY the final long-running app process (setsid nohup APP > /tmp/app.log 2>&1 & disown)
  * end with a health-check loop that exits 0 only when the app responds

After calling Write, reply with ONE LINE confirming the file was written. Do not paste the script back.`,
			in.Goal, in.ScriptPath)
	} else {
		user = fmt.Sprintf(`Previous attempt failed (iteration %d). stderr/stdout below. Improve the script and write a new %s.

==== PREVIOUS EXIT CODE ====
%d

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
			truncate(strings.TrimSpace(in.PrevStderr), 2048),
			truncate(strings.TrimSpace(in.PrevStdout), 2048),
			in.Goal,
		)
	}
	return system, user
}

// Copyright 2026 Zibby Lab. Apache-2.0.

// Package bootstrap performs first-run initialization for the daemon:
// generates/loads the MCP bearer token, persists it under <state>/mcp.token,
// and (when a bootstrap schedule is configured) runs it exactly once.
//
// "First run" is detected by the presence of <state>/bootstrap.done; this is
// a single file rather than a state.events query so a future Raft replay
// doesn't trigger a fresh bootstrap on every follower.
package bootstrap

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/config"
	"github.com/ZibbyHQ/agent-ops/internal/scheduler"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/task"
	"github.com/ZibbyHQ/agent-ops/internal/zibby"
)

// EnsureToken returns the daemon's MCP bearer token. Priority:
//  1. AGENT_OPS_TOKEN env var (the Zibby control plane sets this when
//     provisioning a sidecar — token is already known upstream)
//  2. <stateDir>/mcp.token file (preserved across restarts)
//  3. A fresh 32-byte random token, persisted to (2)
func EnsureToken(stateDir, envName string) (string, error) {
	if envName != "" {
		if v := os.Getenv(envName); v != "" {
			return v, nil
		}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("bootstrap.EnsureToken: state dir: %w", err)
	}
	path := filepath.Join(stateDir, "mcp.token")
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		return string(raw), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("bootstrap.EnsureToken: read: %w", err)
	}
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", fmt.Errorf("bootstrap.EnsureToken: persist: %w", err)
	}
	return tok, nil
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "ao_" + hex.EncodeToString(buf), nil
}

// MaybeRunFirstRun fires the configured Bootstrap exactly once if
// <stateDir>/bootstrap.done is absent. Idempotent — subsequent restarts skip.
//
// AGENT_OPS_BOOTSTRAP_MODE selects the strategy:
//   - "script" — exec AGENT_OPS_BOOTSTRAP_SCRIPT as `bash -c` directly, no
//     LLM. Used for catalog (deterministic) installs where every line is
//     known good — skips the ~$0.20 / 9-minute LLM-thinks-through-each-bash
//     hop that v0.1.11 ran for these.
//   - "cheatsheet" — LLM-driven install with catalog-provided hints
//     (image + steps + known pitfalls). Reads AGENT_OPS_CHEATSHEET_JSON,
//     assembles a system+user prompt, then routes through the same
//     scheduler/claudecli path as agent-mode. Falls between script-mode
//     (zero LLM, brittle) and goal-mode (full LLM, expensive). See
//     cheatsheet.go for the prompt template + budget enforcement.
//   - "agent" (default, or empty) — original path: hand cfg.Bootstrap.Prompt
//     to the configured agent driver and let it use shell tools to think
//     its way through. Still required for free-form `zibby_deploy_app
//     --goal="..."` invocations and for non-catalog use of agent-ops.
//
// On every path the post-bootstrap port-register handshake + marker file
// write are identical — they all run through finalizeBootstrapSuccess.
func MaybeRunFirstRun(
	ctx context.Context,
	cfg *config.Config,
	sched *scheduler.Scheduler,
	store *state.Store,
) error {
	if cfg.Bootstrap == nil &&
		strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_SCRIPT")) == "" &&
		!isCheatsheetMode() {
		return nil
	}
	marker := filepath.Join(cfg.StateDir, "bootstrap.done")
	if _, err := os.Stat(marker); err == nil {
		// Marker exists from a previous container's bootstrap. BUT marker
		// just records "install completed once" — it does NOT mean the
		// app process is currently running. Containers are ephemeral; on
		// any ECS task restart (upgrade, crash, scale event) the install
		// on EFS is durable but the launched process is gone.
		//
		// If the app's declared port isn't responding, re-run the script.
		// Catalog install commands (apt-get, npm install, curl downloads)
		// are idempotent — they no-op when already-installed — and the
		// trailing launch portion respawns the app.
		//
		// Without this, every container restart left agent-ops idle and
		// the ALB target group went unhealthy (502 / connection refused).
		if appIsListening(ctx, cfg) {
			return nil // app is up — really done, skip
		}
		slog.Info("bootstrap: marker exists but app port not listening — re-running script (container restart, app process gone with previous container)",
			"app_port", os.Getenv("AGENT_OPS_APP_PORT"))
		// fall through to the re-run path. finalizeBootstrapSuccess at
		// the end of the run will rewrite the marker (idempotent).
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("bootstrap.MaybeRunFirstRun: stat marker: %w", err)
	}

	// Script mode short-circuit. Skips the LLM entirely — `bash -c <script>`
	// in-process. We still respect cfg.Bootstrap when present (for the
	// verifier / fact-store), but the script itself runs verbatim from env.
	//
	// Catalog scripts often end with a `curl localhost:<port>` smoke test
	// that's racy against slow-starting apps (gastown, n8n with many nodes,
	// etc.) — if the script's final smoke-check fails but the declared port
	// is actually accepting connections, we still register the port and
	// mark bootstrap successful. The ALB has its own health check + the
	// agent-ops MCP layer can re-trigger setup later if the app crashes.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_MODE")), "script") {
		scriptErr := runScriptBootstrap(ctx, cfg, store)
		if scriptErr != nil && !appIsListening(ctx, cfg) {
			return scriptErr
		}
		if scriptErr != nil {
			slog.Warn("bootstrap: script exited non-zero but app port is open, treating as success",
				"err", scriptErr.Error())
			_, _ = store.AddFact(ctx, "bootstrap",
				"script_exit_nonzero_but_port_open at "+time.Now().UTC().Format(time.RFC3339)+
					": "+scriptErr.Error())
		}
		return finalizeBootstrapSuccess(ctx, cfg, marker)
	}

	// Cheatsheet mode (0.3.4+) — LLM-driven install with catalog hints.
	// Same final-state contract as script-mode: if the agent gets the app
	// listening on AGENT_OPS_APP_PORT we treat it as success regardless of
	// whether the model's own self-report claims success (LLMs lie about
	// installs that crashed at the last second; the open TCP socket is
	// the source of truth). On any agent failure we still check the port
	// before declaring bootstrap failed, matching the script-mode
	// "exit-nonzero-but-port-open" tolerance.
	if isCheatsheetMode() {
		_, runErr := runCheatsheetBootstrap(ctx, cfg, sched, store)
		if runErr != nil && !appIsListening(ctx, cfg) {
			return runErr
		}
		if runErr != nil {
			slog.Warn("bootstrap: cheatsheet run errored but app port is open, treating as success",
				"err", runErr.Error())
			_, _ = store.AddFact(ctx, "cheatsheet",
				"agent_errored_but_port_open at "+time.Now().UTC().Format(time.RFC3339)+
					": "+runErr.Error())
		}
		return finalizeBootstrapSuccess(ctx, cfg, marker)
	}

	if cfg.Bootstrap == nil {
		// AGENT_OPS_BOOTSTRAP_SCRIPT was set but MODE was not "script" and no
		// agent-mode Bootstrap exists. Defer to operator — don't pick a path.
		return nil
	}

	// Persist the bootstrap as a normal task so MCP can re-run it ad-hoc.
	t := state.Task{
		Name:    cfg.Bootstrap.Name,
		Cron:    "@yearly", // never auto-fires; bootstrap is manual-only thereafter
		Prompt:  cfg.Bootstrap.Prompt,
		Tools:   cfg.Bootstrap.Tools,
		Enabled: false,
	}
	if err := store.UpsertTask(ctx, t); err != nil {
		return err
	}

	// Trigger one synchronous run. Pass through the scheduler so the same
	// path is used as RunNow + tests.
	slog.Info("bootstrap: invoking first-run task", "name", t.Name)
	run, err := sched.RunNow(ctx, t.Name, t.Prompt)
	if err != nil {
		// Don't write the marker on failure — operator may want to retry
		// once they fix what was broken.
		return fmt.Errorf("bootstrap: first-run task failed: %w", err)
	}

	// Surface what the agent actually did. Without this the daemon goes
	// silent between "mcp token ready" and "mcp server listening" while
	// the LLM works, and the operator sees nothing in CloudWatch even
	// after the run completes — the result lives only in SQLite. Logging
	// the post-run summary is the cheapest way to make container logs
	// useful for inspecting bootstrap outcomes.
	summary := strings.TrimSpace(run.Summary)
	if summary == "" {
		summary = "(no summary returned by bootstrap agent)"
	}
	slog.Info("bootstrap: first-run task complete",
		"run_id", run.ID,
		"status", run.Status,
		"tool_calls", run.ToolCalls,
		"cost_usd_micro", run.CostUSDMicro,
		"summary", truncate(summary, 1200),
		"error", run.Error,
	)

	if _, addErr := store.AddFact(ctx, "bootstrap",
		"Initial setup completed at "+time.Now().UTC().Format(time.RFC3339)+
			": "+truncate(summary, 600)); addErr != nil {
		// Non-fatal — bootstrap succeeded, just couldn't write the fact.
		// Log into the run's log so it's discoverable.
		_ = store.AppendRunLog(ctx, run.ID, 99999, "warn",
			"could not persist bootstrap fact: "+addErr.Error())
	}

	// Optional verifier pass. The main agent sometimes lies about success
	// (e.g. n8n install crashed mid-way, agent reports "done"). If the
	// operator configured verify_prompt, run a second pass — typically a
	// cheaper model — that independently re-checks via shell and emits
	// strict JSON. pass=false skips writing the marker so the next
	// container start retries the whole bootstrap. We don't fail the
	// daemon — partial success + visible diagnostics > daemon crashloop.
	if strings.TrimSpace(cfg.Bootstrap.VerifyPrompt) != "" {
		ok := runVerifier(ctx, cfg, sched, store, t.Name)
		if !ok {
			// Don't write marker. Operator (or next restart) re-triggers.
			return nil
		}
	}

	return finalizeBootstrapSuccess(ctx, cfg, marker)
}

// finalizeBootstrapSuccess wires up the port-register handshake (so the ALB
// can route `<id>.apps.zibby.dev` → the installed app's port) and writes
// the bootstrap.done marker so subsequent daemon restarts skip re-running.
// Shared between agent-mode and script-mode bootstrap paths.
func finalizeBootstrapSuccess(ctx context.Context, cfg *config.Config, marker string) error {
	// In script mode the app process is typically backgrounded via `nohup`
	// or `&` from the install script, so /proc/net/tcp may not yet show the
	// listen socket when we hit this line. AGENT_OPS_APP_PORT (passed from
	// the catalog) makes RegisterPortIfNeeded skip the scan entirely; if
	// it's unset we fall back to a short poll loop so the legacy /proc scan
	// path still works.
	if strings.TrimSpace(os.Getenv("AGENT_OPS_APP_PORT")) == "" {
		waitForAnyListener(ctx, parseMCPPort(cfg.MCP.ListenAddr), 15*time.Second)
	}

	// Tell the Zibby control plane (if integrated) which port the
	// installed app picked, so ALB host-routing can be wired. Silent no-op
	// when ZIBBY_API_BASE_URL / INSTANCE_ID / AGENT_OPS_TOKEN aren't set.
	mcpPort := parseMCPPort(cfg.MCP.ListenAddr)
	zibby.RegisterPortIfNeeded(ctx, mcpPort)

	return os.WriteFile(marker, []byte("ok"), 0o600)
	// the marker has no business value beyond "we've been here" — its
	// presence alone keeps subsequent restarts idempotent.
}

// appIsListening polls AGENT_OPS_APP_PORT for up to 30s, returning true once
// something accepts a TCP connect. Used to gracefully recover from buggy
// catalog smoke tests that race the app's startup (`curl localhost:8080`
// before the app is bound). When AGENT_OPS_APP_PORT isn't set, falls back
// to "any listener besides our own MCP port is up".
func appIsListening(ctx context.Context, cfg *config.Config) bool {
	mcpPort := parseMCPPort(cfg.MCP.ListenAddr)
	raw := strings.TrimSpace(os.Getenv("AGENT_OPS_APP_PORT"))
	var declaredPort int
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n < 65536 {
			declaredPort = n
		}
	}
	// 2-minute budget — gastown, n8n-with-many-nodes, and grafana commonly
	// take 60-90s to first-bind their HTTP listener after the install script
	// returns. ALB health checks pick up failures past this point.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if declaredPort > 0 {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", declaredPort), 1*time.Second)
			if err == nil {
				conn.Close()
				return true
			}
		} else if hasNonMCPListener(mcpPort) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}

// waitForAnyListener polls /proc/net/tcp{,6} until at least one LISTEN port
// other than skipPort is visible, or the budget runs out. Best-effort — if
// we time out we still call RegisterPortIfNeeded so its error surfaces.
func waitForAnyListener(ctx context.Context, skipPort int, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if hasNonMCPListener(skipPort) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func hasNonMCPListener(skipPort int) bool {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if i == 0 || strings.TrimSpace(line) == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" {
				continue
			}
			colon := strings.LastIndex(fields[1], ":")
			if colon < 0 {
				continue
			}
			n, err := strconv.ParseInt(fields[1][colon+1:], 16, 32)
			if err != nil {
				continue
			}
			if int(n) == skipPort {
				continue
			}
			return true
		}
	}
	return false
}

// runScriptBootstrap is the LLM-less bootstrap path. It execs the verbatim
// script from AGENT_OPS_BOOTSTRAP_SCRIPT via `bash -c` and streams stdout +
// stderr to the daemon logger so CloudWatch captures every command. Honors
// AGENT_OPS_BOOTSTRAP_TIMEOUT (default 30m) — exceeded → SIGKILL + error.
//
// Critical assumption: the script is responsible for daemonizing the
// installed app (typically `nohup ... &` at the end). Once `bash -c`
// returns 0 we treat the bootstrap as successful and proceed to
// port-register. If the catalog declares AGENT_OPS_APP_PORT we skip the
// /proc scan and use that — the catalog knows the answer.
func runScriptBootstrap(ctx context.Context, cfg *config.Config, store *state.Store) error {
	script := os.Getenv("AGENT_OPS_BOOTSTRAP_SCRIPT")
	if strings.TrimSpace(script) == "" {
		return errors.New("bootstrap: AGENT_OPS_BOOTSTRAP_MODE=script but AGENT_OPS_BOOTSTRAP_SCRIPT is empty")
	}

	timeout := scriptBootstrapTimeout(cfg)
	slog.Info("bootstrap: running script mode",
		"timeout", timeout,
		"script_bytes", len(script),
		"app_type", os.Getenv("AGENT_OPS_APP_TYPE"),
		"app_port", os.Getenv("AGENT_OPS_APP_PORT"),
	)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	startedAt := time.Now()

	cmd := exec.CommandContext(runCtx, "bash", "-c", script)
	// Inherit env so the script sees AGENT_OPS_APP_PORT, INSTANCE_ID, the
	// per-app HOME, etc. Override Cancel to send SIGTERM first (giving
	// child processes a chance to clean up) before the default SIGKILL on
	// timeout. WaitDelay = 5s — if SIGTERM doesn't take, escalate.
	cmd.Env = os.Environ()
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	// Detach from the daemon's process group so a `nohup ... &` inside the
	// script keeps running after `bash -c` returns. Without Setpgid the
	// app would be in our pgrp and could get SIGKILLed on daemon shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("bootstrap: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("bootstrap: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("bootstrap: bash start: %w", err)
	}

	// Stream both pipes to slog. Capture the last 32KB of combined output
	// for the fact store so the operator can `mcp list-facts` and see what
	// happened without spelunking CloudWatch.
	var tail tailBuffer
	var wg sync.WaitGroup
	wg.Add(2)
	go streamPipe(&wg, stdoutPipe, "stdout", &tail)
	go streamPipe(&wg, stderrPipe, "stderr", &tail)

	waitErr := cmd.Wait()
	wg.Wait()

	exitCode := cmd.ProcessState.ExitCode()
	tailStr := tail.String()
	slog.Info("bootstrap: script complete",
		"exit_code", exitCode,
		"duration", time.Since(startedAt).String(),
		"tail_bytes", len(tailStr),
	)

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		_, _ = store.AddFact(ctx, "bootstrap",
			"script_timeout at "+time.Now().UTC().Format(time.RFC3339)+
				" after "+timeout.String()+"; tail: "+truncate(tailStr, 800))
		return fmt.Errorf("bootstrap: script timed out after %s", timeout)
	}
	if waitErr != nil {
		_, _ = store.AddFact(ctx, "bootstrap",
			"script_failed at "+time.Now().UTC().Format(time.RFC3339)+
				" exit="+strconv.Itoa(exitCode)+"; tail: "+truncate(tailStr, 800))
		return fmt.Errorf("bootstrap: script exit %d: %w", exitCode, waitErr)
	}

	_, _ = store.AddFact(ctx, "bootstrap",
		"script_ok at "+time.Now().UTC().Format(time.RFC3339)+
			" exit=0; tail: "+truncate(tailStr, 600))
	return nil
}

// scriptBootstrapTimeout reads AGENT_OPS_BOOTSTRAP_TIMEOUT (e.g. "10m",
// "1200s") with a 30m fallback. The previous LLM path enforced timeout via
// task.TaskTimeout; script mode reuses the same env hook so operators have
// one knob.
//
// Bumped from 20m → 30m in 0.3.6 to match the new agent-mode bootstrap
// default. Heavy installs (n8n's ~500MB npm install hits 25 min on slow
// CDN days) blew through the old 20m baseline; bumping gives those
// one-shot installs room to finish without operator override.
func scriptBootstrapTimeout(cfg *config.Config) time.Duration {
	if v := strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	if cfg.Agent.TaskTimeout > 0 {
		// Script installs are slower than LLM-reasoning tasks (5min apt-get
		// + npm install) — give them 2x the per-task budget.
		return 2 * cfg.Agent.TaskTimeout
	}
	return 30 * time.Minute
}

// streamPipe copies the child's stdout/stderr to slog line-by-line and
// mirrors a bounded tail into buf for fact-store retention.
func streamPipe(wg *sync.WaitGroup, r io.ReadCloser, stream string, buf *tailBuffer) {
	defer wg.Done()
	defer r.Close()
	sc := bufio.NewScanner(r)
	// 1MB buffer — npm install lines can blow past the default 64KB.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		buf.Write(line + "\n")
		slog.Info("bootstrap-script", "stream", stream, "line", line)
	}
	if err := sc.Err(); err != nil {
		slog.Warn("bootstrap-script: scan error", "stream", stream, "err", err.Error())
	}
}

// tailBuffer is a thread-safe ring-ish buffer holding the last ~32KB of
// streamed output. Cheap to write, dropped silently when full enough.
type tailBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
	cap int
}

func (t *tailBuffer) Write(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cap == 0 {
		t.cap = 32 * 1024
	}
	t.buf.WriteString(s)
	// Once we exceed 2x cap, snip the front. Keeps the recent tail without
	// shuffling on every line write.
	if t.buf.Len() > 2*t.cap {
		s := t.buf.String()
		t.buf.Reset()
		t.buf.WriteString(s[len(s)-t.cap:])
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.buf.String()
	if len(s) > t.cap {
		return s[len(s)-t.cap:]
	}
	return s
}

// verifierResult is the strict JSON shape the verifier prompt asks for.
// We tolerate prose around the JSON — extractJSONObject finds the first
// brace-delimited block before unmarshaling.
type verifierResult struct {
	Pass       bool   `json:"pass"`
	Evidence   string `json:"evidence"`
	FailReason string `json:"fail_reason"`
}

// runVerifier executes the verify_prompt as a one-shot task and parses the
// JSON answer. Returns true iff the verifier reported pass=true. Any error
// (run failure, unparseable output, etc.) is treated as pass=false — we
// don't claim success on ambiguous signal.
func runVerifier(
	ctx context.Context,
	cfg *config.Config,
	sched *scheduler.Scheduler,
	store *state.Store,
	bootstrapTaskName string,
) bool {
	verifyName := bootstrapTaskName + ".verify"

	// Persist as a disabled task so we can route through sched.RunNow (which
	// looks up Tools, prompt, etc. by name from the store) and so the MCP
	// layer can introspect verifier runs after the fact.
	vt := state.Task{
		Name:    verifyName,
		Cron:    "@yearly", // never auto-fires
		Prompt:  cfg.Bootstrap.VerifyPrompt,
		Tools:   cfg.Bootstrap.Tools,
		Enabled: false,
	}
	if err := store.UpsertTask(ctx, vt); err != nil {
		slog.Warn("bootstrap: verifier upsert failed, skipping verification", "error", err)
		return false
	}
	// Plumb the per-task model override through the same map RunNow reads.
	sched.SetModelOverride(verifyName, cfg.Bootstrap.VerifyModel)

	slog.Info("bootstrap: running verifier pass", "name", verifyName,
		"model", cfg.Bootstrap.VerifyModel)

	vrun, vErr := sched.RunNow(ctx, verifyName, cfg.Bootstrap.VerifyPrompt)
	if vErr != nil {
		slog.Warn("bootstrap: verifier run failed", "error", vErr)
		_, _ = store.AddFact(ctx, "bootstrap",
			"verify_failed: verifier run errored at "+time.Now().UTC().Format(time.RFC3339)+
				": "+vErr.Error())
		return false
	}

	res, parseErr := parseVerifierJSON(vrun.Summary)
	if parseErr != nil {
		slog.Warn("bootstrap: verifier output unparseable, treating as fail",
			"error", parseErr,
			"summary", truncate(vrun.Summary, 400),
		)
		_, _ = store.AddFact(ctx, "bootstrap",
			"verify_failed: could not parse verifier JSON at "+time.Now().UTC().Format(time.RFC3339)+
				": "+parseErr.Error())
		return false
	}

	slog.Info("bootstrap: verifier complete",
		"pass", res.Pass,
		"evidence", res.Evidence,
		"fail_reason", res.FailReason,
		"run_id", vrun.ID,
	)

	if !res.Pass {
		_, _ = store.AddFact(ctx, "bootstrap",
			"verify_failed at "+time.Now().UTC().Format(time.RFC3339)+
				": "+res.FailReason+" (evidence: "+res.Evidence+")")
		return false
	}

	_, _ = store.AddFact(ctx, "bootstrap",
		"verify_passed at "+time.Now().UTC().Format(time.RFC3339)+
			": "+res.Evidence)
	return true
}

// parseVerifierJSON extracts the first {...} block from s and unmarshals it
// into a verifierResult. Tolerates leading/trailing prose because LLMs love
// to add "Sure! Here's the JSON:" no matter how strict the prompt is.
func parseVerifierJSON(s string) (verifierResult, error) {
	raw, ok := extractJSONObject(s)
	if !ok {
		return verifierResult{}, errors.New("no JSON object found in verifier output")
	}
	var v verifierResult
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return verifierResult{}, fmt.Errorf("unmarshal: %w", err)
	}
	return v, nil
}

// extractJSONObject returns the substring from the first '{' to its matching
// closing '}', balancing braces and ignoring braces inside JSON strings.
// Returns (raw, true) on success.
func extractJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseMCPPort extracts the numeric port from cfg.MCP.ListenAddr, which is
// typically ":7842" or "0.0.0.0:7842". Returns 7842 as a safe default when
// parsing fails — only used to exclude the daemon's own listener when
// scanning for the installed app's port.
func parseMCPPort(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 7842
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 7842
	}
	return n
}

// We use task.TriggerBootstrap directly nowhere in this file (RunNow tags it
// as TriggerManual), but the constant is re-exported here so future cluster
// code can label cluster-induced bootstraps separately.
var _ = task.TriggerBootstrap

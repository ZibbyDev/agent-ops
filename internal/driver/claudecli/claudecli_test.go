// Copyright 2026 Zibby Lab. Apache-2.0.

package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZibbyHQ/agent-ops/internal/driver"
)

// `errors` removed — only used by the deleted nilOrNot helper.

// fakeClaudeCLI writes a shell script in tmpDir that, when invoked,
// emits `stdoutJSON` to stdout, `stderrText` to stderr, then exits with
// `exitCode`. The script ignores its argv — tests inspect args via
// argsFile if they need to verify the driver passed the right flags.
//
// Returns the absolute path to the fake binary (set on Driver.Binary).
func fakeClaudeCLI(t *testing.T, stdoutJSON, stderrText string, exitCode int) string {
	t.Helper()
	tmpDir := t.TempDir()
	script := filepath.Join(tmpDir, "claude")
	body := "#!/usr/bin/env bash\n"
	if stderrText != "" {
		body += "cat >&2 <<'EOF_STDERR'\n" + stderrText + "\nEOF_STDERR\n"
	}
	if stdoutJSON != "" {
		body += "cat <<'EOF_STDOUT'\n" + stdoutJSON + "\nEOF_STDOUT\n"
	}
	body += "exit " + intToStr(exitCode) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return script
}

func intToStr(n int) string {
	// Avoid importing strconv just for this. Range covers process exit codes.
	if n < 0 || n > 255 {
		return "0"
	}
	digits := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// captureLog installs a JSON slog handler that writes to the returned
// buffer. Restores the default slog logger when the test ends. Tests
// can decode the buffer line-by-line to assert specific log records.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// logRecords decodes the JSONL buffer into a slice of records, one per
// line. Test helper — no need to leak the JSON shape into every test.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// findRecord returns the first log record whose msg matches `msgPrefix`.
// Fails the test if not found.
func findRecord(t *testing.T, records []map[string]any, msgPrefix string) map[string]any {
	t.Helper()
	for _, r := range records {
		if m, ok := r["msg"].(string); ok && strings.HasPrefix(m, msgPrefix) {
			return r
		}
	}
	t.Fatalf("no log record found with msg prefix %q (had %d records)", msgPrefix, len(records))
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────

func TestRun_SuccessLogsConversation(t *testing.T) {
	// Claude CLI's `--output-format json` post-run JSON. The driver
	// parses this AND (post-Wave-B-log-fix) emits a structured slog
	// record so the conversation is visible in CloudWatch.
	stdoutJSON := `{"result":"n8n responded HTTP 200 on port 5678","total_cost_usd":0.0042,"num_turns":3,"is_error":false}`
	bin := fakeClaudeCLI(t, stdoutJSON, "", 0)
	logBuf := captureLog(t)

	d := &Driver{Binary: bin}
	res, err := d.Run(context.Background(), driver.Request{
		SystemPrompt: "You are a health checker.",
		UserPrompt:   "Curl localhost:5678 and report.",
		MaxToolCalls: 5,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// 1. Driver Result is parsed correctly.
	if got, want := res.FinalMessage, "n8n responded HTTP 200 on port 5678"; got != want {
		t.Errorf("FinalMessage = %q, want %q", got, want)
	}
	if got, want := res.ToolCalls, 3; got != want {
		t.Errorf("ToolCalls = %d, want %d", got, want)
	}
	if got, want := res.CostUSDMicro, int64(4200); got != want {
		t.Errorf("CostUSDMicro = %d, want %d (0.0042 * 1_000_000)", got, want)
	}
	if res.Error != "" {
		t.Errorf("Error should be empty on success, got %q", res.Error)
	}

	// 2. Spawning log is emitted (pre-existing behaviour — keep it).
	records := logRecords(t, logBuf)
	spawn := findRecord(t, records, "claudecli: spawning subprocess")
	if spawn["max_turns"] != float64(5) {
		t.Errorf("spawning log max_turns = %v, want 5", spawn["max_turns"])
	}

	// 3. NEW: conversation-complete log is emitted with prompt + result
	//    + cost. This is the fix the user asked for — without this log,
	//    Claude's response is invisible outside the container.
	convo := findRecord(t, records, "claudecli: conversation complete")
	if !strings.Contains(convo["user_prompt"].(string), "Curl localhost:5678") {
		t.Errorf("user_prompt missing in convo log: %v", convo["user_prompt"])
	}
	if !strings.Contains(convo["system_prompt"].(string), "health checker") {
		t.Errorf("system_prompt missing in convo log: %v", convo["system_prompt"])
	}
	if convo["result"] != "n8n responded HTTP 200 on port 5678" {
		t.Errorf("result missing/wrong in convo log: %v", convo["result"])
	}
	if convo["num_turns"] != float64(3) {
		t.Errorf("num_turns = %v, want 3", convo["num_turns"])
	}
	if convo["total_cost_usd"] != 0.0042 {
		t.Errorf("total_cost_usd = %v, want 0.0042", convo["total_cost_usd"])
	}
	if convo["is_error"] != false {
		t.Errorf("is_error = %v, want false", convo["is_error"])
	}
}

func TestRun_TruncatesLongFields(t *testing.T) {
	// Truncation caps were bumped in 0.1.19 — the previous 1.5K cap on
	// system_prompt was clipping users' MISSION + KNOWN FACTS blocks
	// mid-section in the dashboard. New caps: 8K/4K/8K/0.8K for
	// system/user/result/stderr. CloudWatch event ceiling is 256KB so
	// worst-case ~21KB is well under budget.
	longResult := strings.Repeat("x", 16000)
	stdoutJSON := `{"result":"` + longResult + `","total_cost_usd":0,"num_turns":1,"is_error":false}`
	bin := fakeClaudeCLI(t, stdoutJSON, "", 0)
	logBuf := captureLog(t)

	d := &Driver{Binary: bin}
	_, err := d.Run(context.Background(), driver.Request{
		SystemPrompt: strings.Repeat("s", 12000),
		UserPrompt:   strings.Repeat("u", 8000),
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	convo := findRecord(t, logRecords(t, logBuf), "claudecli: conversation complete")

	// 8k result cap.
	if got := len(convo["result"].(string)); got > 8000+12 /* "...(16000)" suffix room */ {
		t.Errorf("result length = %d, expected ≈8000-ish (truncated)", got)
	}
	// 8k system_prompt cap.
	if got := len(convo["system_prompt"].(string)); got > 8000+12 {
		t.Errorf("system_prompt length = %d, expected ≈8000-ish (truncated)", got)
	}
	// 4k user_prompt cap.
	if got := len(convo["user_prompt"].(string)); got > 4000+12 {
		t.Errorf("user_prompt length = %d, expected ≈4000-ish (truncated)", got)
	}
}

func TestRun_SubprocessFailure_LogsStderrAndReturnsError(t *testing.T) {
	// Pre-existing behaviour — keep it green.
	bin := fakeClaudeCLI(t, "", "claude: not authenticated", 1)
	logBuf := captureLog(t)

	d := &Driver{Binary: bin}
	res, err := d.Run(context.Background(), driver.Request{UserPrompt: "x"})
	if err != nil {
		t.Fatalf("Run unexpectedly returned error: %v (should surface error on Result, not as Go error)", err)
	}
	if res.Error == "" {
		t.Error("expected res.Error to be set on non-zero exit")
	}
	if !strings.Contains(res.Error, "not authenticated") {
		t.Errorf("res.Error should include stderr; got %q", res.Error)
	}
	failRec := findRecord(t, logRecords(t, logBuf), "claudecli: subprocess failed")
	if !strings.Contains(failRec["stderr"].(string), "not authenticated") {
		t.Errorf("subprocess failed log missing stderr; got %v", failRec["stderr"])
	}
}

func TestRun_UnparsableStdout_FallsBackAndLogsWarning(t *testing.T) {
	// If Claude CLI's output format changes (new fields, missing top-
	// level keys), the driver returns the raw bytes as FinalMessage but
	// shouldn't crash. Also: emit a Warn slog so operators see it before
	// users start filing tickets.
	bin := fakeClaudeCLI(t, "not json at all", "", 0)
	logBuf := captureLog(t)

	d := &Driver{Binary: bin}
	res, err := d.Run(context.Background(), driver.Request{UserPrompt: "x"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.FinalMessage != "not json at all" {
		t.Errorf("FinalMessage should fall back to raw stdout; got %q", res.FinalMessage)
	}
	if res.Error == "" {
		t.Error("Error should be set on parse failure")
	}
	warn := findRecord(t, logRecords(t, logBuf), "claudecli: stdout parse failed")
	if !strings.Contains(warn["raw"].(string), "not json") {
		t.Errorf("parse-fail warn log missing raw stdout: %v", warn["raw"])
	}
}

func TestRun_BinaryNotFound_ReturnsError(t *testing.T) {
	// If `claude` isn't on PATH / at the configured path, the driver
	// should report it as a failure (either Go-error or Result.Error)
	// without panicking. Failure mode shape doesn't matter to caller —
	// just that SOMETHING flagged the problem.
	d := &Driver{Binary: "/nonexistent/path/claude-binary"}
	res, err := d.Run(context.Background(), driver.Request{UserPrompt: "x"})
	if err == nil && res.Error == "" {
		t.Error("expected an error path when binary is missing, got success")
	}
}

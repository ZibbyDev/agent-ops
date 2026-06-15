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
	"time"

	"github.com/ZibbyDev/agent-ops/internal/driver"
)

// fakeClaudeCLI writes a shell script in tmpDir that, when invoked,
// emits `stdoutNDJSON` to stdout, `stderrText` to stderr, then exits
// with `exitCode`. The script ignores its argv — tests inspect args
// via argsFile if they need to verify the driver passed the right flags.
//
// stdoutNDJSON is expected to be one JSON event per line
// (--output-format=stream-json shape) — pass an empty string to skip.
//
// Returns the absolute path to the fake binary (set on Driver.Binary).
func fakeClaudeCLI(t *testing.T, stdoutNDJSON, stderrText string, exitCode int) string {
	t.Helper()
	tmpDir := t.TempDir()
	script := filepath.Join(tmpDir, "claude")
	body := "#!/usr/bin/env bash\n"
	if stderrText != "" {
		body += "cat >&2 <<'EOF_STDERR'\n" + stderrText + "\nEOF_STDERR\n"
	}
	if stdoutNDJSON != "" {
		body += "cat <<'EOF_STDOUT'\n" + stdoutNDJSON + "\nEOF_STDOUT\n"
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

// findAllRecords returns every log record whose msg matches msgPrefix
// (in insertion order). Used by per-turn tests that expect MANY matches.
func findAllRecords(records []map[string]any, msgPrefix string) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if m, ok := r["msg"].(string); ok && strings.HasPrefix(m, msgPrefix) {
			out = append(out, r)
		}
	}
	return out
}

// ndjsonResult is a convenience for tests: produces a single-line
// {"type":"result"} NDJSON event with the given fields.
func ndjsonResult(result string, numTurns int, costUSD float64, isError bool) string {
	r := map[string]any{
		"type":           "result",
		"subtype":        "success",
		"result":         result,
		"num_turns":      numTurns,
		"total_cost_usd": costUSD,
		"is_error":       isError,
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// ─────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────

func TestRun_SuccessLogsConversation(t *testing.T) {
	// Driver consumes the stream-json NDJSON event stream now (0.3.3+).
	// Only the terminal {"type":"result"} event populates Driver.Result;
	// intermediate events drive the per-turn progress log.
	stdoutNDJSON := ndjsonResult("n8n responded HTTP 200 on port 5678", 3, 0.0042, false)
	bin := fakeClaudeCLI(t, stdoutNDJSON, "", 0)
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

	// 3. conversation-complete log is emitted with prompt + result
	//    + cost. Without this log, Claude's response is invisible
	//    outside the container.
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
	stdoutNDJSON := ndjsonResult(longResult, 1, 0, false)
	bin := fakeClaudeCLI(t, stdoutNDJSON, "", 0)
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
	// If Claude CLI's output format changes (new event types, missing
	// terminal result event), the driver returns the raw bytes as
	// FinalMessage but shouldn't crash. Also: emit a Warn slog so
	// operators see it before users start filing tickets.
	bin := fakeClaudeCLI(t, "not json at all", "", 0)
	logBuf := captureLog(t)

	d := &Driver{Binary: bin}
	res, err := d.Run(context.Background(), driver.Request{UserPrompt: "x"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(res.FinalMessage, "not json at all") {
		t.Errorf("FinalMessage should fall back to raw stdout; got %q", res.FinalMessage)
	}
	if res.Error == "" {
		t.Error("Error should be set on parse failure")
	}
	// Stream ended without a result event — driver emits the
	// "no result event in stream" warning.
	warn := findRecord(t, logRecords(t, logBuf), "claudecli: no result event")
	if !strings.Contains(warn["raw"].(string), "not json") {
		t.Errorf("no-result-event warn log missing raw stdout: %v", warn["raw"])
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

// ─────────────────────────────────────────────────────────────────────
// Per-turn progress (0.3.3 new behaviour)
// ─────────────────────────────────────────────────────────────────────

// realisticStream is the NDJSON event sequence the bundled Claude Code
// CLI 2.1.158 actually emits for a 3-turn run (verified by hand-piping
// `claude --print "run 'echo hi' via Bash then 'pwd' via Bash"
// --output-format=stream-json --verbose`). Used to assert the parser
// emits one "claudecli: turn" per assistant message + one
// "claudecli: tool_result" per tool_result.
const realisticStream = `{"type":"system","subtype":"init","cwd":"/x","session_id":"s1","model":"claude-opus-4-8","permissionMode":"acceptEdits","claude_code_version":"2.1.158"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I'll run both commands."}]},"session_id":"s1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_A","name":"Bash","input":{"command":"echo hi"}}]},"session_id":"s1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_B","name":"Read","input":{"file_path":"/etc/hosts"}}]},"session_id":"s1"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_A","type":"tool_result","content":"hi","is_error":false}]},"session_id":"s1"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_B","type":"tool_result","content":"127.0.0.1 localhost","is_error":false}]},"session_id":"s1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]},"session_id":"s1"}
{"type":"result","subtype":"success","is_error":false,"num_turns":4,"result":"Done.","total_cost_usd":0.057,"session_id":"s1"}`

func TestRun_PerTurnProgressLogged(t *testing.T) {
	// The whole point of 0.3.3: operators watching CloudWatch see
	// per-turn progress during long installs. Assert one "claudecli:
	// turn" record per assistant message + one "claudecli: tool_result"
	// per tool_result, with the right field names ops will grep on:
	// n, max, tool, elapsed_ms.
	bin := fakeClaudeCLI(t, realisticStream, "", 0)
	logBuf := captureLog(t)

	d := &Driver{Binary: bin}
	res, err := d.Run(context.Background(), driver.Request{
		UserPrompt:   "run echo hi and read /etc/hosts",
		MaxToolCalls: 25,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected Result.Error: %q", res.Error)
	}

	records := logRecords(t, logBuf)

	turns := findAllRecords(records, "claudecli: turn")
	// 4 assistant messages in the stream → 4 turn records.
	if len(turns) != 4 {
		t.Fatalf("expected 4 turn records, got %d", len(turns))
	}

	// Field shape check on the first turn.
	first := turns[0]
	if first["n"] != float64(1) {
		t.Errorf("turn[0].n = %v, want 1", first["n"])
	}
	if first["max"] != float64(25) {
		t.Errorf("turn[0].max = %v, want 25", first["max"])
	}
	if _, ok := first["elapsed_ms"]; !ok {
		t.Errorf("turn[0] missing elapsed_ms")
	}
	if first["text"] != "I'll run both commands." {
		t.Errorf("turn[0].text = %v, want \"I'll run both commands.\"", first["text"])
	}

	// Second turn: tool_use Bash.
	if turns[1]["tool"] != "Bash" {
		t.Errorf("turn[1].tool = %v, want Bash", turns[1]["tool"])
	}
	// Third turn: tool_use Read.
	if turns[2]["tool"] != "Read" {
		t.Errorf("turn[2].tool = %v, want Read", turns[2]["tool"])
	}

	// tool_result records (2 in this stream).
	results := findAllRecords(records, "claudecli: tool_result")
	if len(results) != 2 {
		t.Fatalf("expected 2 tool_result records, got %d", len(results))
	}
	if results[0]["tool"] != "Read" {
		// last_tool at the time the first result arrives = the most-
		// recent assistant tool_use, which is Read (the Bash call was
		// followed by a Read call before either result was returned).
		t.Errorf("tool_result[0].tool = %v, want Read (last_tool tracking)", results[0]["tool"])
	}
	if results[0]["output"] != "hi" {
		t.Errorf("tool_result[0].output = %v, want hi", results[0]["output"])
	}
	if results[0]["is_error"] != false {
		t.Errorf("tool_result[0].is_error = %v, want false", results[0]["is_error"])
	}

	// Final result is still parsed correctly.
	if res.FinalMessage != "Done." {
		t.Errorf("FinalMessage = %q, want Done.", res.FinalMessage)
	}
	if res.ToolCalls != 4 {
		t.Errorf("ToolCalls = %d, want 4", res.ToolCalls)
	}
	if res.CostUSDMicro != int64(57000) {
		t.Errorf("CostUSDMicro = %d, want 57000 (0.057 * 1e6)", res.CostUSDMicro)
	}
}

func TestParseStream_DefaultsMaxTurnsForLog(t *testing.T) {
	// When the caller doesn't set MaxToolCalls, the per-turn log should
	// show "max=25" (CLI default) — not "max=0". That's what makes
	// "turn N/25" actually informative.
	bin := fakeClaudeCLI(t, realisticStream, "", 0)
	logBuf := captureLog(t)

	d := &Driver{Binary: bin}
	if _, err := d.Run(context.Background(), driver.Request{UserPrompt: "x"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	turns := findAllRecords(logRecords(t, logBuf), "claudecli: turn")
	if len(turns) == 0 {
		t.Fatal("expected at least one turn record")
	}
	if turns[0]["max"] != float64(defaultMaxTurns) {
		t.Errorf("default max in turn log = %v, want %d", turns[0]["max"], defaultMaxTurns)
	}
}

// TestParseStream_DirectlyOnNDJSON exercises parseStream against a
// crafted NDJSON sample without spawning a subprocess. Faster + easier
// to assert ordering / sliding-tail behaviour on.
func TestParseStream_DirectlyOnNDJSON(t *testing.T) {
	logBuf := captureLog(t)
	out, err := parseStream(strings.NewReader(realisticStream), 25, time.Now())
	if err != nil {
		t.Fatalf("parseStream err: %v", err)
	}
	if out.result == nil {
		t.Fatal("expected result populated")
	}
	if out.result.Result != "Done." {
		t.Errorf("result.Result = %q", out.result.Result)
	}
	if out.eventCount != 8 {
		t.Errorf("eventCount = %d, want 8", out.eventCount)
	}
	turns := findAllRecords(logRecords(t, logBuf), "claudecli: turn")
	if len(turns) != 4 {
		t.Errorf("turn log count = %d, want 4", len(turns))
	}
}

// TestParseStream_HandlesUnknownEventTypes ensures forward-compat: a
// future CLI version adding new {"type":...} events should not break
// parsing or the per-turn log.
func TestParseStream_HandlesUnknownEventTypes(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"future_event_added_in_3_0","payload":"whatever"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"ok","total_cost_usd":0.001}`,
	}, "\n")

	logBuf := captureLog(t)
	out, err := parseStream(strings.NewReader(stream), 25, time.Now())
	if err != nil {
		t.Fatalf("parseStream err: %v", err)
	}
	if out.result == nil || out.result.Result != "ok" {
		t.Errorf("result not parsed; got %+v", out.result)
	}
	turns := findAllRecords(logRecords(t, logBuf), "claudecli: turn")
	if len(turns) != 1 {
		t.Errorf("turn count = %d, want 1 (unknown event should be ignored)", len(turns))
	}
}

// Copyright 2026 Zibby Lab. Apache-2.0.

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZibbyDev/agent-ops/internal/driver"
)

// fakeCodexCLI writes a shell script in tmpDir that, when invoked,
// emits `stdoutBlob` to stdout, `stderrText` to stderr, then exits with
// `exitCode`. The script ignores its argv — tests inspect args separately
// only if needed.
//
// Returns the absolute path to the fake binary (set on Driver.Binary).
func fakeCodexCLI(t *testing.T, stdoutBlob, stderrText string, exitCode int) string {
	t.Helper()
	tmpDir := t.TempDir()
	script := filepath.Join(tmpDir, "codex")
	body := "#!/usr/bin/env bash\n"
	if stderrText != "" {
		body += "cat >&2 <<'EOF_STDERR'\n" + stderrText + "\nEOF_STDERR\n"
	}
	if stdoutBlob != "" {
		body += "cat <<'EOF_STDOUT'\n" + stdoutBlob + "\nEOF_STDOUT\n"
	}
	body += "exit " + intToStr(exitCode) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return script
}

func intToStr(n int) string {
	if n < 0 || n > 255 {
		return "0"
	}
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// captureLog installs a JSON slog handler that writes to the returned
// buffer. Restores the default slog logger when the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

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

func TestRun_HappyPath_ParsesNdjsonAndLogsConversation(t *testing.T) {
	// Mimic the Codex NDJSON event stream. Two agent_message items get
	// concatenated. Two command_execution items + one mcp_tool_call →
	// tool_calls = 3. Two turn.completed events; the LAST one's usage
	// is the authoritative cumulative total.
	stdoutBlob := strings.Join([]string{
		`{"type":"item.completed","item":{"item_type":"command_execution","command":"ls"}}`,
		`{"type":"item.completed","item":{"item_type":"agent_message","text":"Checked the directory. "}}`,
		`{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":0,"output_tokens":40}}`,
		`{"type":"item.completed","item":{"item_type":"command_execution","command":"curl localhost:5678"}}`,
		`{"type":"item.completed","item":{"item_type":"mcp_tool_call","tool":"zibby_workflow"}}`,
		`{"type":"item.completed","item":{"item_type":"agent_message","text":"All healthy."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":340,"cached_input_tokens":80,"output_tokens":95}}`,
	}, "\n")

	bin := fakeCodexCLI(t, stdoutBlob, "", 0)
	logBuf := captureLog(t)

	d := &Driver{Binary: bin}
	res, err := d.Run(context.Background(), driver.Request{
		SystemPrompt: "You are a health checker.",
		UserPrompt:   "Curl localhost:5678 and report.",
		MaxToolCalls: 5, // not honored by codex v1, but should not break Run
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// 1. agent_message texts concatenated in order.
	if got, want := res.FinalMessage, "Checked the directory. All healthy."; got != want {
		t.Errorf("FinalMessage = %q, want %q", got, want)
	}
	// 2. command_execution(2) + mcp_tool_call(1) = 3 tool calls.
	if got, want := res.ToolCalls, 3; got != want {
		t.Errorf("ToolCalls = %d, want %d", got, want)
	}
	// 3. CostUSDMicro stays 0 — v1 doesn't compute cost.
	if res.CostUSDMicro != 0 {
		t.Errorf("CostUSDMicro = %d, want 0 (v1 doesn't report cost)", res.CostUSDMicro)
	}
	if res.Error != "" {
		t.Errorf("Error should be empty on success, got %q", res.Error)
	}

	records := logRecords(t, logBuf)

	// 4. Spawning log emitted.
	spawn := findRecord(t, records, "codex: spawning subprocess")
	if spawn["sandbox"] != "workspace-write" {
		t.Errorf("spawning log sandbox = %v, want workspace-write", spawn["sandbox"])
	}

	// 5. Conversation-complete log emitted with the canonical fields.
	convo := findRecord(t, records, "codex: conversation complete")
	if !strings.Contains(convo["user_prompt"].(string), "Curl localhost:5678") {
		t.Errorf("user_prompt missing in convo log: %v", convo["user_prompt"])
	}
	if !strings.Contains(convo["system_prompt"].(string), "health checker") {
		t.Errorf("system_prompt missing in convo log: %v", convo["system_prompt"])
	}
	if convo["result"] != "Checked the directory. All healthy." {
		t.Errorf("result missing/wrong in convo log: %v", convo["result"])
	}
	if convo["tool_calls"] != float64(3) {
		t.Errorf("tool_calls = %v, want 3", convo["tool_calls"])
	}
	// Last turn.completed wins for token totals.
	if convo["tokens_input"] != float64(340) {
		t.Errorf("tokens_input = %v, want 340 (cumulative final turn)", convo["tokens_input"])
	}
	if convo["tokens_output"] != float64(95) {
		t.Errorf("tokens_output = %v, want 95 (cumulative final turn)", convo["tokens_output"])
	}
	if convo["is_error"] != false {
		t.Errorf("is_error = %v, want false", convo["is_error"])
	}
}

func TestRun_NonJsonLinesSkipped(t *testing.T) {
	// Codex may print a banner or human-readable hints at startup, and
	// future versions may interleave non-JSON. The parser must skip them
	// silently and still extract the agent_message + tool counts.
	stdoutBlob := strings.Join([]string{
		`Codex CLI v0.135.0 starting...`, // human-readable banner
		``,                                // blank
		`{"type":"item.completed","item":{"item_type":"command_execution","command":"ls"}}`,
		`>>> Running step 1`, // human-readable progress
		`{"type":"item.completed","item":{"item_type":"agent_message","text":"Hello world."}}`,
		`{this is not valid json at all`, // malformed line — must be tolerated
		`{"type":"turn.completed","usage":{"input_tokens":50,"cached_input_tokens":0,"output_tokens":20}}`,
		``, // trailing blank
	}, "\n")

	bin := fakeCodexCLI(t, stdoutBlob, "", 0)
	_ = captureLog(t)

	d := &Driver{Binary: bin}
	res, err := d.Run(context.Background(), driver.Request{
		UserPrompt: "say hi",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got, want := res.FinalMessage, "Hello world."; got != want {
		t.Errorf("FinalMessage = %q, want %q (parser should tolerate non-JSON noise)", got, want)
	}
	if got, want := res.ToolCalls, 1; got != want {
		t.Errorf("ToolCalls = %d, want %d (one command_execution despite noise)", got, want)
	}
	if res.Error != "" {
		t.Errorf("Error should be empty when non-JSON lines are merely interleaved, got %q", res.Error)
	}
}

func TestRun_SubprocessFailure_LogsStderrAndReturnsError(t *testing.T) {
	// Mirrors claudecli's TestRun_SubprocessFailure. Non-zero exit →
	// stderr surfaced on Result.Error and logged at error level.
	bin := fakeCodexCLI(t, "", "codex: OPENAI_API_KEY not set", 1)
	logBuf := captureLog(t)

	d := &Driver{Binary: bin}
	res, err := d.Run(context.Background(), driver.Request{UserPrompt: "x"})
	if err != nil {
		t.Fatalf("Run unexpectedly returned error: %v (should surface on Result)", err)
	}
	if res.Error == "" {
		t.Error("expected res.Error to be set on non-zero exit")
	}
	if !strings.Contains(res.Error, "OPENAI_API_KEY") {
		t.Errorf("res.Error should include stderr; got %q", res.Error)
	}
	failRec := findRecord(t, logRecords(t, logBuf), "codex: subprocess failed")
	if !strings.Contains(failRec["stderr"].(string), "OPENAI_API_KEY") {
		t.Errorf("subprocess failed log missing stderr; got %v", failRec["stderr"])
	}
}

func TestRun_BinaryNotFound(t *testing.T) {
	// Preflight should reject a missing binary clearly. Run itself
	// should also degrade safely if Preflight is bypassed.
	d := &Driver{Binary: "/nonexistent/path/codex-binary"}

	preflightErr := d.Preflight()
	if preflightErr == nil {
		t.Error("Preflight should fail when binary is missing")
	}
	if !strings.Contains(preflightErr.Error(), "codex") {
		t.Errorf("Preflight error should mention codex; got %q", preflightErr.Error())
	}

	res, err := d.Run(context.Background(), driver.Request{UserPrompt: "x"})
	if err == nil && res.Error == "" {
		t.Error("expected an error path when binary is missing, got success")
	}
}

// Copyright 2026 Zibby Lab. Apache-2.0.

// Package claudecli is the Anthropic Claude Code CLI subprocess driver.
//
// Why a second Claude driver: the sibling `claude` package calls the
// Anthropic Messages REST API directly using an `x-api-key`. That works
// only when the user has Anthropic API credit, billed separately from any
// Claude.ai / Claude Code subscription. This driver shells out to the
// `claude` Code CLI (`npm i -g @anthropic-ai/claude-code`), which reads
// CLAUDE_CODE_OAUTH_TOKEN and bills against the user's existing Claude
// subscription — same auth mode Zibby's workflow executor uses.
//
// Tool execution: agent-ops's internal tool registry is bypassed in this
// path. The CLI runs Claude's built-in Bash / Read / Write / Edit tools
// inside its own subprocess. Driver.Run just spawns + awaits + parses.
//
// Progress logging (0.3.3+): the driver invokes the CLI with
// --output-format=stream-json --verbose and parses the NDJSON event
// stream as it arrives. Every assistant message + tool_use + tool_result
// emits a "claudecli: turn" slog record so operators watching CloudWatch
// see PROGRESS during long-running installs (the n8n install on the
// goal-based runner can take 10+ minutes with no intermediate output
// otherwise). The final {"type":"result"} event still carries the same
// fields the old --output-format=json mode did (result, num_turns,
// total_cost_usd, is_error) and supplies the Driver.Result.
package claudecli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/ZibbyDev/agent-ops/internal/driver"
)

// Driver implements driver.Driver against the `claude` CLI binary.
type Driver struct {
	// Binary is the path/name of the CLI to invoke. Defaults to "claude".
	Binary string

	// Model passes through as --model. Empty = CLI default.
	Model string

	// AllowedTools is the comma-separated list passed via --allowedTools.
	// Defaults to "Bash,Read,Write,Edit" — the minimum a generic ops agent
	// needs to install + manage software.
	AllowedTools string

	// PermissionMode is passed via --permission-mode. "acceptEdits" works
	// with OAuth tokens; "bypassPermissions" is rejected at spawn by
	// Claude Code when running under an OAuth subscription (per
	// claude-strategy-permission-mode memory).
	PermissionMode string
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return "claude-cli" }

// defaultMaxTurns matches Claude Code's own internal default when the
// caller doesn't set MaxToolCalls. Surfaced in the per-turn progress log
// so operators see "turn N/25" instead of "turn N/0".
const defaultMaxTurns = 25

// Run shells out to the CLI with stream-json output and parses the
// NDJSON event stream. Per-turn progress is logged as events arrive;
// the final {"type":"result"} event supplies Driver.Result. Tool
// execution happens inside the CLI; req.Tools (agent-ops's own registry)
// is ignored beyond the AllowedTools allowlist mapping.
func (d *Driver) Run(ctx context.Context, req driver.Request) (driver.Result, error) {
	bin := d.Binary
	if bin == "" {
		bin = "claude"
	}
	allowedTools := d.AllowedTools
	if allowedTools == "" {
		allowedTools = "Bash,Read,Write,Edit"
	}
	permMode := d.PermissionMode
	if permMode == "" {
		permMode = "acceptEdits"
	}

	// Combine system + user prompt into one --print payload. The CLI doesn't
	// expose --system-prompt in older versions, so we prepend the system
	// prompt with a clear separator the model can latch onto.
	combined := req.UserPrompt
	if strings.TrimSpace(req.SystemPrompt) != "" {
		combined = req.SystemPrompt + "\n\n" + req.UserPrompt
	}

	// Per-request override beats driver default. Lets one driver instance
	// route Haiku for cron checks + Sonnet for installs without rebuilding.
	model := d.Model
	if req.Model != "" {
		model = req.Model
	}

	maxTurns := req.MaxToolCalls
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	args := []string{
		"--print", combined,
		"--output-format", "stream-json",
		"--verbose", // stream-json requires --verbose
		"--permission-mode", permMode,
		"--allowedTools", allowedTools,
		"--max-turns", fmt.Sprintf("%d", maxTurns),
	}
	if model != "" {
		args = append(args, "--model", model)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	stdoutPipe, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		return driver.Result{Error: fmt.Sprintf("claudecli: stdout pipe: %v", pipeErr)}, nil
	}
	var stderrBuf strings.Builder
	cmd.Stderr = newCapWriter(&stderrBuf, 8192)

	slog.Info("claudecli: spawning subprocess",
		"bin", bin, "model", model, "permission_mode", permMode,
		"allowed_tools", allowedTools, "max_turns", req.MaxToolCalls,
	)

	if err := cmd.Start(); err != nil {
		slog.Error("claudecli: subprocess start failed", "err", err.Error())
		return driver.Result{Error: fmt.Sprintf("claude CLI start failed: %v", err)}, nil
	}

	parsed, parseErr := parseStream(stdoutPipe, maxTurns, time.Now())

	// Always wait so we collect the exit code + reap the process even when
	// parsing aborts early (corrupt NDJSON, EOF before result, etc).
	waitErr := cmd.Wait()
	stderrStr := strings.TrimSpace(stderrBuf.String())

	if waitErr != nil {
		// Non-zero exit. Surface stderr (truncated) on the Result and the
		// daemon's structured log so the operator can see what went wrong
		// without exec'ing into the container.
		slog.Error("claudecli: subprocess failed",
			"err", waitErr.Error(),
			"stderr", truncate(stderrStr, 800),
			"events_seen", parsed.eventCount,
		)
		msg := fmt.Sprintf("claude CLI failed: %v: %s", waitErr, stderrStr)
		return driver.Result{Error: msg}, nil
	}

	if parseErr != nil && parsed.result == nil {
		// Stream ended without a result event AND parsing hit an error —
		// surface the raw buffered output so operators can see what the CLI
		// actually emitted (output-format drift is the usual culprit).
		slog.Warn("claudecli: stream parse failed",
			"err", parseErr.Error(),
			"raw", truncate(parsed.rawTail, 4000),
			"events_seen", parsed.eventCount,
		)
		return driver.Result{
			FinalMessage: strings.TrimSpace(parsed.rawTail),
			Error:        fmt.Sprintf("claude CLI: parse stream: %v", parseErr),
		}, nil
	}

	if parsed.result == nil {
		// Clean exit but no result event observed. Treat like a parse failure
		// — return whatever text we collected so the agent's work isn't lost.
		slog.Warn("claudecli: no result event in stream",
			"events_seen", parsed.eventCount,
			"raw", truncate(parsed.rawTail, 4000),
		)
		return driver.Result{
			FinalMessage: strings.TrimSpace(parsed.rawTail),
			Error:        "claude CLI: stream ended without result event",
		}, nil
	}

	resp := parsed.result

	// Emit the assistant's final message + cost stats + turn count to
	// the daemon's structured log so the dashboard / operator can SEE
	// what Claude actually said. Without this we only know that
	// claudecli was *spawned* — not what it produced. Truncated at 4k
	// to bound CloudWatch event size (events over 256KB get dropped).
	// Truncation limits: CloudWatch caps a single log event at 256KB and
	// drops anything larger. We were previously well under-budget (1.5K +
	// 2K + 4K + 0.8K + JSON overhead = ~10K) which left users with cut-off
	// MISSION/KNOWN FACTS blocks in the dashboard. Bumped to roomier
	// limits that still stay comfortably below the per-event cap:
	//   system_prompt → 8000  (covers ~40 facts + mission + base guidance)
	//   user_prompt   → 4000  (covers most schedule prompts in full)
	//   result        → 8000  (covers most Claude responses in full)
	// Total worst case ~21KB, well under the 256KB ceiling.
	slog.Info("claudecli: conversation complete",
		"system_prompt", truncate(req.SystemPrompt, 8000),
		"user_prompt", truncate(req.UserPrompt, 4000),
		"result", truncate(resp.Result, 8000),
		"num_turns", resp.NumTurns,
		"total_cost_usd", resp.TotalCostUSD,
		"is_error", resp.IsError,
		"stderr", truncate(stderrStr, 800),
	)

	if resp.IsError {
		return driver.Result{
			FinalMessage: resp.Result,
			ToolCalls:    resp.NumTurns,
			CostUSDMicro: int64(resp.TotalCostUSD * 1_000_000),
			Error:        resp.ErrorMessage,
		}, nil
	}

	return driver.Result{
		FinalMessage: resp.Result,
		ToolCalls:    resp.NumTurns,
		CostUSDMicro: int64(resp.TotalCostUSD * 1_000_000),
	}, nil
}

// streamResult mirrors the {"type":"result"} terminal NDJSON event that
// the CLI's --output-format=stream-json mode emits. Same shape as the
// pre-0.3.3 --output-format=json single-document output.
type streamResult struct {
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
	IsError      bool    `json:"is_error"`
	ErrorMessage string  `json:"error,omitempty"`
}

// parsed is what parseStream returns: the final result (nil if stream
// ended early) + counters useful for diagnostics in the error paths.
type parsedStream struct {
	result     *streamResult
	eventCount int
	// rawTail is the last few KB of raw stdout, used to populate diagnostic
	// logs when parsing fails or the stream ends without a result event.
	rawTail string
}

// parseStream reads NDJSON events from r and emits a "claudecli: turn"
// slog record at each meaningful boundary (assistant text, tool_use,
// tool_result). The terminal {"type":"result"} event is captured and
// returned. Other event types are tolerated and ignored.
//
// startTime seeds the elapsed_ms field on per-turn logs so operators can
// see install pacing.
func parseStream(r io.Reader, maxTurns int, startTime time.Time) (parsedStream, error) {
	out := parsedStream{}
	// Claude CLI lines can include large message payloads (system init
	// announces every available tool/skill/agent + memory_paths). 4 MiB
	// is generous headroom — bufio default is 64 KiB which truncates.
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	turn := 0
	var lastTool string

	// Keep a sliding tail of recent raw lines for error diagnostics.
	const tailBudget = 4096
	var tail strings.Builder

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		out.eventCount++

		// Maintain rawTail (bounded). Reset if we'd overflow the budget;
		// the goal is the last KB-or-so, not a perfect transcript.
		if tail.Len()+len(line)+1 > tailBudget {
			tail.Reset()
		}
		tail.Write(line)
		tail.WriteByte('\n')

		var env struct {
			Type    string          `json:"type"`
			Subtype string          `json:"subtype"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			// Non-JSON noise (rare — should not happen on stdout in stream-
			// json mode, but tolerate it rather than aborting the run).
			continue
		}

		switch env.Type {
		case "system":
			// init / config — useful for debugging but no progress to log.
			continue

		case "assistant":
			// Decode the message.content array to find tool_use or text
			// blocks. A single assistant message can contain BOTH text
			// (the thinking) AND one or more tool_use blocks.
			tool, text := extractAssistantSignal(env.Message)
			turn++
			fields := []any{
				"n", turn,
				"max", maxTurns,
				"elapsed_ms", time.Since(startTime).Milliseconds(),
			}
			if tool != "" {
				lastTool = tool
				fields = append(fields, "tool", tool)
			}
			if text != "" {
				fields = append(fields, "text", truncate(text, 200))
			}
			slog.Info("claudecli: turn", fields...)

		case "user":
			// tool_result event coming back from the CLI's tool executor.
			// Useful: tells the operator the tool actually finished (vs.
			// hung). Don't increment turn — that's the assistant's job —
			// but log a separate marker.
			toolID, isErr, snippet := extractToolResult(env.Message)
			fields := []any{
				"n", turn,
				"max", maxTurns,
				"elapsed_ms", time.Since(startTime).Milliseconds(),
				"tool", lastTool,
				"is_error", isErr,
			}
			if toolID != "" {
				fields = append(fields, "tool_use_id", toolID)
			}
			if snippet != "" {
				fields = append(fields, "output", truncate(snippet, 200))
			}
			slog.Info("claudecli: tool_result", fields...)

		case "result":
			var r streamResult
			if err := json.Unmarshal(line, &r); err != nil {
				return out, fmt.Errorf("parse result event: %w", err)
			}
			out.result = &r
			out.rawTail = tail.String()
			// Keep draining in case there are trailing events, but
			// nothing actionable follows the result event today.

		case "rate_limit_event":
			// Visible in the dashboard already via api logs; don't double-
			// log here.
			continue

		default:
			// Forward-compat: silently ignore unknown event types so a CLI
			// upgrade that adds new event shapes doesn't break the driver.
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		out.rawTail = tail.String()
		return out, fmt.Errorf("scan stream: %w", err)
	}

	if out.rawTail == "" {
		out.rawTail = tail.String()
	}
	return out, nil
}

// extractAssistantSignal pulls the most useful summary fields from an
// assistant message: the name of the first tool_use block (if any) and
// the first text block (if any). Empty strings if the field is absent.
func extractAssistantSignal(raw json.RawMessage) (toolName, text string) {
	if len(raw) == 0 {
		return "", ""
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return "", ""
	}
	for _, c := range msg.Content {
		switch c.Type {
		case "tool_use":
			if toolName == "" {
				toolName = c.Name
			}
		case "text":
			if text == "" {
				text = c.Text
			}
		}
	}
	return toolName, text
}

// extractToolResult pulls the tool_use_id, is_error flag, and a short
// snippet of the result content from a {"type":"user"} message that
// wraps a tool_result block.
func extractToolResult(raw json.RawMessage) (toolUseID string, isErr bool, snippet string) {
	if len(raw) == 0 {
		return "", false, ""
	}
	var msg struct {
		Content []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return "", false, ""
	}
	for _, c := range msg.Content {
		if c.Type != "tool_result" {
			continue
		}
		toolUseID = c.ToolUseID
		isErr = c.IsError
		// content can be a plain string OR an array of {type:"text",text:...}
		// — try string first, fall back to array.
		var asString string
		if err := json.Unmarshal(c.Content, &asString); err == nil {
			snippet = asString
		} else {
			var asArr []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(c.Content, &asArr); err == nil {
				for _, e := range asArr {
					if e.Type == "text" && e.Text != "" {
						snippet = e.Text
						break
					}
				}
			}
		}
		return
	}
	return "", false, ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// capWriter is an io.Writer that drops everything past `cap` bytes.
// Used for stderr so a pathological subprocess (e.g. tight error-log
// loop) can't OOM the daemon. cap=8192 mirrors the per-event budget
// the surrounding slog truncation already uses.
type capWriter struct {
	dst     *strings.Builder
	cap     int
	written int
}

func newCapWriter(dst *strings.Builder, cap int) *capWriter {
	return &capWriter{dst: dst, cap: cap}
}

func (w *capWriter) Write(p []byte) (int, error) {
	remaining := w.cap - w.written
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		w.dst.Write(p[:remaining])
		w.written += remaining
		return len(p), nil
	}
	w.dst.Write(p)
	w.written += len(p)
	return len(p), nil
}

// ensureBinaryAvailable is a friendly preflight: returns an error if the
// CLI binary isn't on PATH. Called from main.buildDriver so daemon
// startup fails with a clear message rather than at first task fire.
func (d *Driver) ensureBinaryAvailable() error {
	bin := d.Binary
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("claudecli: %q not on PATH (install with `npm i -g @anthropic-ai/claude-code`): %w", bin, err)
	}
	return nil
}

// Preflight is the public hook main.go calls before installing this driver.
func (d *Driver) Preflight() error {
	if d == nil {
		return errors.New("claudecli: nil driver")
	}
	return d.ensureBinaryAvailable()
}

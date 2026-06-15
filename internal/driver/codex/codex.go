// Copyright 2026 Zibby Lab. Apache-2.0.

// Package codex is the OpenAI Codex CLI subprocess driver.
//
// Why a third driver: agent-ops shipped with `claude` (Anthropic REST API,
// x-api-key billing) and `claude-cli` (the `claude` Code CLI binary,
// OAuth/subscription billing). Customers on the OpenAI side asked to use
// their existing ChatGPT / OpenAI account instead. This driver shells out
// to OpenAI's `codex` CLI (`npm i -g @openai/codex`) and parses its NDJSON
// event stream.
//
// Auth modes: v1 is API-key-only. Codex CLI natively reads OPENAI_API_KEY
// from the environment — we don't pass it on the command line. OAuth /
// `~/.codex/auth.json` distribution is deferred to a follow-up.
//
// Tool execution: identical model to claudecli — agent-ops's internal tool
// registry (req.Tools) is bypassed in this path. The CLI runs its own
// built-in shell + file-edit tools inside its subprocess. Driver.Run just
// spawns + awaits + parses the NDJSON event stream.
//
// Differences from claudecli to keep in mind:
//   - No `--system-prompt` flag → we prepend the system prompt to the user
//     prompt with a "\n\n" separator (same trick claudecli uses).
//   - No `--output-format json` → we use `--json` which emits NDJSON
//     events (one JSON object per line) on stdout. We parse the stream
//     incrementally and tolerate non-JSON lines.
//   - No `--max-turns` cap → enforced via TaskTimeout (wall-clock) only
//     for v1. Per-turn cap is a v2 follow-up. Document in main.go.
//   - No per-run cost reported by the CLI → CostUSDMicro is left at 0
//     for v1; computing from tokens × per-model price is v2.
//   - No `--allowedTools` allowlist → Codex CLI default-allows shell.
//     We pass `--sandbox workspace-write` (matches Claude's `acceptEdits`
//     semantics: agent can mutate workspace files).
//
// Argument order matters for Codex: global flags MUST come BEFORE the
// `exec` subcommand. See https://developers.openai.com/codex/cli/reference.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/ZibbyDev/agent-ops/internal/driver"
)

// Driver implements driver.Driver against the `codex` CLI binary.
type Driver struct {
	// Binary is the path/name of the CLI to invoke. Defaults to "codex".
	Binary string

	// Model passes through as --model. Empty = let Codex CLI pick its
	// default (typically the user's account's default reasoning model).
	Model string

	// Sandbox passes through as --sandbox. Defaults to "workspace-write" —
	// the equivalent of Claude Code's `acceptEdits` permission mode, which
	// lets the agent mutate workspace files without prompting. Codex also
	// supports "read-only" and "danger-full-access"; we expose this field
	// in case a deployment wants a tighter or looser policy.
	Sandbox string
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return "codex" }

// Run shells out to `codex --json --sandbox <s> [--model <m>] exec <prompt>`
// and parses the NDJSON event stream. Tool execution happens inside the
// CLI; req.Tools (agent-ops's own registry) is ignored.
//
// Note: req.MaxToolCalls is NOT honored in v1 — Codex CLI has no
// `--max-turns` flag. Enforce caps via context deadline (TaskTimeout) only.
func (d *Driver) Run(ctx context.Context, req driver.Request) (driver.Result, error) {
	bin := d.Binary
	if bin == "" {
		bin = "codex"
	}
	sandbox := d.Sandbox
	if sandbox == "" {
		sandbox = "workspace-write"
	}

	// Combine system + user prompt into one exec payload. Codex CLI doesn't
	// expose a --system-prompt flag — same situation as claudecli. The
	// model is good enough at picking up the "you are X" preamble that
	// the simple "\n\n" join is sufficient.
	combined := req.UserPrompt
	if strings.TrimSpace(req.SystemPrompt) != "" {
		combined = req.SystemPrompt + "\n\n" + req.UserPrompt
	}

	// Per-request override beats driver default. Same shape as claudecli.
	model := d.Model
	if req.Model != "" {
		model = req.Model
	}

	// Argument order matters: global flags (--json, --sandbox, --model)
	// MUST precede the `exec` subcommand or Codex rejects them.
	args := []string{"--json", "--sandbox", sandbox}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "exec", combined)

	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	slog.Info("codex: spawning subprocess",
		"bin", bin, "model", model, "sandbox", sandbox,
	)
	if err := cmd.Run(); err != nil {
		// Non-zero exit. Surface stderr (truncated) on the Result and the
		// daemon's structured log so the operator can see what went wrong
		// without exec'ing into the container.
		stderrStr := strings.TrimSpace(stderr.String())
		slog.Error("codex: subprocess failed",
			"err", err.Error(),
			"stderr", truncate(stderrStr, 800),
			"stdout_size", stdout.Len(),
		)
		msg := fmt.Sprintf("codex CLI failed: %v: %s", err, stderrStr)
		return driver.Result{Error: msg}, nil
	}

	// Parse the NDJSON event stream. Codex emits one JSON object per line
	// on stdout in --json mode. Events we care about:
	//   - {"type":"item.completed","item":{"item_type":"agent_message","text":"..."}}
	//       → concatenate text into FinalMessage
	//   - {"type":"item.completed","item":{"item_type":"command_execution",...}}
	//     {"type":"item.completed","item":{"item_type":"mcp_tool_call",...}}
	//       → count toward ToolCalls (rough proxy for "turns")
	//   - {"type":"turn.completed","usage":{"input_tokens":N,"cached_input_tokens":N,"output_tokens":N}}
	//       → last one wins for total tokens (cumulative across the run)
	//   - {"type":"error","message":"..."} → treat as failure
	//
	// The parser tolerates non-JSON lines (skipped silently) and a final
	// blank line — Codex's stable output format may interleave human-
	// readable hints if a future version regresses, and we don't want to
	// hard-fail on that.
	parsed := parseNDJSON(stdout.Bytes())

	stderrStr := strings.TrimSpace(stderr.String())

	// Truncation caps match claudecli.go (post-0.1.19 bump): 8K system,
	// 4K user, 8K result, 0.8K stderr. CloudWatch event ceiling is 256KB
	// and we want to stay comfortably below it. The field names below are
	// the canonical CloudWatch search keys — do NOT rename without
	// updating the dashboard queries.
	slog.Info("codex: conversation complete",
		"system_prompt", truncate(req.SystemPrompt, 8000),
		"user_prompt", truncate(req.UserPrompt, 4000),
		"result", truncate(parsed.finalMessage, 8000),
		"tool_calls", parsed.toolCalls,
		"tokens_input", parsed.inputTokens,
		"tokens_output", parsed.outputTokens,
		"is_error", parsed.isError,
		"stderr", truncate(stderrStr, 800),
	)

	if parsed.isError {
		return driver.Result{
			FinalMessage: parsed.finalMessage,
			ToolCalls:    parsed.toolCalls,
			// CostUSDMicro: 0 — Codex CLI does not report a cost number.
			// Computing from tokens × per-model price is a v2 follow-up.
			Error: parsed.errorMessage,
		}, nil
	}

	return driver.Result{
		FinalMessage: parsed.finalMessage,
		ToolCalls:    parsed.toolCalls,
		// CostUSDMicro: 0 — see note above. Tokens are logged for the
		// dashboard but not propagated into the Result struct in v1.
	}, nil
}

// parsedRun is the distilled result of walking the NDJSON event stream.
type parsedRun struct {
	finalMessage string
	toolCalls    int
	inputTokens  int
	outputTokens int
	isError      bool
	errorMessage string
}

// parseNDJSON walks the stdout bytes line-by-line, decoding each as JSON.
// Non-JSON lines are skipped (Codex may emit a stray banner in some
// configurations). The function returns a distilled view; callers don't
// need to know the wire format.
//
// Event shapes consumed (see codex CLI source — these are stable as of
// @openai/codex@0.135.0):
//   - item.completed with item.item_type = "agent_message" → text
//   - item.completed with item.item_type = "command_execution" → count
//   - item.completed with item.item_type = "mcp_tool_call" → count
//   - turn.completed with usage{input_tokens, cached_input_tokens, output_tokens}
//   - error with message
//
// We use the *last* turn.completed.usage because Codex emits cumulative
// totals (one per completed turn) and the final one is the true total.
func parseNDJSON(raw []byte) parsedRun {
	var out parsedRun
	var finalParts []string

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	// Codex events can be large (a long agent_message text). Bump the
	// buffer from the 64K default to 1MB so we don't truncate.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			// Skip blank lines and non-JSON lines (e.g. a banner or
			// human-readable hint Codex might print at startup).
			continue
		}

		// Decode each event into a permissive struct. Unknown fields are
		// ignored, which keeps the parser forward-compatible with new
		// event types.
		var ev struct {
			Type    string `json:"type"`
			Message string `json:"message,omitempty"`
			Item    struct {
				ItemType string `json:"item_type"`
				Text     string `json:"text"`
			} `json:"item,omitempty"`
			Usage struct {
				InputTokens       int `json:"input_tokens"`
				CachedInputTokens int `json:"cached_input_tokens"`
				OutputTokens      int `json:"output_tokens"`
			} `json:"usage,omitempty"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			// Tolerate malformed JSON lines. A single bad line shouldn't
			// poison the run — the agent's final message may still be
			// reachable later in the stream.
			continue
		}

		switch ev.Type {
		case "item.completed":
			switch ev.Item.ItemType {
			case "agent_message":
				if ev.Item.Text != "" {
					finalParts = append(finalParts, ev.Item.Text)
				}
			case "command_execution", "mcp_tool_call":
				out.toolCalls++
			}
		case "turn.completed":
			// Cumulative totals — last one wins. Codex emits these
			// once per completed reasoning turn.
			out.inputTokens = ev.Usage.InputTokens
			out.outputTokens = ev.Usage.OutputTokens
		case "error":
			out.isError = true
			if ev.Message != "" {
				out.errorMessage = ev.Message
			}
		}
	}

	out.finalMessage = strings.Join(finalParts, "")
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ensureBinaryAvailable is a friendly preflight: returns an error if the
// CLI binary isn't on PATH. Called from main.buildDriver so daemon
// startup fails with a clear message rather than at first task fire.
func (d *Driver) ensureBinaryAvailable() error {
	bin := d.Binary
	if bin == "" {
		bin = "codex"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("codex: %q not on PATH (install with `npm i -g @openai/codex`): %w", bin, err)
	}
	return nil
}

// Preflight is the public hook main.go calls before installing this driver.
// Mirrors claudecli.Driver.Preflight so the call site in buildDriver looks
// the same for both subprocess drivers.
func (d *Driver) Preflight() error {
	if d == nil {
		return errors.New("codex: nil driver")
	}
	return d.ensureBinaryAvailable()
}

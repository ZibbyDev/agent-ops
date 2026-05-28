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
package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/ZibbyHQ/agent-ops/internal/driver"
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

// Run shells out to `claude --print <prompt> --output-format json` and
// parses the structured result. Tool execution happens inside the CLI;
// req.Tools (agent-ops's own registry) is ignored beyond the AllowedTools
// allowlist mapping.
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

	args := []string{
		"--print", combined,
		"--output-format", "json",
		"--permission-mode", permMode,
		"--allowedTools", allowedTools,
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.MaxToolCalls > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", req.MaxToolCalls))
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	slog.Info("claudecli: spawning subprocess",
		"bin", bin, "model", model, "permission_mode", permMode,
		"allowed_tools", allowedTools, "max_turns", req.MaxToolCalls,
	)
	if err := cmd.Run(); err != nil {
		// Non-zero exit. Surface stderr (truncated) on the Result and the
		// daemon's structured log so the operator can see what went wrong
		// without exec'ing into the container.
		stderrStr := strings.TrimSpace(stderr.String())
		slog.Error("claudecli: subprocess failed",
			"err", err.Error(),
			"stderr", truncate(stderrStr, 800),
			"stdout_size", stdout.Len(),
		)
		msg := fmt.Sprintf("claude CLI failed: %v: %s", err, stderrStr)
		return driver.Result{Error: msg}, nil
	}

	// `--output-format json` returns:
	//   {"result": "...", "total_cost_usd": 0.0042, "num_turns": 3, ...}
	var resp struct {
		Result       string  `json:"result"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		NumTurns     int     `json:"num_turns"`
		IsError      bool    `json:"is_error"`
		ErrorMessage string  `json:"error,omitempty"`
	}
	raw := stdout.Bytes()
	if err := json.Unmarshal(raw, &resp); err != nil {
		// Fall back to raw output. Better than dropping the agent's work.
		// Surface the unparseable stdout to the daemon log too — operator
		// needs the actual bytes to debug a CLI output format change.
		slog.Warn("claudecli: stdout parse failed",
			"err", err.Error(),
			"raw", truncate(string(raw), 4000),
		)
		return driver.Result{
			FinalMessage: strings.TrimSpace(string(raw)),
			Error:        fmt.Sprintf("claude CLI: parse JSON output: %v", err),
		}, nil
	}

	// Emit the assistant's final message + cost stats + turn count to
	// the daemon's structured log so the dashboard / operator can SEE
	// what Claude actually said. Without this we only know that
	// claudecli was *spawned* — not what it produced. Truncated at 4k
	// to bound CloudWatch event size (events over 256KB get dropped).
	stderrStr := strings.TrimSpace(stderr.String())
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

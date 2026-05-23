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

	args := []string{
		"--print", combined,
		"--output-format", "json",
		"--permission-mode", permMode,
		"--allowedTools", allowedTools,
	}
	if d.Model != "" {
		args = append(args, "--model", d.Model)
	}
	if req.MaxToolCalls > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", req.MaxToolCalls))
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Non-zero exit. Surface stderr in the Result so the user sees the
		// reason without having to dig through container logs.
		msg := fmt.Sprintf("claude CLI failed: %v: %s", err, strings.TrimSpace(stderr.String()))
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
		return driver.Result{
			FinalMessage: strings.TrimSpace(string(raw)),
			Error:        fmt.Sprintf("claude CLI: parse JSON output: %v", err),
		}, nil
	}
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

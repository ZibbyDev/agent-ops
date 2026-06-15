// Copyright 2026 Zibby Lab. Apache-2.0.

// Package driver is the LLM backend interface.
//
// v0.1 ships only claude.Driver. codex/gemini/ollama implementations live in
// sibling packages and register through the same Factory.
package driver

import (
	"context"

	"github.com/ZibbyDev/agent-ops/internal/tool"
)

// Driver runs one agent loop: given a system prompt + user task + tool
// registry, drive the LLM through tool-call iterations until it produces a
// final summary or hits MaxToolCalls.
type Driver interface {
	// Name is the provider identifier ("claude", "codex", …).
	Name() string

	// Run executes one task. The returned Result records what happened —
	// successful runs include the final assistant message; failed runs include
	// the error in Error. ToolCalls counts iterations for cost accounting.
	Run(ctx context.Context, req Request) (Result, error)
}

// Request is one task invocation.
type Request struct {
	// SystemPrompt sets the agent's identity + invariants. Stable across runs
	// of the same Task.
	SystemPrompt string

	// UserPrompt is the per-invocation instruction. For scheduled tasks this
	// is just the schedule's prompt; for on-demand runs it's whatever the
	// caller (MCP client) supplied.
	UserPrompt string

	// Tools is the allowlist for this task. The driver should not invoke any
	// tool not present in this registry.
	Tools *tool.Registry

	// MaxToolCalls bounds the iteration loop.
	MaxToolCalls int

	// Model overrides the driver's default model for this single request.
	// Empty → driver's configured default applies. Used to route routine
	// cron tasks (Haiku) and install/upgrade tasks (Sonnet) through one
	// driver instance without rebuilding it per call.
	Model string

	// LogSink receives narration of the run (one line per tool call etc.)
	// for persistence into task_run_logs. Nil disables.
	LogSink LogSink
}

// LogSink is the daemon's persistent run-log writer.
type LogSink interface {
	Log(ctx context.Context, level, message string) error
}

// Result is what Run returns.
type Result struct {
	// FinalMessage is the assistant's last message after all tool calls.
	FinalMessage string

	// ToolCalls is the number of tool-use iterations consumed.
	ToolCalls int

	// CostUSDMicro is the run cost in micro-dollars (1e-6 USD), best-effort.
	CostUSDMicro int64

	// Error is set when the run failed mid-loop. The caller should treat
	// this as the task's failure mode.
	Error string
}

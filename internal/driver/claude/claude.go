// Copyright 2026 Zibby Lab. Apache-2.0.

// Package claude is the Anthropic Messages API driver.
//
// We call the REST endpoint directly rather than pulling in the official
// anthropic-sdk-go because it keeps the dependency surface tiny (one HTTP
// client, no transitive deps) and the Messages API is stable enough that
// hand-rolling is low risk. If we later need streaming or extended thinking
// we can swap in the SDK.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ZibbyDev/agent-ops/internal/driver"
	"github.com/ZibbyDev/agent-ops/internal/tool"
)

// Driver implements driver.Driver against Anthropic's Messages API.
type Driver struct {
	// APIKey is the bearer key (Anthropic uses x-api-key, not Authorization).
	APIKey string

	// Model is the model id, e.g. "claude-sonnet-4-6" or "claude-opus-4-7".
	Model string

	// BaseURL defaults to https://api.anthropic.com when empty. Overridable for
	// tests + future Bedrock-compat shims.
	BaseURL string

	// HTTPClient is overridable; defaults to a sensible client with timeouts.
	HTTPClient *http.Client

	// MaxOutputTokens caps Anthropic's max_tokens parameter. 0 → 4096.
	MaxOutputTokens int
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return "claude" }

const defaultBaseURL = "https://api.anthropic.com"
const apiVersion = "2023-06-01"

// Run executes the agent loop: keep calling the Messages API, run any
// requested tool, feed the result back, until either the assistant emits
// stop_reason="end_turn" or we hit MaxToolCalls.
func (d *Driver) Run(ctx context.Context, req driver.Request) (driver.Result, error) {
	if d.APIKey == "" {
		return driver.Result{}, errors.New("claude: APIKey is empty")
	}
	// Per-request override beats driver default. Lets one driver instance
	// route mixed-model traffic (Haiku for cron checks, Sonnet for installs)
	// without rebuilding.
	model := d.Model
	if req.Model != "" {
		model = req.Model
	}
	if model == "" {
		return driver.Result{}, errors.New("claude: Model is empty (set agent.model or per-task model)")
	}
	base := d.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	maxOut := d.MaxOutputTokens
	if maxOut == 0 {
		maxOut = 4096
	}
	httpc := d.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 120 * time.Second}
	}

	tools := claudeToolSpecs(req.Tools)

	// Conversation state — start with one user turn.
	messages := []claudeMessage{
		{Role: "user", Content: []claudeContent{{Type: "text", Text: req.UserPrompt}}},
	}

	maxIter := req.MaxToolCalls
	if maxIter <= 0 {
		maxIter = 25
	}

	out := driver.Result{}

	for iter := 0; iter < maxIter; iter++ {
		body := claudeRequest{
			Model:     model,
			MaxTokens: maxOut,
			System:    req.SystemPrompt,
			Messages:  messages,
			Tools:     tools,
		}
		buf, err := json.Marshal(body)
		if err != nil {
			return out, fmt.Errorf("claude: marshal request: %w", err)
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(buf))
		if err != nil {
			return out, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("anthropic-version", apiVersion)
		httpReq.Header.Set("x-api-key", d.APIKey)

		resp, err := httpc.Do(httpReq)
		if err != nil {
			return out, fmt.Errorf("claude: http: %w", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode/100 != 2 {
			return out, fmt.Errorf("claude: %d %s: %s", resp.StatusCode, resp.Status, truncate(string(respBody), 400))
		}

		var msg claudeResponse
		if err := json.Unmarshal(respBody, &msg); err != nil {
			return out, fmt.Errorf("claude: parse response: %w", err)
		}

		// Accumulate cost. Anthropic returns usage in tokens; we don't know the
		// per-model rate at compile time, so leave CostUSDMicro at 0 unless
		// the caller wires up a pricing table. The fields are still recorded
		// in the run logs below.
		_ = msg.Usage

		// Capture any text content from the assistant's reply so we can
		// surface it as the run's FinalMessage even when we stop on
		// max_tool_calls or stop_reason=end_turn.
		var assistantText string
		var toolUses []claudeToolUse
		for _, c := range msg.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					assistantText += c.Text
				}
			case "tool_use":
				toolUses = append(toolUses, claudeToolUse{
					ID:    c.ID,
					Name:  c.Name,
					Input: c.Input,
				})
			}
		}

		// Append the assistant turn to history.
		messages = append(messages, claudeMessage{
			Role:    "assistant",
			Content: msg.Content,
		})

		if len(toolUses) == 0 {
			// Conversation done — no more tool calls requested.
			out.FinalMessage = assistantText
			out.ToolCalls = iter
			return out, nil
		}

		if req.LogSink != nil {
			_ = req.LogSink.Log(ctx, "assistant", truncate(assistantText, 1000))
		}

		// Run each requested tool and feed results back in one user turn.
		toolResults := make([]claudeContent, 0, len(toolUses))
		for _, tu := range toolUses {
			out.ToolCalls++
			t, ok := req.Tools.Get(tu.Name)
			if !ok {
				toolResults = append(toolResults, claudeContent{
					Type:      "tool_result",
					ToolUseID: tu.ID,
					Content:   fmt.Sprintf("error: tool %q is not registered or not allowed for this task", tu.Name),
					IsError:   true,
				})
				if req.LogSink != nil {
					_ = req.LogSink.Log(ctx, "error", fmt.Sprintf("tool %q not allowed", tu.Name))
				}
				continue
			}
			if req.LogSink != nil {
				_ = req.LogSink.Log(ctx, "tool",
					fmt.Sprintf("invoke %s args=%s", tu.Name, truncate(string(tu.Input), 400)))
			}
			res, err := t.Invoke(ctx, tu.Input)
			if err != nil {
				toolResults = append(toolResults, claudeContent{
					Type:      "tool_result",
					ToolUseID: tu.ID,
					Content:   "error: " + err.Error(),
					IsError:   true,
				})
				if req.LogSink != nil {
					_ = req.LogSink.Log(ctx, "error", fmt.Sprintf("tool %s failed: %v", tu.Name, err))
				}
				continue
			}
			toolResults = append(toolResults, claudeContent{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Content:   res.Output,
			})
			if req.LogSink != nil {
				out := res.Output
				if res.Sensitive {
					out = "[sensitive output redacted]"
				}
				_ = req.LogSink.Log(ctx, "tool-result", truncate(out, 1000))
			}
		}

		messages = append(messages, claudeMessage{Role: "user", Content: toolResults})

		// If the assistant indicated end_turn already (rare with tool calls
		// pending, but defensible), bail early so we don't loop on nothing.
		if msg.StopReason == "end_turn" {
			out.FinalMessage = assistantText
			return out, nil
		}
	}

	out.Error = fmt.Sprintf("max_tool_calls (%d) reached before assistant emitted a final answer", maxIter)
	return out, nil
}

// ─── Wire types ─────────────────────────────────────────────────────────────

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []claudeMessage `json:"messages"`
	Tools     []claudeTool    `json:"tools,omitempty"`
}

type claudeMessage struct {
	Role    string          `json:"role"` // "user" | "assistant"
	Content []claudeContent `json:"content"`
}

// claudeContent is a discriminated union: type tells you which fields matter.
// We keep all fields on one struct because the Anthropic API does too.
type claudeContent struct {
	Type string `json:"type"` // "text" | "tool_use" | "tool_result"

	// type=text
	Text string `json:"text,omitempty"`

	// type=tool_use (assistant → us)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type=tool_result (us → assistant)
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type claudeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type claudeResponse struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Content    []claudeContent `json:"content"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type claudeToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// claudeToolSpecs renders the daemon's Tool registry into Anthropic's
// tool-spec list. nil registry → empty (no tool use).
func claudeToolSpecs(r *tool.Registry) []claudeTool {
	if r == nil {
		return nil
	}
	tools := r.List()
	out := make([]claudeTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, claudeTool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.Schema(),
		})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Copyright 2026 Zibby Lab. Apache-2.0.

// Package mcpclient is the MCP CLIENT side of agent-ops.
//
// agent-ops's own MCP SERVER (internal/api/mcp) lets remote agents drive the
// daemon. This package is the inverse: it lets the daemon dial OTHER MCP
// servers (HTTP streamable or stdio subprocess), enumerate their tools, and
// expose those tools to the local LLM driver via tool.RemoteToolAdapter so a
// scheduled prompt can call e.g. `zibby_trigger_workflow` the same way it
// calls the built-in `shell`.
//
// Why hand-rolled instead of a Go SDK:
//   - There is no first-party Anthropic Go SDK as of 2026-05; the closest
//     community packages all hard-code transport assumptions (websocket-only,
//     or stdio-only) that don't fit our dual HTTP+stdio need.
//   - The MCP wire format is tiny (JSON-RPC 2.0 + a couple of method names)
//     and the SERVER in this same repo is also hand-rolled — depending on a
//     half-baked third-party port for the client half would split our wire
//     handling across two divergent dialects.
//
// Lifecycle (Manager owns it):
//
//	cfg → NewClient → Start → Initialize → ListTools → repeated CallTool…
//	                                                  ↓
//	                                              (on Close)
//	                                              cleanup
//
// stdio subprocesses are supervised with exponential-backoff restart; HTTP
// clients are stateless per-call.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Transport names the on-the-wire path used to talk to the remote server.
type Transport string

const (
	TransportHTTP  Transport = "http"  // streamable HTTP (POST /mcp + bearer auth)
	TransportStdio Transport = "stdio" // local subprocess JSON-RPC over stdin/stdout
)

// ToolDef is one tool the remote server advertised in tools/list. The
// adapter in internal/tool wraps a ToolDef into a tool.Tool by attaching a
// pointer back to the Client that owns it.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// CallResult is the standardized return of CallTool — extracted from the MCP
// tools/call response's `content` blocks. We flatten `content[*].text` into
// one string because the local LLM driver expects a single tool-result
// payload, same as the built-in shell tool.
type CallResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError"`
}

// Client is one connection to one remote MCP server. Implementations are
// transport-specific; the surface is identical so the Manager / adapter
// don't care which.
type Client interface {
	// Name is the client's unique identifier (matches Config.Name). Used
	// when prefixing remote tool names so `zibby` + `trigger_workflow`
	// becomes `zibby_trigger_workflow` in the local registry.
	Name() string

	// Start does the transport-specific connect:
	//   - HTTP: nothing — POSTs are stateless.
	//   - stdio: fork+exec the subprocess + spin a reader goroutine.
	// Safe to call multiple times; subsequent calls are no-ops.
	Start(ctx context.Context) error

	// Initialize sends the MCP `initialize` handshake. MUST be called before
	// ListTools / CallTool. Some servers (e.g. ours) accept calls without it
	// but the spec requires it, and well-behaved servers will reject.
	Initialize(ctx context.Context) error

	// ListTools fetches the current tool set. Called once at boot;
	// `listChanged` notifications could refresh it but v0.3 doesn't yet.
	ListTools(ctx context.Context) ([]ToolDef, error)

	// CallTool invokes one remote tool. `args` MUST already be JSON-encoded
	// per the tool's inputSchema — the adapter passes through what the LLM
	// produced.
	CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error)

	// Close releases any resources (subprocess, http transports). Safe to
	// call multiple times.
	Close() error
}

// Config is one entry in the config.yaml `mcp_clients:` list. Mirrors
// config.MCPClientConfig but lives in this package so the client code has
// no upward dep on config (which depends on cron + yaml).
type Config struct {
	Name      string            // unique; used as tool-name prefix
	Transport Transport         // "http" | "stdio"
	URL       string            // HTTP only — full endpoint, e.g. https://api/.../mcp
	Command   string            // stdio only — executable
	Args      []string          // stdio only — argv after Command
	AuthToken string            // HTTP only — resolved from AuthEnv at boot
	Env       map[string]string // stdio only — extra env appended to subprocess env

	// MaxBackoff caps the restart delay for crashed stdio subprocesses.
	// Zero → package default (30s).
	MaxBackoff int
}

// New builds a Client for the given Config. Returns an error for unknown
// transports or missing required fields.
func New(cfg Config) (Client, error) {
	switch cfg.Transport {
	case TransportHTTP:
		if cfg.URL == "" {
			return nil, fmt.Errorf("mcpclient: %s: http transport requires URL", cfg.Name)
		}
		return newHTTPClient(cfg), nil
	case TransportStdio:
		if cfg.Command == "" {
			return nil, fmt.Errorf("mcpclient: %s: stdio transport requires Command", cfg.Name)
		}
		return newStdioClient(cfg), nil
	default:
		return nil, fmt.Errorf("mcpclient: %s: unknown transport %q (want http|stdio)", cfg.Name, cfg.Transport)
	}
}

// ─── shared JSON-RPC primitives ────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// idGen is a process-wide monotonic id source for JSON-RPC request ids.
// Servers don't care about reuse across clients, but a single monotonic
// counter keeps debug logs ordered.
type idGen struct {
	mu sync.Mutex
	n  int
}

func (g *idGen) next() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return g.n
}

// flattenContent extracts the `content[*].text` blocks from an MCP
// tools/call result. The wire shape is:
//
//	{ "content": [{"type":"text","text":"..."}], "isError": false }
//
// We concatenate text blocks (separated by newlines) into one string so the
// local driver sees a single tool-result, matching how the built-in shell
// tool's Result.Output behaves.
func flattenContent(raw json.RawMessage) (CallResult, error) {
	var wire struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return CallResult{}, fmt.Errorf("mcp: decode tools/call result: %w", err)
	}
	out := CallResult{IsError: wire.IsError}
	for i, c := range wire.Content {
		if i > 0 {
			out.Text += "\n"
		}
		out.Text += c.Text
	}
	return out, nil
}

// initializeParams is the body of the MCP `initialize` request.
func initializeParams(name string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "agent-ops-mcpclient",
			"version": "0.3.0",
		},
	})
	_ = name // reserved for future server-side identification
	return b
}

// callToolParams builds the tools/call params blob.
func callToolParams(name string, args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	b, _ := json.Marshal(map[string]any{
		"name":      name,
		"arguments": json.RawMessage(args),
	})
	return b
}


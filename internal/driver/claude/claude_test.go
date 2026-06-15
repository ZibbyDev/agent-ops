package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZibbyDev/agent-ops/internal/driver"
	"github.com/ZibbyDev/agent-ops/internal/tool"
)

// scriptedHandler returns scripted responses in order. Each call consumes one.
type scriptedHandler struct {
	t       *testing.T
	replies []claudeResponse
	calls   int
}

func (s *scriptedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" {
		s.t.Fatalf("unexpected path %q", r.URL.Path)
	}
	if r.Header.Get("anthropic-version") == "" {
		s.t.Fatal("missing anthropic-version")
	}
	if r.Header.Get("x-api-key") != "test-key" {
		s.t.Fatalf("missing/wrong x-api-key: %q", r.Header.Get("x-api-key"))
	}
	// Sanity-check the request payload is parseable.
	body, _ := io.ReadAll(r.Body)
	var in claudeRequest
	if err := json.Unmarshal(body, &in); err != nil {
		s.t.Fatalf("daemon sent bad json: %v", err)
	}
	if in.Model == "" {
		s.t.Fatal("daemon sent no model")
	}
	if s.calls >= len(s.replies) {
		s.t.Fatalf("driver made more calls than scripted: %d", s.calls+1)
	}
	out := s.replies[s.calls]
	s.calls++
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// fakeTool records invocations + returns a fixed string.
type fakeTool struct {
	name    string
	desc    string
	schema  json.RawMessage
	invoke  func(json.RawMessage) (tool.Result, error)
	called  int
	lastArg json.RawMessage
}

func (f *fakeTool) Name() string             { return f.name }
func (f *fakeTool) Description() string      { return f.desc }
func (f *fakeTool) Schema() json.RawMessage  { return f.schema }
func (f *fakeTool) Invoke(_ context.Context, a json.RawMessage) (tool.Result, error) {
	f.called++
	f.lastArg = a
	if f.invoke != nil {
		return f.invoke(a)
	}
	return tool.Result{Output: "tool-default-output"}, nil
}

func TestRun_NoToolCall_ReturnsFinalText(t *testing.T) {
	srv := httptest.NewServer(&scriptedHandler{
		t: t,
		replies: []claudeResponse{{
			ID:         "msg_1",
			Type:       "message",
			Role:       "assistant",
			StopReason: "end_turn",
			Content:    []claudeContent{{Type: "text", Text: "all good"}},
		}},
	})
	defer srv.Close()

	d := &Driver{APIKey: "test-key", Model: "claude-test", BaseURL: srv.URL}
	reg := tool.NewRegistry()
	res, err := d.Run(context.Background(), driver.Request{
		SystemPrompt: "you are an ops agent",
		UserPrompt:   "summarize",
		Tools:        reg,
		MaxToolCalls: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "all good" {
		t.Fatalf("FinalMessage = %q", res.FinalMessage)
	}
	if res.ToolCalls != 0 {
		t.Fatalf("expected 0 tool calls, got %d", res.ToolCalls)
	}
}

func TestRun_ToolCallLoop_FeedsResultBack(t *testing.T) {
	tu := json.RawMessage(`{"command":"echo hi"}`)
	srv := httptest.NewServer(&scriptedHandler{
		t: t,
		replies: []claudeResponse{
			{
				ID:         "msg_1",
				StopReason: "tool_use",
				Content: []claudeContent{
					{Type: "text", Text: "I'll run a command"},
					{Type: "tool_use", ID: "toolu_1", Name: "shell", Input: tu},
				},
			},
			{
				ID:         "msg_2",
				StopReason: "end_turn",
				Content:    []claudeContent{{Type: "text", Text: "done"}},
			},
		},
	})
	defer srv.Close()
	d := &Driver{APIKey: "test-key", Model: "claude-test", BaseURL: srv.URL}
	reg := tool.NewRegistry()
	ft := &fakeTool{name: "shell", desc: "shell tool", schema: json.RawMessage(`{"type":"object"}`)}
	if err := reg.Register(ft); err != nil {
		t.Fatal(err)
	}
	res, err := d.Run(context.Background(), driver.Request{
		UserPrompt:   "do thing",
		Tools:        reg,
		MaxToolCalls: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ft.called != 1 {
		t.Fatalf("tool not invoked, called=%d", ft.called)
	}
	if string(ft.lastArg) != string(tu) {
		t.Fatalf("tool got wrong args: %s vs %s", ft.lastArg, tu)
	}
	if res.FinalMessage != "done" {
		t.Fatalf("FinalMessage = %q", res.FinalMessage)
	}
	if res.ToolCalls != 1 {
		t.Fatalf("ToolCalls = %d, want 1", res.ToolCalls)
	}
}

func TestRun_ToolNotInRegistry_RecordsError(t *testing.T) {
	srv := httptest.NewServer(&scriptedHandler{
		t: t,
		replies: []claudeResponse{
			{
				StopReason: "tool_use",
				Content: []claudeContent{
					{Type: "tool_use", ID: "tu_1", Name: "rm-rf", Input: json.RawMessage(`{}`)},
				},
			},
			{
				StopReason: "end_turn",
				Content:    []claudeContent{{Type: "text", Text: "abandoned"}},
			},
		},
	})
	defer srv.Close()
	d := &Driver{APIKey: "test-key", Model: "claude-test", BaseURL: srv.URL}
	reg := tool.NewRegistry() // empty — no rm-rf registered
	res, err := d.Run(context.Background(), driver.Request{
		UserPrompt: "do bad",
		Tools:      reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalMessage != "abandoned" {
		t.Fatalf("FinalMessage = %q", res.FinalMessage)
	}
	if res.ToolCalls != 1 {
		t.Fatalf("ToolCalls should count the attempted call, got %d", res.ToolCalls)
	}
}

func TestRun_MaxToolCalls_BoundsLoop(t *testing.T) {
	// Always reply with another tool_use → driver should give up after maxIter.
	reply := claudeResponse{
		StopReason: "tool_use",
		Content: []claudeContent{
			{Type: "tool_use", ID: "tu", Name: "shell", Input: json.RawMessage(`{"command":"true"}`)},
		},
	}
	srv := httptest.NewServer(&scriptedHandler{
		t:       t,
		replies: []claudeResponse{reply, reply, reply, reply, reply}, // plenty
	})
	defer srv.Close()
	d := &Driver{APIKey: "test-key", Model: "claude-test", BaseURL: srv.URL}
	reg := tool.NewRegistry()
	_ = reg.Register(&fakeTool{name: "shell", schema: json.RawMessage(`{"type":"object"}`)})
	res, err := d.Run(context.Background(), driver.Request{
		UserPrompt:   "loop forever",
		Tools:        reg,
		MaxToolCalls: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == "" {
		t.Fatal("expected Error to be set on max_tool_calls")
	}
	if !strings.Contains(res.Error, "max_tool_calls") {
		t.Fatalf("Error: %q", res.Error)
	}
}

func TestRun_RejectsMissingCreds(t *testing.T) {
	d := &Driver{Model: "m"}
	if _, err := d.Run(context.Background(), driver.Request{}); err == nil {
		t.Fatal("expected error for empty APIKey")
	}
	d = &Driver{APIKey: "k"}
	if _, err := d.Run(context.Background(), driver.Request{}); err == nil {
		t.Fatal("expected error for empty Model")
	}
}

func TestRun_Returns_5xxAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"oops"}`))
	}))
	defer srv.Close()
	d := &Driver{APIKey: "k", Model: "m", BaseURL: srv.URL}
	if _, err := d.Run(context.Background(), driver.Request{Tools: tool.NewRegistry()}); err == nil {
		t.Fatal("expected error on 500")
	}
}

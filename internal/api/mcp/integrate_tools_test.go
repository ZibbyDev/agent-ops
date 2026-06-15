// Copyright 2026 Zibby Lab. Apache-2.0.

package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZibbyDev/agent-ops/internal/scheduler"
	"github.com/ZibbyDev/agent-ops/internal/state"
	"github.com/ZibbyDev/agent-ops/internal/task"
	"github.com/ZibbyDev/agent-ops/internal/tool"
)

// integrationFixtureYAML is the minimum config the integrate tools need
// in order to round-trip — the daemon-side MCP server reads its own
// configPath, parses it, mutates, writes back.
const integrationFixtureYAML = `state_dir: /tmp/ao
agent:
  provider: claude
  model: claude-sonnet-4-6
  api_key_env: K
schedules:
  - name: x
    cron: "@hourly"
    prompt: y
mcp:
  listen_addr: ":7842"
  token_env: AGENT_OPS_TOKEN
`

func setupServerWithConfig(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(integrationFixtureYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	tools := tool.NewRegistry()
	_ = tools.Register(tool.NewShellTool())
	runner := task.NewRunner(fakeDriver{}, tools, st)
	sched := scheduler.New(runner, st, slog.Default())
	sched.Start()
	t.Cleanup(func() { _ = sched.Stop(context.Background()) })

	srv, err := New(Config{
		Scheduler: sched, Store: st, Tools: tools,
		Token: "test-token", ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, cfgPath
}

// TestMCPTool_IntegrateAdd_RoundTrip drives the same flow that a remote
// agent (Claude Code / Cursor) would: call tools/call agent_integrate_add
// → on-disk config picks up the entry → agent_integrate_list reflects it
// → agent_integrate_remove drops it.
func TestMCPTool_IntegrateAdd_RoundTrip(t *testing.T) {
	hs, cfgPath := setupServerWithConfig(t)
	dir := filepath.Dir(cfgPath)
	envPath := filepath.Join(dir, "agent-ops.env")

	// --- add
	raw := rpcCall(t, hs.URL, "tools/call", map[string]any{
		"name": "agent_integrate_add",
		"arguments": map[string]any{
			"name":      "zibby",
			"transport": "http",
			"url":       "https://api/mcp",
			"auth_env":  "ZBY",
			"token":     "secret",
			"extra_env": map[string]string{"AGENT_OPS_NOTIFY_WORKFLOW_ID": "wf_x"},
			"env_file":  envPath,
		},
	})
	addText := unwrapText(t, raw)
	if !strings.Contains(addText, `"restart_required": true`) {
		t.Errorf("add response missing restart_required:true: %s", addText)
	}
	body, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(body), "name: zibby") {
		t.Errorf("config.yaml did not pick up integration: %s", body)
	}
	envBody, _ := os.ReadFile(envPath)
	if !strings.Contains(string(envBody), "ZBY=secret") {
		t.Errorf("env file missing bearer: %s", envBody)
	}

	// --- list
	raw = rpcCall(t, hs.URL, "tools/call", map[string]any{
		"name": "agent_integrate_list", "arguments": map[string]any{},
	})
	listText := unwrapText(t, raw)
	if !strings.Contains(listText, `"zibby"`) || !strings.Contains(listText, `"count": 1`) {
		t.Errorf("list response unexpected: %s", listText)
	}
	// Critically, the response MUST NOT contain the bearer.
	if strings.Contains(listText, "secret") {
		t.Errorf("list response leaked bearer token: %s", listText)
	}

	// --- remove
	raw = rpcCall(t, hs.URL, "tools/call", map[string]any{
		"name":      "agent_integrate_remove",
		"arguments": map[string]any{"name": "zibby", "env_file": envPath},
	})
	if !strings.Contains(unwrapText(t, raw), `"restart_required": true`) {
		t.Errorf("remove response unexpected: %s", raw)
	}
}

// TestMCPTool_IntegrateAdd_AuthRequired pins the bearer-gate on the new
// tools. Same as every other tool — but worth a regression test because
// these touch on-disk config.
func TestMCPTool_IntegrateAdd_AuthRequired(t *testing.T) {
	hs, _ := setupServerWithConfig(t)
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_integrate_add","arguments":{}}}`)
	resp, err := http.Post(hs.URL+"/mcp", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "unauthorized") {
		t.Errorf("expected unauthorized; got %s", out)
	}
}

// TestMCPTool_IntegrateAdd_NoConfigPath shows the tool refuses to write
// when the daemon was constructed without a ConfigPath — keeps test
// harnesses from accidentally rewriting /etc/agent-ops/config.yaml.
func TestMCPTool_IntegrateAdd_NoConfigPath(t *testing.T) {
	hs, _, _ := setup(t) // shared helper from server_test.go — no ConfigPath
	raw := rpcCall(t, hs.URL, "tools/call", map[string]any{
		"name":      "agent_integrate_add",
		"arguments": map[string]any{"name": "x", "transport": "http", "url": "u"},
	})
	if !strings.Contains(unwrapText(t, raw), "without a config path") {
		t.Errorf("expected refusal message: %s", raw)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────

func unwrapText(t *testing.T, raw []byte) string {
	t.Helper()
	var resp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, raw)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %v", resp.Error)
	}
	if len(resp.Result.Content) == 0 {
		t.Fatalf("no content blocks: %s", raw)
	}
	return resp.Result.Content[0].Text
}

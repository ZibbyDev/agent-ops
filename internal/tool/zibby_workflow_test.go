// Copyright 2026 Zibby Lab. Apache-2.0.

package tool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubEnv returns an Env func that satisfies ZibbyWorkflowTool.Env.
// Tests pre-load the map with whichever subset of vars they want set.
func stubEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestZibbyWorkflow_HappyPath(t *testing.T) {
	var gotPath, gotAuth, gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"jobId":"job-abc123","status":"accepted","workflow":"notify-slack","version":4,"projectId":"proj-xyz","triggeredAt":"2026-05-22T12:00:00Z"}`))
	}))
	defer srv.Close()

	tl := NewZibbyWorkflowTool()
	tl.Env = stubEnv(map[string]string{
		"ZIBBY_API_BASE_URL": srv.URL,
		"ZIBBY_PAT_TOKEN":    "zby_pat_secret-test-token",
		"ZIBBY_PROJECT_ID":   "proj-xyz",
	})

	args := json.RawMessage(`{
        "workflow": "notify-slack",
        "input": {"channel":"#alerts","text":"app down"},
        "idempotency_key": "evt-42"
    }`)

	res, err := tl.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if !strings.Contains(res.Output, "ok") {
		t.Fatalf("expected ok in output, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "job-abc123") {
		t.Fatalf("expected job id in output, got: %s", res.Output)
	}
	if !res.Sensitive {
		t.Fatal("expected Sensitive=true (response carries PAT-authed body)")
	}

	// URL shape: /projects/{projectId}/workflows/{slug}/trigger
	if gotPath != "/projects/proj-xyz/workflows/notify-slack/trigger" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	// Auth header must be Bearer + PAT exactly
	if gotAuth != "Bearer zby_pat_secret-test-token" {
		t.Fatalf("unexpected Authorization header: %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("unexpected Content-Type: %q", gotContentType)
	}
	// Body shape: { input: {...}, idempotencyKey: "..." }
	var parsed struct {
		Input          map[string]any `json:"input"`
		IdempotencyKey string         `json:"idempotencyKey"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("body is not valid JSON: %v (raw=%s)", err, string(gotBody))
	}
	if parsed.Input["channel"] != "#alerts" {
		t.Fatalf("input.channel not forwarded: %+v", parsed.Input)
	}
	if parsed.IdempotencyKey != "evt-42" {
		t.Fatalf("idempotencyKey not forwarded: %q", parsed.IdempotencyKey)
	}
}

func TestZibbyWorkflow_DefaultInputIsEmptyObject(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"jobId":"job-x","status":"accepted"}`))
	}))
	defer srv.Close()

	tl := NewZibbyWorkflowTool()
	tl.Env = stubEnv(map[string]string{
		"ZIBBY_API_BASE_URL": srv.URL,
		"ZIBBY_PAT_TOKEN":    "zby_pat_x",
		"ZIBBY_PROJECT_ID":   "p1",
	})
	// No input field at all.
	_, err := tl.Invoke(context.Background(), json.RawMessage(`{"workflow":"ping"}`))
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("body invalid: %v", err)
	}
	inp, ok := parsed["input"].(map[string]any)
	if !ok {
		t.Fatalf("input should default to {} object, got %T %v", parsed["input"], parsed["input"])
	}
	if len(inp) != 0 {
		t.Fatalf("expected empty input, got %+v", inp)
	}
}

func TestZibbyWorkflow_BackendErrorIsForwardedNotRaised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"message":"Invalid workflow input","validationErrors":[{"path":"channel","kind":"missing"}]}`))
	}))
	defer srv.Close()

	tl := NewZibbyWorkflowTool()
	tl.Env = stubEnv(map[string]string{
		"ZIBBY_API_BASE_URL": srv.URL,
		"ZIBBY_PAT_TOKEN":    "zby_pat_x",
		"ZIBBY_PROJECT_ID":   "p1",
	})

	res, err := tl.Invoke(context.Background(), json.RawMessage(`{"workflow":"bad"}`))
	// LLM-facing errors come back as a Result, not a Go error — that's
	// the whole point so the LLM can decide to retry / give up.
	if err != nil {
		t.Fatalf("Invoke should not error on backend 4xx; got %v", err)
	}
	if !strings.Contains(res.Output, "http=400") {
		t.Fatalf("expected http=400 in output, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Invalid workflow input") {
		t.Fatalf("expected backend body in output, got: %s", res.Output)
	}
}

func TestZibbyWorkflow_GracefulRefusalWhenEnvMissing(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no base url", map[string]string{"ZIBBY_PAT_TOKEN": "x", "ZIBBY_PROJECT_ID": "p"}, "ZIBBY_API_BASE_URL"},
		{"no pat", map[string]string{"ZIBBY_API_BASE_URL": "https://x", "ZIBBY_PROJECT_ID": "p"}, "ZIBBY_PAT_TOKEN"},
		{"no project", map[string]string{"ZIBBY_API_BASE_URL": "https://x", "ZIBBY_PAT_TOKEN": "x"}, "ZIBBY_PROJECT_ID"},
		{"all empty", map[string]string{}, "ZIBBY_API_BASE_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := NewZibbyWorkflowTool()
			tl.Env = stubEnv(tc.env)
			res, err := tl.Invoke(context.Background(), json.RawMessage(`{"workflow":"ping"}`))
			if err != nil {
				t.Fatalf("graceful refusal should NOT raise; got: %v", err)
			}
			if !strings.Contains(res.Output, "not configured") {
				t.Fatalf("expected 'not configured' refusal, got: %s", res.Output)
			}
			if !strings.Contains(res.Output, tc.want) {
				t.Fatalf("expected missing var %q in output: %s", tc.want, res.Output)
			}
		})
	}
}

func TestZibbyWorkflow_RejectsEmptyWorkflowSlug(t *testing.T) {
	tl := NewZibbyWorkflowTool()
	tl.Env = stubEnv(map[string]string{
		"ZIBBY_API_BASE_URL": "https://api",
		"ZIBBY_PAT_TOKEN":    "x",
		"ZIBBY_PROJECT_ID":   "p",
	})
	if _, err := tl.Invoke(context.Background(), json.RawMessage(`{"workflow":""}`)); err == nil {
		t.Fatal("expected error on empty workflow slug")
	}
	if _, err := tl.Invoke(context.Background(), json.RawMessage(`{"workflow":"   "}`)); err == nil {
		t.Fatal("expected error on whitespace-only slug")
	}
}

func TestZibbyWorkflow_RejectsBadJSON(t *testing.T) {
	tl := NewZibbyWorkflowTool()
	if _, err := tl.Invoke(context.Background(), json.RawMessage(`not json`)); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestZibbyWorkflow_URLEscapesSlug(t *testing.T) {
	// r.RequestURI carries the raw on-wire form (pre-decoding).
	// r.URL.Path is already %-decoded by net/http's request parser, so
	// it's the wrong field to assert against.
	var gotRequestURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"jobId":"j"}`))
	}))
	defer srv.Close()

	tl := NewZibbyWorkflowTool()
	tl.Env = stubEnv(map[string]string{
		"ZIBBY_API_BASE_URL": srv.URL,
		"ZIBBY_PAT_TOKEN":    "x",
		"ZIBBY_PROJECT_ID":   "p1", // keep clean — slug is the LLM-controlled part
	})
	_, err := tl.Invoke(context.Background(), json.RawMessage(`{"workflow":"slug with spaces"}`))
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if !strings.Contains(gotRequestURI, "slug%20with%20spaces") {
		t.Fatalf("slug not URL-escaped on wire: RequestURI=%q", gotRequestURI)
	}
}

// sanity-check Tool interface compliance.
var _ Tool = (*ZibbyWorkflowTool)(nil)

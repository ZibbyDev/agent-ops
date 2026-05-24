// Copyright 2026 Zibby Lab. Apache-2.0.

package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ZibbyWorkflowTool calls a Zibby Cloud workflow as a tool. This is the
// "agent → user" comm channel — when agent-ops sees something noteworthy
// (verifier failed, app crashed, error spike), it can fire one of the
// user's pre-defined workflows (Slack notify, Lark notify, page on-call,
// open a Jira ticket, etc.) instead of just logging to stdout.
//
// The tool is project-scoped because Zibby's workflow trigger endpoint is
// project-scoped: POST /projects/{projectId}/workflows/{type}/trigger.
// The PAT injected into the daemon's env is the credential.
//
// Env vars consumed (all three required — tool is a no-op without them so
// a misconfigured deployment never accidentally calls a wrong API):
//   - ZIBBY_API_BASE_URL  e.g. https://api-prod.zibby.app
//   - ZIBBY_PAT_TOKEN     PAT (zby_pat_*), user-scoped, minted out-of-band
//   - ZIBBY_PROJECT_ID    Project to scope triggers to
//
// The PAT is sensitive — the tool's Result is marked Sensitive so MCP
// callers know not to ship it to log aggregators.
type ZibbyWorkflowTool struct {
	// HTTPClient is used for the outbound POST. Override in tests.
	HTTPClient *http.Client

	// Now returns the current time. Override in tests for determinism.
	Now func() time.Time

	// Env reads an environment variable. Override in tests so each test
	// can stand up its own ZIBBY_API_BASE_URL / ZIBBY_PAT_TOKEN /
	// ZIBBY_PROJECT_ID values without leaking into process env.
	Env func(string) string
}

// NewZibbyWorkflowTool returns a ZibbyWorkflowTool with sane defaults.
func NewZibbyWorkflowTool() *ZibbyWorkflowTool {
	return &ZibbyWorkflowTool{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Now:        time.Now,
		Env:        os.Getenv,
	}
}

func (t *ZibbyWorkflowTool) Name() string { return "zibby_workflow" }

func (t *ZibbyWorkflowTool) Description() string {
	return "Trigger a Zibby Cloud workflow by slug. Use this to reach the human operator " +
		"or external systems via user-configured pipelines — e.g. notify Slack/Lark, " +
		"page on-call, open a Jira ticket, create an incident. The workflow must already " +
		"be deployed in the user's Zibby project. Returns the workflow job id + status."
}

const zibbyWorkflowSchemaJSON = `{
  "type": "object",
  "properties": {
    "workflow": {
      "type": "string",
      "description": "Workflow slug (the workflowType — e.g. 'notify-slack', 'page-oncall'). Must match a workflow already deployed in the user's Zibby project."
    },
    "input": {
      "type": "object",
      "description": "Optional input object passed to the workflow as { input: {...} }. Fields are workflow-specific — read the workflow's inputSchema first if unsure.",
      "additionalProperties": true
    },
    "idempotency_key": {
      "type": "string",
      "description": "Optional idempotency key. If the same key is sent twice within the workflow's de-dup window, the second call is a no-op (returns the original job id)."
    }
  },
  "required": ["workflow"]
}`

func (t *ZibbyWorkflowTool) Schema() json.RawMessage {
	return json.RawMessage(zibbyWorkflowSchemaJSON)
}

type zibbyWorkflowArgs struct {
	Workflow       string          `json:"workflow"`
	Input          json.RawMessage `json:"input,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

// triggerBody is what the Zibby trigger endpoint accepts. Mirrors the
// shape in backend/src/handlers/workflow-trigger.js — `input` is the only
// required field. Keep field names in lockstep with that handler.
type triggerBody struct {
	Input          json.RawMessage `json:"input"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
}

// triggerResponse is the 202 shape from workflow-trigger.js. Other fields
// (workflow, version, triggeredAt) are forwarded verbatim to the LLM via
// the formatted Output blob, but we explicitly grab jobId + status so we
// can produce a structured one-line summary on success.
type triggerResponse struct {
	JobID       string `json:"jobId"`
	Status      string `json:"status"`
	Workflow    string `json:"workflow"`
	Version     any    `json:"version"`
	ProjectID   string `json:"projectId"`
	TriggeredAt string `json:"triggeredAt"`
}

func (t *ZibbyWorkflowTool) Invoke(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a zibbyWorkflowArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return Result{}, fmt.Errorf("zibby_workflow: parse args: %w", err)
	}
	if strings.TrimSpace(a.Workflow) == "" {
		return Result{}, errors.New("zibby_workflow: workflow (slug) is required")
	}

	baseURL := strings.TrimSpace(t.Env("ZIBBY_API_BASE_URL"))
	pat := strings.TrimSpace(t.Env("ZIBBY_PAT_TOKEN"))
	projectID := strings.TrimSpace(t.Env("ZIBBY_PROJECT_ID"))

	// Graceful refusal. The tool's whole point is reaching out to the
	// user's Zibby — if any of these are unset the deployment was never
	// fully wired. Surface to the LLM as a tool_result string so it can
	// decide to fall back to logging, instead of returning a hard error
	// which the driver would propagate as a task failure.
	if baseURL == "" || pat == "" || projectID == "" {
		missing := []string{}
		if baseURL == "" {
			missing = append(missing, "ZIBBY_API_BASE_URL")
		}
		if pat == "" {
			missing = append(missing, "ZIBBY_PAT_TOKEN")
		}
		if projectID == "" {
			missing = append(missing, "ZIBBY_PROJECT_ID")
		}
		return Result{
			Output: fmt.Sprintf(
				"zibby_workflow: not configured — missing env: %s. "+
					"This deployment cannot reach Zibby Cloud; fall back to logging the event instead.",
				strings.Join(missing, ", ")),
		}, nil
	}

	// URL: /projects/{projectId}/workflows/{slug}/trigger. Both pieces
	// are user-controlled (slug from LLM, projectId from env), so encode
	// each path segment. PathEscape over QueryEscape — slugs with "+"
	// must stay literal.
	endpoint := strings.TrimRight(baseURL, "/") +
		"/projects/" + url.PathEscape(projectID) +
		"/workflows/" + url.PathEscape(a.Workflow) + "/trigger"

	// Build the body. Default `input: {}` so the backend's JSON.parse
	// gets a valid object even when the LLM omits input entirely.
	inputJSON := a.Input
	if len(bytes.TrimSpace(inputJSON)) == 0 {
		inputJSON = json.RawMessage(`{}`)
	}
	bodyStruct := triggerBody{
		Input:          inputJSON,
		IdempotencyKey: a.IdempotencyKey,
	}
	bodyBytes, err := json.Marshal(bodyStruct)
	if err != nil {
		return Result{}, fmt.Errorf("zibby_workflow: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return Result{}, fmt.Errorf("zibby_workflow: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Authorization MUST be `Bearer ${PAT}` — the authorizer Lambda
	// strips the "Bearer " prefix before matching `zby_pat_*`.
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("User-Agent", "agent-ops/zibby_workflow")

	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		// Network-level failure — surface as tool result, not a runner
		// error. The LLM can retry / decide.
		return Result{
			Output:    fmt.Sprintf("zibby_workflow: network error calling %s: %v", endpoint, err),
			Sensitive: true,
		}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var parsed triggerResponse
		_ = json.Unmarshal(respBody, &parsed) // best-effort; raw body is the fallback
		summary := fmt.Sprintf(
			"zibby_workflow: ok\nworkflow=%s\njob_id=%s\nstatus=%s\nproject_id=%s\nhttp=%d\nraw=%s",
			a.Workflow,
			parsed.JobID,
			parsed.Status,
			parsed.ProjectID,
			resp.StatusCode,
			string(respBody),
		)
		return Result{Output: summary, Sensitive: true}, nil
	}

	// 4xx/5xx: include status + body so the LLM can debug. Body is
	// already capped at 16KB by LimitReader above.
	return Result{
		Output: fmt.Sprintf(
			"zibby_workflow: error\nhttp=%d\nworkflow=%s\nbody=%s",
			resp.StatusCode, a.Workflow, string(respBody)),
		Sensitive: true,
	}, nil
}

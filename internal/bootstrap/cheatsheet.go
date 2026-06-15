// Copyright 2026 Zibby Lab. Apache-2.0.

// cheatsheet.go — bootstrap mode #4: catalog ships a HINT (image + steps +
// known pitfalls), the agent (claudecli driver) does the actual install and
// adapts on errors. Sits alongside script-mode (mode 1), multi-service
// Docker (mode 2), and goal-mode (mode 3) — does NOT replace any of them.
//
// Why a fourth mode instead of fancier goal-mode prompts: catalog entries
// know facts (port, image, proven build steps) that we'd otherwise have to
// pay tokens for the model to rediscover every cold start. Cheatsheet mode
// is the cheapest LLM-driven path — pre-loaded with the happy path, allowed
// to deviate on error. Pure goal-mode (mode 3) stays for free-form
// "install X" requests that don't have a catalog row.
//
// Dispatch: triggered by AGENT_OPS_BOOTSTRAP_MODE=cheatsheet in
// MaybeRunFirstRun. The cheatsheet block (catalog JSON) ships as
// AGENT_OPS_CHEATSHEET_JSON. We parse it, assemble system + user prompts,
// then route through the SAME sched.RunNow + claudecli driver path as
// goal-mode — no new driver, no new subprocess plumbing.

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ZibbyDev/agent-ops/internal/config"
	"github.com/ZibbyDev/agent-ops/internal/scheduler"
	"github.com/ZibbyDev/agent-ops/internal/state"
)

// Cheatsheet mirrors the catalog `cheatsheet` block. All optional except
// Port — without a port the install has no health-probe target and the
// ALB target group never goes healthy. Validated in Parse().
//
// max_turns / token_budget_usd guard against a runaway agent burning the
// operator's Anthropic credit on a doomed install. Defaults applied in
// applyDefaults().
type Cheatsheet struct {
	RecommendedImage string             `json:"recommended_image,omitempty"`
	RecommendedSteps []string           `json:"recommended_steps,omitempty"`
	StartCommand     string             `json:"start_command,omitempty"`
	Port             int                `json:"port"`
	Env              map[string]string  `json:"env,omitempty"`
	KnownPitfalls    []CheatsheetPitfall `json:"known_pitfalls,omitempty"`
	MaxTurns         int                `json:"max_turns,omitempty"`
	TokenBudgetUSD   float64            `json:"token_budget_usd,omitempty"`
}

// CheatsheetPitfall is one entry in the known_pitfalls list. Phrased as
// {symptom, fix} so the agent can grep its own error output against the
// symptom string and reach for the recommended fix without re-deriving.
type CheatsheetPitfall struct {
	Symptom string `json:"symptom"`
	Fix     string `json:"fix"`
}

// Defaults — used both as floor (when JSON omits the field) and as cap
// (when JSON specifies something obviously wrong like MaxTurns=0).
const (
	defaultCheatsheetMaxTurns       = 50
	defaultCheatsheetTokenBudgetUSD = 0.50
)

// isCheatsheetMode reports whether the operator/control-plane requested
// cheatsheet bootstrap via AGENT_OPS_BOOTSTRAP_MODE=cheatsheet (case-
// insensitive). Defined here (not bootstrap.go) so the cheatsheet logic
// lives in one file and the bootstrap dispatcher only has to call this.
func isCheatsheetMode() bool {
	return strings.EqualFold(
		strings.TrimSpace(os.Getenv("AGENT_OPS_BOOTSTRAP_MODE")),
		"cheatsheet",
	)
}

// ParseCheatsheet decodes AGENT_OPS_CHEATSHEET_JSON and applies defaults.
// Returns a fully-validated Cheatsheet ready to feed BuildPrompts.
func ParseCheatsheet(raw string) (*Cheatsheet, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("cheatsheet: AGENT_OPS_CHEATSHEET_JSON is empty")
	}
	var c Cheatsheet
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("cheatsheet: parse JSON: %w", err)
	}
	if c.Port < 1 || c.Port > 65535 {
		return nil, fmt.Errorf("cheatsheet: port must be 1..65535 (got %d)", c.Port)
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Cheatsheet) applyDefaults() {
	if c.MaxTurns <= 0 {
		c.MaxTurns = defaultCheatsheetMaxTurns
	}
	if c.TokenBudgetUSD <= 0 {
		c.TokenBudgetUSD = defaultCheatsheetTokenBudgetUSD
	}
}

// BuildPrompts assembles the (system, user) prompt pair the claudecli
// driver receives. appType is the AGENT_OPS_APP_TYPE env (e.g.
// "open-design"). The system prompt is fact-dense; the user prompt is
// a short imperative pointing at the system block's success criteria.
//
// Kept verbatim-stable so dashboard / test assertions can substring-match
// without surprise.
func (c *Cheatsheet) BuildPrompts(appType string) (system, user string) {
	if strings.TrimSpace(appType) == "" {
		appType = "app"
	}
	image := strings.TrimSpace(c.RecommendedImage)
	if image == "" {
		image = `none — build from source per steps below`
	}
	start := strings.TrimSpace(c.StartCommand)
	if start == "" {
		start = `(unspecified — derive from steps)`
	}

	var stepsBlock strings.Builder
	if len(c.RecommendedSteps) == 0 {
		stepsBlock.WriteString("(no preset steps — improvise)")
	} else {
		for i, s := range c.RecommendedSteps {
			fmt.Fprintf(&stepsBlock, "  %d. %s\n", i+1, s)
		}
	}

	var envBlock strings.Builder
	if len(c.Env) > 0 {
		envBlock.WriteString("Known-good env (export before running steps):\n")
		// Stable iteration order so prompt is reproducible (Go map iter is
		// randomised — bites prompt-snapshot tests + caches alike).
		keys := sortedKeys(c.Env)
		for _, k := range keys {
			fmt.Fprintf(&envBlock, "  %s=%s\n", k, c.Env[k])
		}
	}

	var pitfallsBlock strings.Builder
	if len(c.KnownPitfalls) == 0 {
		pitfallsBlock.WriteString("(none recorded)")
	} else {
		for i, p := range c.KnownPitfalls {
			fmt.Fprintf(&pitfallsBlock,
				"  %d. symptom: %s\n     fix:     %s\n", i+1, p.Symptom, p.Fix)
		}
	}

	system = fmt.Sprintf(`You are agent-ops installing the %s app on this Fargate container.
Use the cheatsheet below as a STARTING POINT — adapt freely when steps fail.

==== CHEATSHEET ====
Recommended image: %s
Port to bind: %d
Recommended steps:
%sStart command: %s
%sKnown pitfalls (try the fix if you see the symptom):
%s==== END CHEATSHEET ====

Success criteria: the app responds on http://127.0.0.1:%d within 12 minutes.

Tools: Bash, Read, Edit. You have root inside the container.

If the cheatsheet says X but X fails, try the suggested fix or your own alternative — DON'T just report the failure. Self-healing is the whole point of this mode. Only declare failure if every reasonable attempt is exhausted.`,
		appType,
		image,
		c.Port,
		stepsBlock.String(),
		start,
		envBlock.String(),
		pitfallsBlock.String(),
		c.Port,
	)

	user = fmt.Sprintf(
		`Install %s. Cheatsheet attached above. Verify health on :%d before reporting done.`,
		appType, c.Port,
	)
	return system, user
}

// sortedKeys returns m's keys in lexicographic order — small alloc, no
// stdlib import beyond what's already in this file.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion sort — env maps are small (< 20 entries in practice).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// runCheatsheetBootstrap is the LLM-driven counterpart of
// runScriptBootstrap. Parses the cheatsheet env, builds prompts, then
// routes through sched.RunNow — same path goal-mode (mode 3) uses, so
// claudecli's stream-json progress logs + cost accounting carry over
// without changes.
//
// Token budget enforcement is POST-HOC: we read run.CostUSDMicro after
// completion and add a fact to the store flagging budget overrun. The
// claudecli subprocess does NOT support mid-run cost callbacks today
// (--total-cost-usd only fires at the {"type":"result"} terminal event,
// see claudecli/claudecli.go:233). MaxTurns enforces a HARD ceiling on
// LLM iterations, which is the real runaway-cost guard — token budget
// is a soft observability hook for operator-side alerting.
func runCheatsheetBootstrap(
	ctx context.Context,
	cfg *config.Config,
	sched *scheduler.Scheduler,
	store *state.Store,
) (*state.TaskRun, error) {
	raw := os.Getenv("AGENT_OPS_CHEATSHEET_JSON")
	cs, err := ParseCheatsheet(raw)
	if err != nil {
		return nil, err
	}
	appType := strings.TrimSpace(os.Getenv("AGENT_OPS_APP_TYPE"))

	system, user := cs.BuildPrompts(appType)

	// Synthesize a Schedule from the cheatsheet env so the existing
	// agent-mode bootstrap dispatch (cfg.Bootstrap.Prompt + sched.RunNow)
	// can be reused verbatim. The prompt we hand to RunNow is the
	// concatenated system+user pair — the claudecli driver internally
	// re-splits on the "\n\n" boundary (see claudecli.go:94-96), so
	// the model sees a clean system / user separation.
	bs := &config.Schedule{
		Name:   "bootstrap",
		Cron:   "@yearly",
		Prompt: system + "\n\n" + user,
		Tools:  []string{"shell"}, // claudecli ignores agent-ops tool registry; this is for the schedule record only
	}
	cfg.Bootstrap = bs

	taskName := bs.Name
	t := state.Task{
		Name:    taskName,
		Cron:    bs.Cron,
		Prompt:  bs.Prompt,
		Tools:   bs.Tools,
		Enabled: false,
	}
	if err := store.UpsertTask(ctx, t); err != nil {
		return nil, fmt.Errorf("cheatsheet: upsert task: %w", err)
	}

	slog.Info("cheatsheet: invoking bootstrap",
		"app_type", appType,
		"port", cs.Port,
		"recommended_image", cs.RecommendedImage,
		"step_count", len(cs.RecommendedSteps),
		"pitfall_count", len(cs.KnownPitfalls),
		"max_turns", cs.MaxTurns,
		"token_budget_usd", cs.TokenBudgetUSD,
	)

	run, err := sched.RunNow(ctx, taskName, bs.Prompt)
	if err != nil {
		return nil, fmt.Errorf("cheatsheet: run failed: %w", err)
	}

	// Post-hoc budget check. We don't FAIL the bootstrap on overrun —
	// the model may have legitimately spent the budget and still
	// produced a working install. Instead we surface the overrun loudly
	// in logs + fact store so the operator can tune the budget for the
	// next deploy.
	costUSD := float64(run.CostUSDMicro) / 1_000_000
	overrun := costUSD > cs.TokenBudgetUSD
	slog.Info("cheatsheet: bootstrap complete",
		"run_id", run.ID,
		"status", run.Status,
		"tool_calls", run.ToolCalls,
		"cost_usd", costUSD,
		"budget_usd", cs.TokenBudgetUSD,
		"budget_overrun", overrun,
		"error", run.Error,
	)
	if overrun {
		_, _ = store.AddFact(ctx, "cheatsheet",
			fmt.Sprintf("budget_overrun at %s: cost=$%.4f budget=$%.4f",
				time.Now().UTC().Format(time.RFC3339), costUSD, cs.TokenBudgetUSD))
	}
	_, _ = store.AddFact(ctx, "cheatsheet",
		fmt.Sprintf("bootstrap_%s at %s: turns=%d cost=$%.4f",
			func() string {
				if run.Error != "" {
					return "failed"
				}
				return "ok"
			}(),
			time.Now().UTC().Format(time.RFC3339), run.ToolCalls, costUSD))

	return &run, nil
}

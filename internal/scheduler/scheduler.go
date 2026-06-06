// Copyright 2026 Zibby Lab. Apache-2.0.

// Package scheduler is the cron-driven dispatcher.
//
// On boot:
//   - hydrate Task rows from State (so MCP-side edits survive restart)
//   - merge with config's `schedules` (config wins if both define a task)
//   - register a robfig/cron entry per enabled Task that calls Runner.Run
//
// The scheduler is intentionally minimal: every tick spawns a goroutine that
// calls Runner.Run, which itself handles single-flight + persistence. The
// scheduler doesn't track in-flight state.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/robfig/cron/v3"

	"github.com/ZibbyHQ/agent-ops/internal/config"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/task"
)

// Scheduler wraps robfig/cron with daemon-specific glue.
type Scheduler struct {
	cron   *cron.Cron
	runner *task.Runner
	store  *state.Store
	log    *slog.Logger

	mu      sync.Mutex
	entries map[string]cron.EntryID // task name → entry id

	// modelOverrides maps taskName → model id from cfg.Schedules / cfg.Bootstrap.
	// We don't persist Model in SQLite because the YAML config is authoritative;
	// holding the per-task override in memory avoids a schema migration and
	// keeps MCP-side task edits (which set cron / prompt / tools, not model)
	// from accidentally clobbering the model choice.
	modelOverrides map[string]string
}

// New returns a Scheduler. The caller must call Start before Add* takes effect.
func New(runner *task.Runner, store *state.Store, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		cron: cron.New(cron.WithParser(cron.NewParser(
			cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
		))),
		runner:         runner,
		store:          store,
		log:            logger,
		entries:        map[string]cron.EntryID{},
		modelOverrides: map[string]string{},
	}
}

// Start begins firing scheduled ticks.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop blocks until all running ticks have finished. Use during shutdown.
func (s *Scheduler) Stop(ctx context.Context) error {
	stopCtx := s.cron.Stop()
	select {
	case <-stopCtx.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Hydrate loads tasks from the State store + merges with config schedules,
// then registers each enabled task. Config wins on conflict because the
// operator's source-controlled config is more authoritative than DB state
// after a manual MCP edit (which the user can re-apply).
func (s *Scheduler) Hydrate(ctx context.Context, cfg *config.Config) error {
	stored, err := s.store.ListTasks(ctx)
	if err != nil {
		return fmt.Errorf("scheduler.Hydrate: list: %w", err)
	}
	byName := make(map[string]state.Task, len(stored))
	for _, t := range stored {
		byName[t.Name] = t
	}
	// Config-defined tasks overwrite any prior persisted version.
	s.mu.Lock()
	for _, sched := range cfg.Schedules {
		if sched.Model != "" {
			s.modelOverrides[sched.Name] = sched.Model
		}
	}
	// Bootstrap's model override flows in via the same map so RunNow picks
	// it up when bootstrap.MaybeRunFirstRun invokes the task by name.
	if cfg.Bootstrap != nil && cfg.Bootstrap.Model != "" {
		s.modelOverrides[cfg.Bootstrap.Name] = cfg.Bootstrap.Model
	}
	s.mu.Unlock()

	for _, sched := range cfg.Schedules {
		enabled := sched.Enabled == nil || *sched.Enabled
		t := state.Task{
			Name:    sched.Name,
			Cron:    sched.Cron,
			Prompt:  sched.Prompt,
			Tools:   sched.Tools,
			Enabled: enabled,
		}
		if err := s.store.UpsertTask(ctx, t); err != nil {
			return fmt.Errorf("scheduler.Hydrate: upsert %q: %w", sched.Name, err)
		}
		byName[sched.Name] = t
	}

	for _, t := range byName {
		if !t.Enabled {
			continue
		}
		if err := s.register(t); err != nil {
			return err
		}
	}
	return nil
}

// register adds (or replaces) a cron entry for the given Task.
func (s *Scheduler) register(t state.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.entries[t.Name]; ok {
		s.cron.Remove(old)
		delete(s.entries, t.Name)
	}
	job := s.makeJob(t)
	id, err := s.cron.AddFunc(t.Cron, job)
	if err != nil {
		return fmt.Errorf("scheduler.register: %q (cron=%q): %w", t.Name, t.Cron, err)
	}
	s.entries[t.Name] = id
	s.log.Info("scheduler: task registered", "name", t.Name, "cron", t.Cron)
	return nil
}

func (s *Scheduler) makeJob(t state.Task) func() {
	return func() {
		// Re-read the task fresh in case an MCP update happened between
		// registration and tick — the cron entry points at this closure
		// but the prompt/tools may have moved.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cur, err := s.store.GetTask(ctx, t.Name)
		if err != nil {
			s.log.Error("scheduler: get task", "name", t.Name, "error", err)
			return
		}
		if !cur.Enabled {
			s.log.Info("scheduler: task disabled, skipping tick", "name", cur.Name)
			return
		}
		// Defensive: when the daemon booted with the scheduler gated off
		// (no Claude token / agent-ops disabled) the runner is nil and the
		// cron loop was never Start()ed, so this closure shouldn't fire — but
		// guard anyway so a stray tick can't nil-panic the daemon.
		if s.runner == nil {
			s.log.Info("scheduler: runner not configured (scheduler gated off), skipping tick", "name", cur.Name)
			return
		}

		_, _, runErr := s.runner.Run(ctx, task.Spec{
			Name:    cur.Name,
			Trigger: task.TriggerSchedule,
			Prompt:  appendNotifyClause(cur.Prompt),
			Tools:   cur.Tools,
			Model:   s.modelFor(cur.Name),
		})
		if runErr != nil {
			s.log.Error("scheduler: run failed", "name", cur.Name, "error", runErr)
		}
	}
}

// appendNotifyClause optionally appends a notification-tool instruction to the
// user prompt, controlled by the AGENT_OPS_NOTIFY_WORKFLOW_ID env var. When
// the env is unset (or blank after trim) the prompt is returned unchanged so
// non-Zibby deploys see zero behavior change. When set, the LLM is told to
// shell-out to `zibby workflow trigger` with the configured workflow id ONLY
// after attempting recovery — matching the autonomous-mode contract in
// defaultSystemPrompt.
//
// We intentionally shell-out (rather than expose a Go-side MCP tool) so the
// Zibby API contract has exactly one source of truth: the @zibby/cli package
// installed in the Zibby flavour image. This eliminates the drift twin we used
// to maintain in internal/tool/zibby_workflow.go.
//
// Exposed as a package-level function (not a method) so it composes cleanly
// with whichever Spec.Prompt source is in use — schedule tick, RunNow, future
// callers — without each call site needing to know about the env var.
func appendNotifyClause(prompt string) string {
	wfID := strings.TrimSpace(os.Getenv("AGENT_OPS_NOTIFY_WORKFLOW_ID"))
	if wfID == "" {
		return prompt
	}
	clause := "\n\nIf the application is down AND you cannot recover it (soft-restart did not\n" +
		"bring it back), run the shell command:\n" +
		"  zibby workflow trigger " + wfID + " \\\n" +
		"    --input instance_id=\"$INSTANCE_ID\" \\\n" +
		"    --input error=\"<your one-line summary>\" \\\n" +
		"    --input severity=\"high\"\n" +
		"so the operator gets paged."
	return prompt + clause
}

// SetTask atomically upserts + re-registers (or removes) a task. Called by the
// MCP layer when a user updates a schedule.
func (s *Scheduler) SetTask(ctx context.Context, t state.Task) error {
	if t.Name == "" {
		return errors.New("scheduler.SetTask: Name required")
	}
	if t.Enabled && t.Cron == "" {
		return errors.New("scheduler.SetTask: enabled task requires Cron")
	}
	if err := s.store.UpsertTask(ctx, t); err != nil {
		return err
	}
	if !t.Enabled {
		s.mu.Lock()
		if id, ok := s.entries[t.Name]; ok {
			s.cron.Remove(id)
			delete(s.entries, t.Name)
		}
		s.mu.Unlock()
		return nil
	}
	return s.register(t)
}

// RunNow triggers an immediate ad-hoc run of the named task. Returns the run
// id once the runner has persisted it. Synchronous — blocks until completion.
func (s *Scheduler) RunNow(ctx context.Context, taskName, overridePrompt string) (state.TaskRun, error) {
	// When the scheduler is gated off the daemon has no driver/runner wired
	// (see daemon.Run). The MCP server is still up, so an operator could call
	// agent_run_now — fail clearly rather than nil-panic.
	if s.runner == nil {
		return state.TaskRun{}, errors.New("scheduler.RunNow: agent-ops scheduler is disabled (no Claude token configured or AGENT_OPS_SCHEDULER_ENABLED=false) — nothing to run")
	}
	t, err := s.store.GetTask(ctx, taskName)
	if err != nil {
		return state.TaskRun{}, fmt.Errorf("scheduler.RunNow: get task %q: %w", taskName, err)
	}
	prompt := t.Prompt
	if overridePrompt != "" {
		prompt = overridePrompt
	}
	run, _, err := s.runner.Run(ctx, task.Spec{
		Name:    t.Name,
		Trigger: task.TriggerManual,
		Prompt:  appendNotifyClause(prompt),
		Tools:   t.Tools,
		Model:   s.modelFor(t.Name),
	})
	return run, err
}

// SetModelOverride lets callers (e.g. bootstrap.MaybeRunFirstRun's verifier
// pass, or future MCP tools) wire a per-task model that wasn't loaded from
// cfg at Hydrate time. Empty model clears the override.
func (s *Scheduler) SetModelOverride(taskName, model string) {
	if taskName == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if model == "" {
		delete(s.modelOverrides, taskName)
		return
	}
	s.modelOverrides[taskName] = model
}

// modelFor returns the cached per-task model override (empty string falls
// back to the driver's configured default in Driver.Run).
func (s *Scheduler) modelFor(taskName string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelOverrides[taskName]
}

// Entries returns the currently-registered task names (for debugging / MCP).
func (s *Scheduler) Entries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for name := range s.entries {
		out = append(out, name)
	}
	return out
}

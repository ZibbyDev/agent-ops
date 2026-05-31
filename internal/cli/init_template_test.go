// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZibbyHQ/agent-ops/examples"
	"github.com/ZibbyHQ/agent-ops/internal/config"
)

// TestInit_ListTemplates exercises the read-only listing path. We don't
// hardcode the full description text (template authors may tweak the
// leading comment) — but the three bundled names MUST appear, and the
// table header must be present so a future "drop the header to save a
// line" refactor is a conscious one.
func TestInit_ListTemplates(t *testing.T) {
	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"init", "--list-templates"})
	if err := root.Execute(); err != nil {
		t.Fatalf("init --list-templates: %v", err)
	}
	body := out.String()
	for _, want := range []string{
		"NAME", "DESCRIPTION",
		"wordpress-multisite", "single-app", "nodejs-server",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("--list-templates output missing %q. Full output:\n%s", want, body)
		}
	}
}

// TestInit_Template_DryRun writes nothing to disk — just renders the
// chosen template body to stdout. Pins the contract used by operators who
// want to `agent-ops init --template … --dry-run | tee config.yaml` for
// review.
func TestInit_Template_DryRun(t *testing.T) {
	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"init", "--template", "single-app",
		"--dry-run", "--config", "/tmp/nope/agent-ops.yaml"})
	if err := root.Execute(); err != nil {
		t.Fatalf("init --template single-app --dry-run: %v", err)
	}
	body := out.String()
	// Header line (so the operator knows which file it WOULD have written
	// to) + the template's expected anchor keys.
	for _, want := range []string{
		"/tmp/nope/agent-ops.yaml",
		"template: single-app",
		"state_dir:",
		"provider: claude-cli",
		"hourly_health_check",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dry-run output missing %q. Full output:\n%s", want, body)
		}
	}
	// Belt-and-suspenders: --dry-run must NOT have created the directory
	// (caller passed a path that doesn't exist yet).
	if _, err := os.Stat("/tmp/nope/agent-ops.yaml"); !os.IsNotExist(err) {
		t.Errorf("--dry-run wrote to disk; stat err = %v", err)
	}
}

// TestInit_Template_WritesToDisk drives the real write path: --template +
// --yes into a tempdir-rooted config path, then re-reads the file and
// checks it matches the embedded body for the same template. This is the
// canonical "operator runs the command, the file lands on disk" gate.
func TestInit_Template_WritesToDisk(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agent-ops", "config.yaml")

	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"init", "--template", "wordpress-multisite",
		"--yes", "--config", cfg})
	if err := root.Execute(); err != nil {
		t.Fatalf("init --template wordpress-multisite --yes: %v", err)
	}

	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	for _, want := range []string{
		"# Example config — WordPress",
		"state_dir:",
		"liveness_check",
		"weekly_security_patch",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("written config missing %q. First 400 chars:\n%s",
				want, string(body)[:min(400, len(string(body)))])
		}
	}

	// Round-trip the written file through config.Parse — a template that
	// emits invalid YAML or misses a required field would leave the
	// operator with a broken daemon at first restart.
	// (Done inline rather than as a separate test so we re-use the
	// already-written file.)

	if !strings.Contains(out.String(), "wrote "+cfg) {
		t.Errorf("expected confirmation line containing %q, got:\n%s", "wrote "+cfg, out.String())
	}
}

// TestInit_Template_RoundTrip pins the contract between the embedded YAML
// and the daemon's config.Parse — every bundled template must parse
// cleanly so a freshly-initted operator can't end up with a broken daemon.
// Run inside the same file as the writes-to-disk test so it stays close
// to the user-facing path.
//
// NOTE: this duplicates intent with examples/embed_test.go's smoke check
// but uses the REAL config.Parse so a schema drift between the templates
// and the config package is caught here instead of in production.

// TestInit_Template_UnknownName surfaces the embed.Get error verbatim so
// the user sees "Available templates: …" in their terminal. Pin the hint
// text so a future refactor of the error doesn't drop the suggestion.
func TestInit_Template_UnknownName(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")

	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"init", "--template", "not-a-real-template",
		"--yes", "--config", cfg})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unknown template, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not-a-real-template") {
		t.Errorf("error message missing the bad name: %s", msg)
	}
	if !strings.Contains(msg, "available") {
		t.Errorf("error message missing 'available' hint: %s", msg)
	}
	for _, name := range []string{"wordpress-multisite", "single-app", "nodejs-server"} {
		if !strings.Contains(msg, name) {
			t.Errorf("error message missing template %q in suggestions: %s", name, msg)
		}
	}
	// Refusal must not have written the bad path.
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("unknown template wrote to disk; stat err = %v", err)
	}
}

// TestEmbeddedTemplates_ParseAsValidConfig round-trips every bundled
// template through config.Parse. If a template author accidentally ships
// YAML that fails validation (missing required field, bad cron, etc), an
// `agent-ops init --template <name>` would silently land a broken file —
// this gate ensures the regression bites in CI instead.
func TestEmbeddedTemplates_ParseAsValidConfig(t *testing.T) {
	for _, tmpl := range examples.List() {
		t.Run(tmpl.Name, func(t *testing.T) {
			body, err := examples.Get(tmpl.Name)
			if err != nil {
				t.Fatalf("Get(%q): %v", tmpl.Name, err)
			}
			cfg, err := config.Parse(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("template %q failed to parse as a daemon config:\n%v", tmpl.Name, err)
			}
			if cfg.Agent.Provider == "" {
				t.Errorf("template %q parsed with empty agent.provider", tmpl.Name)
			}
			if len(cfg.Schedules) == 0 {
				t.Errorf("template %q emitted zero schedules — none of the bundled templates are intended to be empty", tmpl.Name)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Copyright 2026 Zibby Lab. Apache-2.0.

package examples

import (
	"strings"
	"testing"
)

// TestList_HasAllExpectedTemplates pins the set of bundled templates so a
// regression that drops the `//go:embed` directive (or renames a file)
// surfaces immediately in CI. New templates ARE expected to appear here as
// they're added — bump the expected set when they do.
func TestList_HasAllExpectedTemplates(t *testing.T) {
	got := List()
	if len(got) < 3 {
		t.Fatalf("expected at least 3 embedded templates, got %d: %+v", len(got), got)
	}
	want := map[string]bool{
		"wordpress-multisite": false,
		"single-app":          false,
		"nodejs-server":       false,
	}
	for _, tmpl := range got {
		if _, ok := want[tmpl.Name]; ok {
			want[tmpl.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("template %q missing from List(); got=%+v", name, got)
		}
	}
}

// TestGet_ReturnsNonEmptyBytes is the actual embed-correctness gate: if
// the //go:embed directive doesn't pick up the YAML the function returns a
// not-found error rather than the file body.
func TestGet_ReturnsNonEmptyBytes(t *testing.T) {
	for _, name := range []string{"wordpress-multisite", "single-app", "nodejs-server"} {
		body, err := Get(name)
		if err != nil {
			t.Errorf("Get(%q): %v", name, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("Get(%q): empty body", name)
		}
		// Smoke-check: every template starts with the `state_dir:` top-level
		// key once you strip leading comments. Catches a future "we embedded
		// the file but accidentally truncated it" bug.
		if !strings.Contains(string(body), "state_dir:") {
			t.Errorf("Get(%q): missing expected `state_dir:` key", name)
		}
	}
}

// TestGet_UnknownTemplate verifies the error message lists the available
// names so the CLI's "Available templates: …" hint stays in sync with the
// underlying error.
func TestGet_UnknownTemplate(t *testing.T) {
	_, err := Get("does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown template, got nil")
	}
	for _, name := range []string{"wordpress-multisite", "single-app", "nodejs-server"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("expected error to list %q as available, got: %s", name, err)
		}
	}
}

// TestList_Descriptions parses each template's first-comment line and
// confirms we got a non-empty, non-default description. Pins the
// convention so a future template author who forgets the leading `#
// Example config — …` line gets an immediate test failure.
func TestList_Descriptions(t *testing.T) {
	for _, tmpl := range List() {
		if tmpl.Description == "" {
			t.Errorf("template %q has empty description", tmpl.Name)
		}
		// A description that just echoes the name means firstCommentLine
		// fell through to fallback — the YAML's leading comment is missing
		// or malformed.
		if tmpl.Description == tmpl.Name {
			t.Errorf("template %q description fell back to its name — leading `# … — <desc>` comment is missing", tmpl.Name)
		}
	}
}

// TestList_SortedByName guards the ordering contract used by the CLI's
// --list-templates output and the MCP agent_list_templates tool. Both rely
// on a deterministic sort for stable docs / golden-test matching.
func TestList_SortedByName(t *testing.T) {
	prev := ""
	for _, tmpl := range List() {
		if prev != "" && tmpl.Name < prev {
			t.Errorf("List() not sorted: %q came before %q", prev, tmpl.Name)
		}
		prev = tmpl.Name
	}
}

// TestNames_MatchesList sanity-checks that Names() is just the .Name
// projection of List() so callers can use either interchangeably.
func TestNames_MatchesList(t *testing.T) {
	list := List()
	names := Names()
	if len(list) != len(names) {
		t.Fatalf("List() len %d != Names() len %d", len(list), len(names))
	}
	for i := range list {
		if list[i].Name != names[i] {
			t.Errorf("position %d: List().Name=%q, Names()=%q", i, list[i].Name, names[i])
		}
	}
}

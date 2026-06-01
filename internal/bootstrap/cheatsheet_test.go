// Copyright 2026 Zibby Lab. Apache-2.0.

package bootstrap

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseCheatsheet_Valid round-trips a realistic cheatsheet block —
// the open-design POC entry — and verifies defaults kick in for absent
// max_turns / token_budget_usd. Mirrors what the backend handler emits
// into AGENT_OPS_CHEATSHEET_JSON.
func TestParseCheatsheet_Valid(t *testing.T) {
	raw := `{
		"recommended_image": "",
		"recommended_steps": ["apt-get update", "git clone https://example.com/foo"],
		"start_command": "node apps/daemon/dist/cli.js --no-open",
		"port": 7456,
		"env": {"OD_PORT": "7456", "OD_DATA_DIR": "/var/lib/agent-ops/open-design"},
		"known_pitfalls": [
			{"symptom": "OOM", "fix": "bump NODE_OPTIONS=--max-old-space-size=6144"}
		]
	}`
	cs, err := ParseCheatsheet(raw)
	if err != nil {
		t.Fatalf("ParseCheatsheet: %v", err)
	}
	if cs.Port != 7456 {
		t.Errorf("port: got %d want 7456", cs.Port)
	}
	if cs.MaxTurns != defaultCheatsheetMaxTurns {
		t.Errorf("MaxTurns default: got %d want %d", cs.MaxTurns, defaultCheatsheetMaxTurns)
	}
	if cs.TokenBudgetUSD != defaultCheatsheetTokenBudgetUSD {
		t.Errorf("TokenBudgetUSD default: got %v want %v",
			cs.TokenBudgetUSD, defaultCheatsheetTokenBudgetUSD)
	}
	if len(cs.RecommendedSteps) != 2 {
		t.Errorf("RecommendedSteps: got %d want 2", len(cs.RecommendedSteps))
	}
	if len(cs.KnownPitfalls) != 1 || cs.KnownPitfalls[0].Symptom != "OOM" {
		t.Errorf("KnownPitfalls: got %+v", cs.KnownPitfalls)
	}
}

func TestParseCheatsheet_Empty(t *testing.T) {
	if _, err := ParseCheatsheet(""); err == nil {
		t.Fatal("expected error for empty cheatsheet JSON")
	}
}

func TestParseCheatsheet_InvalidPort(t *testing.T) {
	if _, err := ParseCheatsheet(`{"port": 0}`); err == nil {
		t.Fatal("expected error for port=0")
	}
	if _, err := ParseCheatsheet(`{"port": 70000}`); err == nil {
		t.Fatal("expected error for port>65535")
	}
}

func TestParseCheatsheet_MalformedJSON(t *testing.T) {
	if _, err := ParseCheatsheet(`{not valid`); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestParseCheatsheet_PreservesExplicitBudgets ensures operator-set
// budgets aren't clobbered by the defaults.
func TestParseCheatsheet_PreservesExplicitBudgets(t *testing.T) {
	raw := `{"port": 8080, "max_turns": 12, "token_budget_usd": 1.25}`
	cs, err := ParseCheatsheet(raw)
	if err != nil {
		t.Fatalf("ParseCheatsheet: %v", err)
	}
	if cs.MaxTurns != 12 {
		t.Errorf("MaxTurns: got %d want 12", cs.MaxTurns)
	}
	if cs.TokenBudgetUSD != 1.25 {
		t.Errorf("TokenBudgetUSD: got %v want 1.25", cs.TokenBudgetUSD)
	}
}

// TestBuildPrompts_FullCheatsheet asserts every field flows into the
// system prompt verbatim. Order matters for substring matches on the
// final prompt — env iterates lex-sorted so the snapshot is stable.
func TestBuildPrompts_FullCheatsheet(t *testing.T) {
	cs := &Cheatsheet{
		RecommendedImage: "ghcr.io/example/foo:1.2.3",
		RecommendedSteps: []string{
			"apt-get update",
			"git clone https://example.com/foo",
			"pnpm install",
		},
		StartCommand: "node dist/cli.js",
		Port:         7456,
		Env: map[string]string{
			"OD_PORT":     "7456",
			"OD_DATA_DIR": "/var/lib/agent-ops/foo",
		},
		KnownPitfalls: []CheatsheetPitfall{
			{Symptom: "next build SIGABRT", Fix: "bump NODE_OPTIONS=--max-old-space-size=6144"},
			{Symptom: "isolated-vm fails", Fix: "use --ignore-scripts"},
		},
	}
	cs.applyDefaults()

	system, user := cs.BuildPrompts("open-design")

	// System prompt: every field must appear.
	for _, want := range []string{
		"open-design",
		"ghcr.io/example/foo:1.2.3",
		"Port to bind: 7456",
		"apt-get update",
		"pnpm install",
		"node dist/cli.js",
		"OD_PORT=7456",
		"OD_DATA_DIR=/var/lib/agent-ops/foo",
		"next build SIGABRT",
		"NODE_OPTIONS=--max-old-space-size=6144",
		"http://127.0.0.1:7456",
		"Tools: Bash, Read, Edit",
		"==== CHEATSHEET ====",
		"==== END CHEATSHEET ====",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q\n---\n%s", want, system)
		}
	}

	// Env keys must be lex-sorted (OD_DATA_DIR before OD_PORT).
	if i, j := strings.Index(system, "OD_DATA_DIR"), strings.Index(system, "OD_PORT"); i < 0 || j < 0 || i >= j {
		t.Errorf("env keys not lex-sorted: OD_DATA_DIR=%d OD_PORT=%d", i, j)
	}

	// User prompt: short imperative pointing at the port.
	if !strings.Contains(user, "open-design") || !strings.Contains(user, ":7456") {
		t.Errorf("user prompt unexpected: %q", user)
	}
}

// TestBuildPrompts_EmptyFields uses defaults / fallback strings when
// cheatsheet omits optional bits.
func TestBuildPrompts_EmptyFields(t *testing.T) {
	cs := &Cheatsheet{Port: 3000}
	cs.applyDefaults()
	system, _ := cs.BuildPrompts("")

	if !strings.Contains(system, "Port to bind: 3000") {
		t.Error("port missing")
	}
	if !strings.Contains(system, "none — build from source per steps below") {
		t.Error("image fallback missing")
	}
	if !strings.Contains(system, "(no preset steps — improvise)") {
		t.Error("steps fallback missing")
	}
	if !strings.Contains(system, "(none recorded)") {
		t.Error("pitfalls fallback missing")
	}
	if !strings.Contains(system, "(unspecified — derive from steps)") {
		t.Error("start_command fallback missing")
	}
}

// TestBuildPrompts_PromptAssemblyMatchesDocstring is a snapshot-ish test:
// the system prompt must contain the verbatim "STARTING POINT" guidance
// that the design doc commits to. If a future refactor splits the
// template across multiple files, this catches drift.
func TestBuildPrompts_GuidanceSentencePresent(t *testing.T) {
	cs := &Cheatsheet{Port: 8000}
	cs.applyDefaults()
	system, _ := cs.BuildPrompts("foo")
	for _, want := range []string{
		"STARTING POINT",
		"adapt freely when steps fail",
		"Self-healing is the whole point",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("guidance missing %q", want)
		}
	}
}

// TestCheatsheet_JSONRoundtrip — what the backend marshals must parse
// back into the same struct without info loss. Belt-and-braces against
// a future refactor changing field names on one side and not the other.
func TestCheatsheet_JSONRoundtrip(t *testing.T) {
	orig := Cheatsheet{
		RecommendedImage: "ghcr.io/x/y:1",
		RecommendedSteps: []string{"a", "b"},
		StartCommand:     "run me",
		Port:             1234,
		Env:              map[string]string{"K": "V"},
		KnownPitfalls:    []CheatsheetPitfall{{Symptom: "s", Fix: "f"}},
		MaxTurns:         10,
		TokenBudgetUSD:   0.25,
	}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := ParseCheatsheet(string(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.RecommendedImage != orig.RecommendedImage ||
		got.StartCommand != orig.StartCommand ||
		got.Port != orig.Port ||
		got.MaxTurns != orig.MaxTurns ||
		got.TokenBudgetUSD != orig.TokenBudgetUSD {
		t.Errorf("roundtrip mismatch: got %+v want %+v", got, orig)
	}
}

// TestIsCheatsheetMode covers the dispatch helper used by bootstrap.go.
func TestIsCheatsheetMode(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"cheatsheet", true},
		{"CHEATSHEET", true},
		{"  cheatsheet ", true},
		{"script", false},
		{"agent", false},
		{"", false},
	} {
		t.Setenv("AGENT_OPS_BOOTSTRAP_MODE", tc.val)
		if got := isCheatsheetMode(); got != tc.want {
			t.Errorf("AGENT_OPS_BOOTSTRAP_MODE=%q: got %v want %v", tc.val, got, tc.want)
		}
	}
}

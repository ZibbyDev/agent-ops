package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "plain JSON",
			in:   `{"pass": true, "evidence": "ok"}`,
			want: `{"pass": true, "evidence": "ok"}`,
			ok:   true,
		},
		{
			name: "leading prose",
			in:   "Sure! Here is the JSON:\n{\"pass\": false, \"fail_reason\": \"port closed\"}\n",
			want: `{"pass": false, "fail_reason": "port closed"}`,
			ok:   true,
		},
		{
			name: "nested objects",
			in:   `prose {"pass": true, "nested": {"k": "v"}, "ok": 1} trailing`,
			want: `{"pass": true, "nested": {"k": "v"}, "ok": 1}`,
			ok:   true,
		},
		{
			name: "braces inside strings ignored",
			in:   `{"evidence": "saw } and { in output", "pass": true}`,
			want: `{"evidence": "saw } and { in output", "pass": true}`,
			ok:   true,
		},
		{
			name: "no JSON",
			in:   `the agent just said yes, looks good`,
			ok:   false,
		},
		{
			name: "unbalanced",
			in:   `{"pass": true`,
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractJSONObject(c.in)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && got != c.want {
				t.Fatalf("got %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestParseVerifierJSON(t *testing.T) {
	t.Run("pass true with prose around it", func(t *testing.T) {
		r, err := parseVerifierJSON(`Sure: {"pass": true, "evidence": "ps shows pid 4231, curl 200"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Pass {
			t.Fatal("expected pass=true")
		}
		if !strings.Contains(r.Evidence, "pid 4231") {
			t.Fatalf("evidence = %q", r.Evidence)
		}
	})
	t.Run("pass false with fail_reason", func(t *testing.T) {
		r, err := parseVerifierJSON(`{"pass": false, "evidence": "no process", "fail_reason": "n8n binary missing"}`)
		if err != nil {
			t.Fatal(err)
		}
		if r.Pass {
			t.Fatal("expected pass=false")
		}
		if r.FailReason != "n8n binary missing" {
			t.Fatalf("fail_reason = %q", r.FailReason)
		}
	})
	t.Run("no JSON at all is an error", func(t *testing.T) {
		_, err := parseVerifierJSON("the agent forgot to emit JSON")
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("malformed JSON is an error", func(t *testing.T) {
		_, err := parseVerifierJSON(`{"pass": notbool}`)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestEnsureToken_PreferEnvWhenSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MY_TOKEN", "from-env")
	tok, err := EnsureToken(dir, "MY_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "from-env" {
		t.Fatalf("token = %q", tok)
	}
	// File must NOT be written when env was used (it would clash with a
	// later EnsureToken call that lacks the env var).
	if _, err := os.Stat(filepath.Join(dir, "mcp.token")); err == nil {
		t.Fatal("file should not be written when env wins")
	}
}

func TestEnsureToken_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	tok1, err := EnsureToken(dir, "UNSET_VAR")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok1, "ao_") {
		t.Fatalf("token missing prefix: %q", tok1)
	}
	if len(tok1) < 32 {
		t.Fatalf("token too short: %q", tok1)
	}
	// Second call must reuse the persisted file.
	tok2, err := EnsureToken(dir, "UNSET_VAR")
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != tok2 {
		t.Fatalf("token not stable across calls: %q vs %q", tok1, tok2)
	}
}

func TestEnsureToken_ReadsPersistedFile(t *testing.T) {
	dir := t.TempDir()
	prePersist := "ao_preplaced"
	if err := os.WriteFile(filepath.Join(dir, "mcp.token"), []byte(prePersist), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := EnsureToken(dir, "UNSET_VAR")
	if err != nil {
		t.Fatal(err)
	}
	if tok != prePersist {
		t.Fatalf("expected pre-placed token, got %q", tok)
	}
}

// Copyright 2026 Zibby Lab. Apache-2.0.

package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ZibbyDev/agent-ops/internal/config"
)

// baseConfig is the minimum valid config.yaml an integration tests
// starts with — matches what `agent-ops init` would produce.
const baseConfig = `state_dir: /tmp/ao
agent:
  provider: claude
  model: claude-sonnet-4-6
  api_key_env: ANTHROPIC_API_KEY
schedules:
  - name: hourly_health_check
    cron: "@hourly"
    prompt: check
    tools: [shell]
mcp:
  listen_addr: ":7842"
  token_env: AGENT_OPS_TOKEN
`

func setupTempConfig(t *testing.T) (cfgPath, envPath string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.yaml")
	envPath = filepath.Join(dir, "agent-ops.env")
	if err := os.WriteFile(cfgPath, []byte(baseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, envPath
}

func TestAdd_HappyPath_HTTP(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	res, err := Add(AddSpec{
		Name: "zibby", Transport: "http",
		URL: "https://api/mcp", AuthEnv: "ZBY", Token: "secret-token",
		ExtraEnv: map[string]string{"AGENT_OPS_NOTIFY_WORKFLOW_ID": "wf_x"},
	}, Options{ConfigPath: cfgPath, EnvPath: envPath})
	if err != nil {
		t.Fatal(err)
	}
	if !res.RestartRequired {
		t.Error("RestartRequired should be true")
	}
	// Config parses + has the new entry.
	body, _ := os.ReadFile(cfgPath)
	c := &config.Config{}
	if err := yaml.Unmarshal(body, c); err != nil {
		t.Fatalf("config no longer parses: %v\n--- body ---\n%s", err, body)
	}
	if len(c.MCPClients) != 1 || c.MCPClients[0].Name != "zibby" {
		t.Errorf("integration missing: %+v", c.MCPClients)
	}
	// Env file has the bearer + extra key, both present.
	env, _ := os.ReadFile(envPath)
	if !strings.Contains(string(env), "ZBY=secret-token") {
		t.Errorf("env missing bearer line: %q", env)
	}
	if !strings.Contains(string(env), "AGENT_OPS_NOTIFY_WORKFLOW_ID=wf_x") {
		t.Errorf("env missing extra line: %q", env)
	}
	// Mode 0600.
	st, _ := os.Stat(envPath)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("env file perms should be 0600; got %v", st.Mode())
	}
}

func TestAdd_RejectsDuplicateName(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	opts := Options{ConfigPath: cfgPath, EnvPath: envPath}
	if _, err := Add(AddSpec{Name: "zibby", Transport: "http", URL: "u"}, opts); err != nil {
		t.Fatal(err)
	}
	_, err := Add(AddSpec{Name: "zibby", Transport: "http", URL: "u"}, opts)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("expected ErrAlreadyExists; got %v", err)
	}
}

func TestAdd_RejectsBadTransport(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	_, err := Add(AddSpec{Name: "x", Transport: "smtp"},
		Options{ConfigPath: cfgPath, EnvPath: envPath})
	if err == nil {
		t.Fatal("expected transport-validation error")
	}
}

func TestAdd_RejectsMissingURL_HTTP(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	_, err := Add(AddSpec{Name: "x", Transport: "http"},
		Options{ConfigPath: cfgPath, EnvPath: envPath})
	if err == nil {
		t.Fatal("expected url-required error")
	}
}

func TestAdd_RejectsMissingCommand_Stdio(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	_, err := Add(AddSpec{Name: "x", Transport: "stdio"},
		Options{ConfigPath: cfgPath, EnvPath: envPath})
	if err == nil {
		t.Fatal("expected command-required error")
	}
}

func TestAdd_RejectsInjectionInAuthEnv(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	_, err := Add(AddSpec{
		Name: "x", Transport: "http", URL: "u",
		AuthEnv: "BAD\nINJECTED=1", Token: "t",
	}, Options{ConfigPath: cfgPath, EnvPath: envPath})
	if err == nil {
		t.Fatal("expected validEnvKey rejection")
	}
}

func TestRemove_DropsConfigAndEnv(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	opts := Options{ConfigPath: cfgPath, EnvPath: envPath}
	if _, err := Add(AddSpec{
		Name: "zibby", Transport: "http", URL: "u",
		AuthEnv: "ZBY", Token: "tok",
		ExtraEnv: map[string]string{"KEEP": "1"},
	}, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove("zibby", opts); err != nil {
		t.Fatal(err)
	}
	// Config integration gone.
	body, _ := os.ReadFile(cfgPath)
	c := &config.Config{}
	if err := yaml.Unmarshal(body, c); err != nil {
		t.Fatal(err)
	}
	if len(c.MCPClients) != 0 {
		t.Errorf("integration should be removed: %+v", c.MCPClients)
	}
	// AuthEnv removed from env file, but ExtraEnv key untouched (we don't
	// know it's not shared with another integration).
	env, _ := os.ReadFile(envPath)
	if strings.Contains(string(env), "ZBY=") {
		t.Errorf("AuthEnv line should be gone: %q", env)
	}
	if !strings.Contains(string(env), "KEEP=1") {
		t.Errorf("ExtraEnv KEEP should be preserved: %q", env)
	}
}

func TestRemove_NotFound(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	_, err := Remove("ghost", Options{ConfigPath: cfgPath, EnvPath: envPath})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound; got %v", err)
	}
}

func TestList_ReturnsSortedNoSecrets(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	opts := Options{ConfigPath: cfgPath, EnvPath: envPath}
	_, _ = Add(AddSpec{Name: "zulu", Transport: "http", URL: "u1", AuthEnv: "Z"}, opts)
	_, _ = Add(AddSpec{Name: "alpha", Transport: "http", URL: "u2", AuthEnv: "A"}, opts)
	items, err := List(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Name != "alpha" || items[1].Name != "zulu" {
		t.Errorf("not sorted by name: %+v", items)
	}
	// ListItem has AuthEnv (a name) but NO field for the literal token.
	for _, it := range items {
		if it.AuthEnv == "" {
			t.Errorf("AuthEnv should be surfaced for correlation: %+v", it)
		}
	}
}

// TestAdd_Concurrent_FlockSerializes asserts that two parallel Add()
// calls do NOT interleave: both succeed, and the resulting config has
// exactly two integrations (not one due to a lost write).
func TestAdd_Concurrent_FlockSerializes(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	opts := Options{ConfigPath: cfgPath, EnvPath: envPath, LockTimeout: 30 * time.Second}

	var wg sync.WaitGroup
	var errs atomic.Int32
	for i, name := range []string{"a", "b"} {
		i, name := i, name
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Add(AddSpec{
				Name: name, Transport: "http",
				URL: "u", AuthEnv: "T" + name, Token: "tok",
			}, opts); err != nil {
				t.Logf("worker %d add err: %v", i, err)
				errs.Add(1)
			}
		}()
	}
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("%d workers failed", errs.Load())
	}

	body, _ := os.ReadFile(cfgPath)
	c := &config.Config{}
	if err := yaml.Unmarshal(body, c); err != nil {
		t.Fatal(err)
	}
	if len(c.MCPClients) != 2 {
		t.Errorf("expected 2 integrations after concurrent adds, got %d (lost write): %+v",
			len(c.MCPClients), c.MCPClients)
	}
}

// TestMergeEnvFile_PreservesComments confirms unrelated comments + blank
// lines survive the env-file rewrite. An operator's hand-curated comments
// in agent-ops.env matter for the next person who reads the file.
func TestMergeEnvFile_PreservesComments(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	pre := "# top comment\nFOO=1\n\n# section\nBAR=2\n"
	if err := os.WriteFile(envPath, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeEnvFile(envPath, map[string]string{"BAR": "22", "BAZ": "3"}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(envPath)
	if !strings.Contains(string(got), "# top comment") {
		t.Errorf("top comment lost: %q", got)
	}
	if !strings.Contains(string(got), "# section") {
		t.Errorf("section comment lost: %q", got)
	}
	if !strings.Contains(string(got), "BAR=22") {
		t.Errorf("BAR not updated: %q", got)
	}
	if !strings.Contains(string(got), "BAZ=3") {
		t.Errorf("BAZ not appended: %q", got)
	}
}

// TestAdd_RollsBackConfigOnEnvFailure: when the env write step fails
// (here forced by pointing envPath at a directory) the config.yaml must
// be rolled back to its pre-Add bytes so the daemon never reads a
// config referencing a missing secret.
func TestAdd_RollsBackConfigOnEnvFailure(t *testing.T) {
	cfgPath, _ := setupTempConfig(t)
	// Direct env writes to a directory — os.Rename target-is-dir fails.
	envPath := t.TempDir() // <-- a directory, not a file
	pre, _ := os.ReadFile(cfgPath)

	_, err := Add(AddSpec{
		Name: "zibby", Transport: "http", URL: "u",
		AuthEnv: "ZBY", Token: "tok",
	}, Options{ConfigPath: cfgPath, EnvPath: envPath})
	if err == nil {
		t.Fatal("expected env write failure")
	}
	post, _ := os.ReadFile(cfgPath)
	if string(post) != string(pre) {
		t.Errorf("config not rolled back:\n--- pre ---\n%s\n--- post ---\n%s", pre, post)
	}
}

// TestAdd_NoOpEnvWhenTokenEmpty pins the documented "AuthEnv without
// Token means env is supplied externally" behavior — no env line is
// written, the config still picks up the integration.
func TestAdd_NoOpEnvWhenTokenEmpty(t *testing.T) {
	cfgPath, envPath := setupTempConfig(t)
	_, err := Add(AddSpec{
		Name: "x", Transport: "http", URL: "u", AuthEnv: "ZBY", // Token empty
	}, Options{ConfigPath: cfgPath, EnvPath: envPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(envPath); err == nil {
		body, _ := os.ReadFile(envPath)
		if strings.Contains(string(body), "ZBY=") {
			t.Errorf("env file should not have ZBY= when token was empty: %q", body)
		}
	}
}

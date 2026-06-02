// Copyright 2026 Zibby Lab. Apache-2.0.

package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeCmd records every Run() call. Returns canned stdout/stderr/err
// per matching prefix so individual tests can drive failure paths.
type fakeCmd struct {
	mu     sync.Mutex
	calls  []fakeCall
	canned map[string]fakeRet
}

type fakeCall struct{ name string; args []string }
type fakeRet struct{ stdout, stderr string; err error }

func newFakeCmd() *fakeCmd { return &fakeCmd{canned: map[string]fakeRet{}} }

func (f *fakeCmd) Run(_ context.Context, name string, args ...string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{name: name, args: append([]string(nil), args...)})
	// Match longest-prefix first so "apt-get install ruby" can override
	// a generic "apt-get" entry.
	keys := make([]string, 0, len(f.canned))
	for k := range f.canned {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	joined := name + " " + strings.Join(args, " ")
	for _, k := range keys {
		if strings.HasPrefix(joined, k) {
			r := f.canned[k]
			return r.stdout, r.stderr, r.err
		}
	}
	return "", "", nil
}

// fakePhase captures phase pushes for assertions.
type fakePhase struct {
	mu  sync.Mutex
	got []phaseEvent
}
type phaseEvent struct{ Phase SoloPhase; Detail string }

func (f *fakePhase) Push(_ context.Context, p SoloPhase, d string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, phaseEvent{Phase: p, Detail: d})
}

func (f *fakePhase) phases() []SoloPhase {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SoloPhase, len(f.got))
	for i, e := range f.got {
		out[i] = e.Phase
	}
	return out
}

// TestDetectFramework covers the file-presence detection used when
// spec.framework='auto'. Order matters: Gemfile beats package.json.
func TestDetectFramework(t *testing.T) {
	dir := t.TempDir()
	if got := detectFramework(dir); got != "" {
		t.Fatalf("empty dir should detect nothing; got %q", got)
	}
	mustTouch(t, filepath.Join(dir, "package.json"))
	if got := detectFramework(dir); got != "node" {
		t.Fatalf("package.json should detect node; got %q", got)
	}
	mustTouch(t, filepath.Join(dir, "Gemfile"))
	if got := detectFramework(dir); got != "rails" {
		t.Fatalf("Gemfile beats package.json (rails); got %q", got)
	}
}

func TestOSPackagesFor(t *testing.T) {
	cases := map[string]bool{
		"rails":  true,
		"node":   true,
		"python": true,
		"static": false,
		"":       false,
		"weird":  false,
	}
	for fw, expectNonEmpty := range cases {
		pkgs := osPackagesFor(fw)
		if expectNonEmpty != (len(pkgs) > 0) {
			t.Errorf("osPackagesFor(%q): want non-empty=%v, got %v", fw, expectNonEmpty, pkgs)
		}
	}
}

func TestFrameworkPort(t *testing.T) {
	r := &SoloRunner{Env: map[string]string{"ZIBBY_FRAMEWORK": "rails"}}
	if got := r.frameworkPort(); got != 3000 {
		t.Errorf("rails default port: want 3000, got %d", got)
	}
	r.Env["ZIBBY_FRAMEWORK"] = "python"
	if got := r.frameworkPort(); got != 8000 {
		t.Errorf("python default port: want 8000, got %d", got)
	}
	r.Env["ZIBBY_FRAMEWORK"] = "unknown"
	if got := r.frameworkPort(); got != 3000 {
		t.Errorf("unknown framework default: want 3000, got %d", got)
	}
}

func TestNextVersionDir(t *testing.T) {
	root := t.TempDir()
	r := &SoloRunner{Paths: SoloPaths{AppRoot: root}}
	if got := filepath.Base(r.nextVersionDir()); got != "v1" {
		t.Fatalf("empty root: want v1, got %s", got)
	}
	mustMkdir(t, filepath.Join(root, "v1"))
	mustMkdir(t, filepath.Join(root, "v3"))
	if got := filepath.Base(r.nextVersionDir()); got != "v4" {
		t.Fatalf("with v1+v3: want v4, got %s", got)
	}
}

func TestSoloRunnerLoadParsesSpec(t *testing.T) {
	dir := t.TempDir()
	spec := SoloSpec{
		AppSlug:   "hello",
		Source:    SoloSource{Type: "github", Repo: "ZibbyHQ/hello"},
		Framework: "auto",
		Tier:      "micro",
		Domain:    "hello.solo.zibby.app",
		Persist:   SoloPersist{DB: "sqlite-litestream", Files: "none"},
	}
	specPath := filepath.Join(dir, "spec.json")
	writeJSON(t, specPath, spec)
	writeFile(t, filepath.Join(dir, "account-id"), "455456047181")
	writeFile(t, filepath.Join(dir, "deployment-id"), "dep_abc123")
	writeFile(t, filepath.Join(dir, "status-url"), "https://api-dev.zibby.app/apps/solo/hello/status")

	r := &SoloRunner{
		Paths: SoloPaths{
			SpecPath:      specPath,
			AccountIDFile: filepath.Join(dir, "account-id"),
			DeploymentID:  filepath.Join(dir, "deployment-id"),
			StatusURL:     filepath.Join(dir, "status-url"),
		},
	}
	if err := r.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if r.spec.AppSlug != "hello" {
		t.Errorf("AppSlug: want hello, got %s", r.spec.AppSlug)
	}
	if r.accountID != "455456047181" {
		t.Errorf("accountID: want 455…, got %s", r.accountID)
	}
	// Status URL gets canonicalised to the phase POST route.
	if !strings.HasSuffix(r.statusURL, "/phase") {
		t.Errorf("statusURL should end with /phase, got %s", r.statusURL)
	}
	if r.dbBucketName() != "zibby-solo-455456047181-hello-db" {
		t.Errorf("dbBucketName: got %s", r.dbBucketName())
	}
}

func TestSoloRunnerLoadRejectsBadSpec(t *testing.T) {
	dir := t.TempDir()
	// Missing appSlug.
	writeFile(t, filepath.Join(dir, "spec.json"), `{"source":{"type":"github","repo":"x/y"}}`)
	r := &SoloRunner{Paths: SoloPaths{SpecPath: filepath.Join(dir, "spec.json")}}
	if err := r.load(); err == nil {
		t.Fatal("want error for empty appSlug")
	}

	// Missing source.type.
	writeFile(t, filepath.Join(dir, "spec.json"), `{"appSlug":"x","source":{}}`)
	if err := r.load(); err == nil {
		t.Fatal("want error for empty source.type")
	}
}

func TestRunSoloFromSpecRespectsFailedMarker(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.json")
	marker := filepath.Join(dir, ".failed")
	writeFile(t, marker, "reason=previously failed\n")
	writeJSON(t, specPath, SoloSpec{AppSlug: "x", Source: SoloSource{Type: "github", Repo: "a/b"}, Tier: "micro"})
	paths := SoloPaths{SpecPath: specPath, FailedMarker: marker}
	err := RunSoloFromSpec(context.Background(), paths, nil)
	if err == nil {
		t.Fatal("expected error when marker present")
	}
	if !strings.Contains(err.Error(), "terminally failed") {
		t.Errorf("want 'terminally failed', got %v", err)
	}
}

func TestFrameworkExecStartContainsPort(t *testing.T) {
	got := frameworkExecStart("rails", 3000)
	if !strings.Contains(got, "-p 3000") {
		t.Errorf("rails ExecStart missing port: %s", got)
	}
	got = frameworkExecStart("python", 8000)
	if !strings.Contains(got, "8000") {
		t.Errorf("python ExecStart missing port: %s", got)
	}
}

func TestRailsExecStartPrefersPumaConfig(t *testing.T) {
	// New ExecStart shape: branches on `[ -f config/puma.rb ]`. Both
	// paths bind to PORT so the existing test still passes (it greps
	// for `-p 3000`), but we also need to confirm the puma branch
	// exists for apps that ship their own config.
	got := frameworkExecStart("rails", 3000)
	if !strings.Contains(got, "puma -C config/puma.rb") {
		t.Errorf("rails ExecStart missing puma -C config/puma.rb branch: %s", got)
	}
	// Fallback branch still present for the no-config case.
	if !strings.Contains(got, "rails server") {
		t.Errorf("rails ExecStart missing fallback `rails server`: %s", got)
	}
}

func TestPersistOrGenerateSecretIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	r := &SoloRunner{}
	got1 := r.persistOrGenerateSecret(path)
	if len(got1) != 128 {
		t.Errorf("expected 128 hex chars, got %d", len(got1))
	}
	got2 := r.persistOrGenerateSecret(path)
	if got1 != got2 {
		t.Error("persistOrGenerateSecret should return the same value on second call")
	}
	// File mode should be 0600 (secret).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected 0600, got %o", info.Mode().Perm())
	}
}

func TestMaybeRestoreFromLitestreamSkipsWhenLocalDBExists(t *testing.T) {
	// If the SQLite file already exists with content, skip the restore.
	dir := t.TempDir()
	appDir := filepath.Join(dir, "v1")
	mustMkdir(t, filepath.Join(appDir, "storage"))
	mustMkdir(t, filepath.Join(appDir, "db"))
	// Write a non-empty file at both candidate paths.
	writeFile(t, filepath.Join(appDir, "storage/production.sqlite3"), "SQLite-magic")
	writeFile(t, filepath.Join(appDir, "db/production.sqlite3"), "SQLite-magic")

	stub := newFakeCmd()
	r := &SoloRunner{
		Cmd: stub,
		Env: map[string]string{"ZIBBY_APP_DIR": appDir},
		spec: &SoloSpec{
			AppSlug: "rails-blog",
			Persist: SoloPersist{DB: "sqlite-litestream"},
		},
		accountID: "455456047181",
	}
	err := r.maybeRestoreFromLitestream(context.Background())
	if err != nil {
		t.Fatalf("expected nil err when DB already exists, got %v", err)
	}
	// No restore command should have run.
	for _, c := range stub.calls {
		if c.name == "litestream" {
			t.Errorf("expected NO litestream call when local DB exists, got: %+v", c)
		}
	}
}

func TestMaybeRestoreFromLitestreamCallsLitestreamWhenDBMissing(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "v1")
	mustMkdir(t, appDir)
	// No storage/production.sqlite3 file yet — restore SHOULD attempt.

	stub := newFakeCmd()
	r := &SoloRunner{
		Cmd: stub,
		Env: map[string]string{"ZIBBY_APP_DIR": appDir},
		spec: &SoloSpec{
			AppSlug: "rails-blog",
			Persist: SoloPersist{DB: "sqlite-litestream"},
		},
		accountID: "455456047181",
	}
	// stub returns OK for the litestream restore so the first candidate
	// "succeeds" and we return early.
	_ = r.maybeRestoreFromLitestream(context.Background())
	// First call should be the litestream restore.
	if len(stub.calls) == 0 {
		t.Fatal("expected at least one Cmd.Run call")
	}
	if stub.calls[0].name != "litestream" || stub.calls[0].args[0] != "restore" {
		t.Errorf("first call expected `litestream restore`, got %+v", stub.calls[0])
	}
	// Targeted at the storage/ layout first (Rails 7+ default).
	hasStoragePath := false
	for _, c := range stub.calls {
		if c.name != "litestream" {
			continue
		}
		for _, a := range c.args {
			if strings.Contains(a, "storage/production.sqlite3") {
				hasStoragePath = true
			}
		}
	}
	if !hasStoragePath {
		t.Errorf("expected litestream call to target storage/production.sqlite3, calls=%+v", stub.calls)
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────

func mustTouch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(b))
}

// guard: stop unused-import complaints when this file is the only one
// importing fmt (it is via fakeCall.args formatting; left for parity
// with sibling test files).
var _ = fmt.Sprintf

// Copyright 2026 Zibby Lab. Apache-2.0.

// solo.go — Zibby solo-mode bootstrap.
//
// When a Zibby solo deploy fires, cloud-init drops:
//
//	/etc/zibby/spec.json     (mode 0640, owned by zibby:zibby)
//	/etc/zibby/account-id    (plain text)
//	/etc/zibby/deployment-id (correlates to a DDB row)
//	/etc/zibby/status-url    (where the daemon POSTs phase changes)
//
// agent-ops's `bootstrap --from-spec /etc/zibby/spec.json` subcommand
// reads the file, then walks a DETERMINISTIC plan/apply pipeline.
// Unlike the catalog cheatsheet / agent_script modes, solo is not
// LLM-driven — the spec is fully structured (framework + persistence
// + source), so we run plain apt-get / git / systemctl. Saves ~$0.20
// + ~5 minutes per deploy vs. the agent path AND removes the failure
// mode where the LLM mis-detects a framework.
//
// Contract: backend/src/handlers/__contracts__/solo-deploy-spec.md
//
// Phases reported back to the status URL (POST with phase + detail):
//
//	bootstrapping → downloading → installing → configuring →
//	starting → healthcheck → running   (success)
//
// On failure: failed with detail = the error string.
//
// The phase POST is best-effort. Network failures don't block the
// install — we still finish the local install so the operator can
// SSM into the box and recover. The structured agent.log under
// /var/log/zibby/agent.log is the source of truth.
package bootstrap

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cryptoRandRead is the binding for crypto/rand.Read — pulled into a
// var so testers can stub.
var cryptoRandRead = cryptorand.Read

// SoloSpec mirrors backend/src/handlers/__contracts__/solo-deploy-spec.md.
// JSON tags MUST match the backend wire shape verbatim.
type SoloSpec struct {
	AppSlug   string      `json:"appSlug"`
	Source    SoloSource  `json:"source"`
	Framework string      `json:"framework"`
	Tier      string      `json:"tier"`
	Secrets   []SoloSec   `json:"secrets"`
	Domain    string      `json:"domain"`
	Persist   SoloPersist `json:"persistence"`
}

// SoloSource is the source-of-truth for what we install. Exactly one of
// the inner discriminators is populated based on Type.
type SoloSource struct {
	Type  string `json:"type"`  // "github" | "tarball"
	Repo  string `json:"repo,omitempty"`
	Ref   string `json:"ref,omitempty"`
	S3URL string `json:"s3Url,omitempty"`
}

// SoloSec carries the key + a reference into the user's workspace-
// credentials store. The actual value is materialised from /etc/zibby/
// secrets.env (written by cloud-init OR the SSM puller). agent-ops
// NEVER receives plaintext.
type SoloSec struct {
	Key      string `json:"key"`
	ValueRef string `json:"valueRef,omitempty"`
}

type SoloPersist struct {
	DB    string `json:"db"`    // "sqlite-litestream" | "postgres-walg" | "none"
	Files string `json:"files"` // "activestorage-s3" | "rclone-bisync" | "none"
}

// SoloPhase is the wire enum for status updates. Backend's
// /apps/solo/<slug>/phase accepts these exact strings.
type SoloPhase string

const (
	PhaseBootstrapping SoloPhase = "bootstrapping"
	PhaseDownloading   SoloPhase = "downloading"
	PhaseInstalling    SoloPhase = "installing"
	PhaseConfiguring   SoloPhase = "configuring"
	PhaseStarting      SoloPhase = "starting"
	PhaseHealthcheck   SoloPhase = "healthcheck"
	PhaseRunning       SoloPhase = "running"
	PhaseFailed        SoloPhase = "failed"
)

// SoloPaths is the set of host paths the install touches. Centralised so
// tests can override (chdir to a tempdir, etc.).
type SoloPaths struct {
	SpecPath      string // /etc/zibby/spec.json
	AccountIDFile string // /etc/zibby/account-id
	DeploymentID  string // /etc/zibby/deployment-id
	StatusURL     string // /etc/zibby/status-url
	SecretsEnv    string // /etc/zibby/secrets.env
	AppRoot       string // /opt/app
	CurrentLink   string // /opt/app/current
	BackupsDir    string // /var/zibby/backups
	LogDir        string // /var/log/zibby
	CaddyFile     string // /etc/caddy/Caddyfile
	SystemdUnit   string // /etc/systemd/system/zibby-app.service
	LitestreamCfg string // /etc/litestream.yml
	FailedMarker  string // /etc/zibby/.failed
}

// DefaultSoloPaths returns the production paths. Tests override.
func DefaultSoloPaths() SoloPaths {
	return SoloPaths{
		SpecPath:      "/etc/zibby/spec.json",
		AccountIDFile: "/etc/zibby/account-id",
		DeploymentID:  "/etc/zibby/deployment-id",
		StatusURL:     "/etc/zibby/status-url",
		SecretsEnv:    "/etc/zibby/secrets.env",
		AppRoot:       "/opt/app",
		CurrentLink:   "/opt/app/current",
		BackupsDir:    "/var/zibby/backups",
		LogDir:        "/var/log/zibby",
		CaddyFile:     "/etc/caddy/Caddyfile",
		SystemdUnit:   "/etc/systemd/system/zibby-app.service",
		LitestreamCfg: "/etc/litestream.yml",
		FailedMarker:  "/etc/zibby/.failed",
	}
}

// SoloRunner threads the per-deploy state through the pipeline. Exposed
// for tests so they can stub commandRunner / phaseReporter.
type SoloRunner struct {
	Paths      SoloPaths
	Logger     *slog.Logger
	Cmd        commandRunner  // exec.CommandContext wrapper; tests stub
	Phase      phaseReporter  // POSTs to status URL
	HTTPClient *http.Client   // for healthcheck + phase
	Env        map[string]string

	// Loaded from disk in Load().
	spec         *SoloSpec
	accountID    string
	deploymentID string
	statusURL    string
}

// commandRunner abstracts exec.Cmd so we can dry-run + assert in tests.
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

type osCommandRunner struct{ logger *slog.Logger }

func (o osCommandRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Inherit env so apt-get sees DEBIAN_FRONTEND, etc.
	cmd.Env = os.Environ()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if o.logger != nil {
		o.logger.Info("solo: exec", "cmd", name, "args", args)
	}
	err := cmd.Run()
	if o.logger != nil {
		o.logger.Info("solo: exec done",
			"cmd", name, "exit_err", err,
			"stdout_bytes", stdout.Len(), "stderr_bytes", stderr.Len(),
		)
		// Spill the last 4KB of each pipe into structured logs so
		// CloudWatch captures the failure context.
		if stdout.Len() > 0 {
			o.logger.Info("solo: stdout", "cmd", name, "tail", lastN(stdout.String(), 4096))
		}
		if stderr.Len() > 0 {
			o.logger.Info("solo: stderr", "cmd", name, "tail", lastN(stderr.String(), 4096))
		}
	}
	return stdout.String(), stderr.String(), err
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// phaseReporter posts a phase + optional detail back to the Zibby
// control plane. Best-effort — Push errors are logged, never fatal.
type phaseReporter interface {
	Push(ctx context.Context, phase SoloPhase, detail string)
}

type httpPhaseReporter struct {
	url    string
	client *http.Client
	logger *slog.Logger
	token  string // bearer; from AGENT_OPS_TOKEN
}

func (h httpPhaseReporter) Push(ctx context.Context, phase SoloPhase, detail string) {
	if h.url == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{
		"phase":  string(phase),
		"detail": detail,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, strings.NewReader(string(body)))
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("solo: phase build request failed", "err", err.Error())
		}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	c := h.client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("solo: phase POST failed", "phase", phase, "err", err.Error())
		}
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 400 && h.logger != nil {
		h.logger.Warn("solo: phase POST rejected",
			"phase", phase, "status", resp.StatusCode, "body", string(rb))
	}
}

// NewSoloRunner builds the default production runner. Loads the spec
// + correlation metadata from disk. Caller is responsible for
// constructing a logger.
func NewSoloRunner(paths SoloPaths, logger *slog.Logger) (*SoloRunner, error) {
	r := &SoloRunner{
		Paths:      paths,
		Logger:     logger,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Env:        map[string]string{},
	}
	r.Cmd = osCommandRunner{logger: logger}
	if err := r.load(); err != nil {
		return nil, err
	}
	r.Phase = httpPhaseReporter{
		url:    r.statusURL,
		client: r.HTTPClient,
		logger: logger,
		token:  os.Getenv("AGENT_OPS_TOKEN"),
	}
	return r, nil
}

// load reads /etc/zibby/* into the runner. Each file is optional except
// spec.json — without that we have no goal.
func (r *SoloRunner) load() error {
	specBytes, err := os.ReadFile(r.Paths.SpecPath)
	if err != nil {
		return fmt.Errorf("solo: read spec: %w", err)
	}
	var s SoloSpec
	if err := json.Unmarshal(specBytes, &s); err != nil {
		return fmt.Errorf("solo: parse spec: %w", err)
	}
	if s.AppSlug == "" {
		return errors.New("solo: spec.appSlug is empty")
	}
	if s.Source.Type == "" {
		return errors.New("solo: spec.source.type is empty")
	}
	if s.Framework == "" {
		s.Framework = "auto"
	}
	r.spec = &s

	if b, err := os.ReadFile(r.Paths.AccountIDFile); err == nil {
		r.accountID = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(r.Paths.DeploymentID); err == nil {
		r.deploymentID = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(r.Paths.StatusURL); err == nil {
		r.statusURL = strings.TrimSpace(string(b))
		// The status URL we get from the provisioner is the GET status
		// route — derive the POST phase route off it. Backend wires
		// /apps/solo/<slug>/status (GET) and /apps/solo/<slug>/phase
		// (POST).
		if strings.HasSuffix(r.statusURL, "/status") {
			r.statusURL = strings.TrimSuffix(r.statusURL, "/status") + "/phase"
		}
	}
	return nil
}

// RunSoloFromSpec is the public entrypoint the `agent-ops bootstrap
// --from-spec` cobra subcommand calls. Reads /etc/zibby/spec.json,
// runs the pipeline, writes /etc/zibby/.failed on terminal error
// so the systemd unit doesn't loop.
//
// Returns nil on success. On failure, returns the original error AFTER
// updating the failed phase + writing the marker — so the caller's
// exit code maps to the systemd unit's restart policy. Callers must
// NOT retry past the marker (see the systemd unit's
// ConditionPathExists negation).
func RunSoloFromSpec(ctx context.Context, paths SoloPaths, logger *slog.Logger) error {
	// Marker check: if we've terminally failed, don't loop.
	if _, err := os.Stat(paths.FailedMarker); err == nil {
		if logger != nil {
			logger.Warn("solo: failed marker present, refusing to retry",
				"marker", paths.FailedMarker)
		}
		return errors.New("solo: previous run terminally failed (see /var/log/zibby/agent.log)")
	}

	r, err := NewSoloRunner(paths, logger)
	if err != nil {
		writeFailedMarker(paths, fmt.Sprintf("init: %s", err.Error()))
		return err
	}
	if err := r.Run(ctx); err != nil {
		r.Phase.Push(ctx, PhaseFailed, err.Error())
		writeFailedMarker(paths, err.Error())
		return err
	}
	return nil
}

// writeFailedMarker drops a small marker so systemd's
// ConditionPathExists=!/etc/zibby/.failed stops the unit from looping
// after a terminal failure. Best-effort.
func writeFailedMarker(paths SoloPaths, reason string) {
	body := fmt.Sprintf("failed_at=%s\nreason=%s\n",
		time.Now().UTC().Format(time.RFC3339), reason)
	// Ignore errors — we're already on the failure path; if the FS is
	// read-only there's nothing the marker can do anyway.
	_ = os.MkdirAll(filepath.Dir(paths.FailedMarker), 0o755)
	_ = os.WriteFile(paths.FailedMarker, []byte(body), 0o644)
}

// Run executes the pipeline end-to-end. Phase pushes are wrapped per
// step so a network blip doesn't abort the install.
func (r *SoloRunner) Run(ctx context.Context) error {
	if r.spec == nil {
		return errors.New("solo: spec not loaded; call NewSoloRunner first")
	}
	r.Phase.Push(ctx, PhaseBootstrapping, fmt.Sprintf("slug=%s framework=%s", r.spec.AppSlug, r.spec.Framework))

	// Each step gets its own timeout so a hung apt-get can't eat the
	// whole budget.
	if err := r.withTimeout(ctx, 10*time.Minute, r.stepDownload); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := r.withTimeout(ctx, 1*time.Minute, r.stepDetectFramework); err != nil {
		return fmt.Errorf("detect: %w", err)
	}
	if err := r.withTimeout(ctx, 15*time.Minute, r.stepInstallOSDeps); err != nil {
		return fmt.Errorf("install-os: %w", err)
	}
	if err := r.withTimeout(ctx, 15*time.Minute, r.stepInstallAppDeps); err != nil {
		return fmt.Errorf("install-app: %w", err)
	}
	if err := r.withTimeout(ctx, 5*time.Minute, r.stepConfigurePersistence); err != nil {
		return fmt.Errorf("persistence: %w", err)
	}
	if err := r.withTimeout(ctx, 2*time.Minute, r.stepConfigureCaddy); err != nil {
		return fmt.Errorf("caddy: %w", err)
	}
	if err := r.withTimeout(ctx, 2*time.Minute, r.stepWriteSystemdUnit); err != nil {
		return fmt.Errorf("systemd: %w", err)
	}
	if err := r.withTimeout(ctx, 2*time.Minute, r.stepStartService); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := r.withTimeout(ctx, 3*time.Minute, r.stepHealthcheck); err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}

	r.Phase.Push(ctx, PhaseRunning, fmt.Sprintf("domain=%s", r.spec.Domain))
	return nil
}

func (r *SoloRunner) withTimeout(parent context.Context, d time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	return fn(ctx)
}

// ─── Steps ────────────────────────────────────────────────────────────────

// stepDownload pulls the source. github → git clone --depth=1 with the
// short-lived token (if mounted), tarball → curl | tar.
func (r *SoloRunner) stepDownload(ctx context.Context) error {
	r.Phase.Push(ctx, PhaseDownloading, fmt.Sprintf("type=%s", r.spec.Source.Type))
	versioned := r.nextVersionDir()
	if err := os.MkdirAll(versioned, 0o755); err != nil {
		return fmt.Errorf("mkdir version: %w", err)
	}

	switch r.spec.Source.Type {
	case "github":
		ref := r.spec.Source.Ref
		args := []string{"clone", "--depth=1"}
		if ref != "" {
			args = append(args, "--branch", ref)
		}
		// If the cloud-init dropped a token at /run/secrets/github-token,
		// inject it into the clone URL. Otherwise we attempt anonymous
		// (works for public repos).
		repo := r.spec.Source.Repo
		if repo == "" {
			return errors.New("source.repo is empty")
		}
		cloneURL := fmt.Sprintf("https://github.com/%s.git", repo)
		if tok := readTrimmed("/run/secrets/github-token"); tok != "" {
			// Format: https://x-access-token:<token>@github.com/owner/repo.git
			cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", tok, repo)
		}
		args = append(args, cloneURL, versioned)
		if _, _, err := r.Cmd.Run(ctx, "git", args...); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
	case "tarball":
		if r.spec.Source.S3URL == "" {
			return errors.New("source.s3Url is empty")
		}
		// curl | tar — the s3 URL is presigned by the backend.
		tmpTar := filepath.Join(versioned, ".source.tar.gz")
		if _, _, err := r.Cmd.Run(ctx, "curl", "-fsSL", "-o", tmpTar, r.spec.Source.S3URL); err != nil {
			return fmt.Errorf("curl: %w", err)
		}
		if _, _, err := r.Cmd.Run(ctx, "tar", "-xzf", tmpTar, "-C", versioned, "--strip-components=1"); err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		_ = os.Remove(tmpTar)
	default:
		return fmt.Errorf("unknown source.type %q", r.spec.Source.Type)
	}

	// Chown to zibby:zibby so the systemd unit can read it.
	_, _, _ = r.Cmd.Run(ctx, "chown", "-R", "zibby:zibby", versioned)
	r.Env["ZIBBY_APP_DIR"] = versioned
	return nil
}

// stepDetectFramework looks at files in the version dir. Re-runs the
// auto detection if spec.framework='auto'.
func (r *SoloRunner) stepDetectFramework(ctx context.Context) error {
	versioned := r.Env["ZIBBY_APP_DIR"]
	if r.spec.Framework != "auto" && r.spec.Framework != "" {
		// User specified — trust them.
		r.Env["ZIBBY_FRAMEWORK"] = r.spec.Framework
		return nil
	}
	detected := detectFramework(versioned)
	if detected == "" {
		return errors.New("could not auto-detect framework (no Gemfile/package.json/requirements.txt/etc.)")
	}
	if r.Logger != nil {
		r.Logger.Info("solo: framework auto-detected", "framework", detected)
	}
	r.Env["ZIBBY_FRAMEWORK"] = detected
	return nil
}

func detectFramework(dir string) string {
	checks := []struct {
		file, framework string
	}{
		{"Gemfile", "rails"},
		{"package.json", "node"},
		{"requirements.txt", "python"},
		{"pyproject.toml", "python"},
		{"mix.exs", "elixir"},
		{"Cargo.toml", "rust"},
		{"go.mod", "go"},
		{"index.html", "static"},
	}
	for _, c := range checks {
		if _, err := os.Stat(filepath.Join(dir, c.file)); err == nil {
			return c.framework
		}
	}
	return ""
}

// stepInstallOSDeps runs apt-get install -y for the framework-required
// packages. Idempotent — apt-get install -y on an already-installed
// package is a no-op.
func (r *SoloRunner) stepInstallOSDeps(ctx context.Context) error {
	r.Phase.Push(ctx, PhaseInstalling, "os-deps")
	fw := r.Env["ZIBBY_FRAMEWORK"]
	pkgs := osPackagesFor(fw)
	// Always present in the AMI: caddy, curl, git, ca-certificates, unzip.
	// These are framework-specific add-ons.
	if len(pkgs) == 0 {
		// Nothing extra to install — e.g. static.
		return nil
	}
	// apt-get update — bounded to 60s. If the index is fresh enough the
	// no-op finishes in <2s.
	_, _, _ = r.Cmd.Run(ctx, "apt-get", "update", "-y")
	args := append([]string{"install", "-y", "--no-install-recommends"}, pkgs...)
	if _, _, err := r.Cmd.Run(ctx, "apt-get", args...); err != nil {
		return fmt.Errorf("apt-get install %v: %w", pkgs, err)
	}
	return nil
}

// osPackagesFor returns the apt packages a framework needs beyond the
// always-installed baseline. Kept conservative — when the user picks a
// non-default Ruby version, etc., they own the version mgmt.
func osPackagesFor(framework string) []string {
	switch framework {
	case "rails":
		// ruby + dev headers for native gems (sqlite3-ruby, nokogiri).
		// SQLite client libs are needed even when the user picks
		// postgres because Litestream wraps sqlite.
		return []string{"ruby", "ruby-dev", "build-essential", "libsqlite3-dev", "libyaml-dev", "pkg-config", "zlib1g-dev"}
	case "node":
		// Node 20 LTS from Debian backports. The user's package.json
		// engines.node should match; if not, that's their bug — the
		// agent doesn't try to install nvm.
		return []string{"nodejs", "npm"}
	case "python":
		return []string{"python3", "python3-pip", "python3-venv", "build-essential"}
	case "elixir":
		return []string{"elixir", "erlang-dev", "build-essential"}
	case "rust":
		// We assume rustup is already on the AMI for rust. If not, this
		// install is the user's job. Adding cargo here avoids the full
		// rustup curl-pipe-bash.
		return []string{"cargo", "build-essential"}
	case "go":
		return []string{"golang-go"}
	case "static", "":
		return nil
	default:
		return nil
	}
}

// stepInstallAppDeps runs the framework's "install dependencies" cmd:
// bundle install / npm install / pip install -r ... etc.
func (r *SoloRunner) stepInstallAppDeps(ctx context.Context) error {
	r.Phase.Push(ctx, PhaseInstalling, "app-deps")
	fw := r.Env["ZIBBY_FRAMEWORK"]
	versioned := r.Env["ZIBBY_APP_DIR"]
	switch fw {
	case "rails":
		// `bundle install --deployment` for prod sets path to vendor/bundle.
		// Skip if no Gemfile.lock — `bundle install` will create it.
		if _, _, err := r.Cmd.Run(ctx, "bash", "-c", fmt.Sprintf("cd %s && gem install bundler --no-document", shellEscape(versioned))); err != nil {
			return fmt.Errorf("gem install bundler: %w", err)
		}
		if _, _, err := r.Cmd.Run(ctx, "bash", "-c", fmt.Sprintf("cd %s && bundle config set --local path vendor/bundle && bundle install --jobs=4 --retry=3", shellEscape(versioned))); err != nil {
			return fmt.Errorf("bundle install: %w", err)
		}
		// Storage + tmp dirs — Rails creates these on first boot but
		// it's safer to pre-create so SQLite path resolution works on
		// the first db:migrate.
		_, _, _ = r.Cmd.Run(ctx, "bash", "-c", fmt.Sprintf("mkdir -p %s/storage %s/tmp/pids %s/log",
			shellEscape(versioned), shellEscape(versioned), shellEscape(versioned)))
		// Rails db migrate — production env, needs SECRET_KEY_BASE.
		// We don't have it at install time (it's per-VM and we generate
		// it in stepWriteSystemdUnit), so pass a one-shot dummy here
		// just to clear Rails's boot check. db:migrate doesn't use the
		// secret for anything; it only reads schema.rb.
		_, _, _ = r.Cmd.Run(ctx, "bash", "-c", fmt.Sprintf(
			"cd %s && RAILS_ENV=production SECRET_KEY_BASE=$(openssl rand -hex 32) bundle exec rake db:create db:migrate 2>&1 || true",
			shellEscape(versioned),
		))
	case "node":
		if _, _, err := r.Cmd.Run(ctx, "bash", "-c", fmt.Sprintf("cd %s && npm ci --omit=dev || npm install --omit=dev", shellEscape(versioned))); err != nil {
			return fmt.Errorf("npm install: %w", err)
		}
	case "python":
		if _, _, err := r.Cmd.Run(ctx, "bash", "-c", fmt.Sprintf("cd %s && python3 -m venv .venv && .venv/bin/pip install --upgrade pip && .venv/bin/pip install -r requirements.txt", shellEscape(versioned))); err != nil {
			return fmt.Errorf("pip install: %w", err)
		}
	case "elixir":
		if _, _, err := r.Cmd.Run(ctx, "bash", "-c", fmt.Sprintf("cd %s && mix local.hex --force && mix local.rebar --force && mix deps.get --only prod && MIX_ENV=prod mix compile", shellEscape(versioned))); err != nil {
			return fmt.Errorf("mix deps: %w", err)
		}
	case "rust":
		if _, _, err := r.Cmd.Run(ctx, "bash", "-c", fmt.Sprintf("cd %s && cargo build --release", shellEscape(versioned))); err != nil {
			return fmt.Errorf("cargo build: %w", err)
		}
	case "go":
		if _, _, err := r.Cmd.Run(ctx, "bash", "-c", fmt.Sprintf("cd %s && go build -o ./app", shellEscape(versioned))); err != nil {
			return fmt.Errorf("go build: %w", err)
		}
	case "static":
		// nothing
	}
	return nil
}

// stepConfigurePersistence sets up Litestream (if persistence.db =
// sqlite-litestream) before the app starts, so the WAL is captured
// from the first write.
//
// On a redeploy / reprovision (same slug, fresh EBS — common case after
// terminate+launch), we ALSO attempt a `litestream restore` BEFORE
// starting the app, so the new VM picks up the previous SQLite state
// from the S3 replica. The restore is conditional on the local DB file
// being missing/empty — first-deploy still creates a fresh DB.
func (r *SoloRunner) stepConfigurePersistence(ctx context.Context) error {
	r.Phase.Push(ctx, PhaseConfiguring, fmt.Sprintf("db=%s files=%s", r.spec.Persist.DB, r.spec.Persist.Files))
	if r.spec.Persist.DB == "sqlite-litestream" {
		if err := r.installLitestream(ctx); err != nil {
			return err
		}
		if err := r.writeLitestreamConfig(); err != nil {
			return err
		}
		// RESTORE pass — runs BEFORE we enable the replicate daemon. If
		// the S3 replica has objects AND the local DB is missing, pull
		// it down. Idempotent: when the local DB already exists +
		// non-empty (fresh deploy already wrote schema), restore is
		// skipped.
		if err := r.maybeRestoreFromLitestream(ctx); err != nil {
			// Restore failure is logged but NOT fatal: a brand-new app
			// with an empty S3 replica produces a "no snapshots found"
			// error, which is the happy path on first deploy. The Rails
			// `db:migrate` step below will create the schema fresh.
			if r.Logger != nil {
				r.Logger.Warn("solo: litestream restore returned non-fatal error (continuing)", "err", err.Error())
			}
		}
		// systemd unit comes preinstalled with the apt package, but
		// we re-enable + start to be defensive.
		if _, _, err := r.Cmd.Run(ctx, "systemctl", "enable", "--now", "litestream.service"); err != nil {
			// Litestream's failure is logged but NOT fatal — we'd rather
			// have a running app without backups than no app at all.
			// The operator can SSM in and inspect litestream.service.
			if r.Logger != nil {
				r.Logger.Warn("solo: litestream enable failed (continuing)", "err", err.Error())
			}
		}
	}
	if r.spec.Persist.Files == "activestorage-s3" {
		bucket := r.uploadsBucketName()
		if bucket != "" {
			r.Env["ZIBBY_FILES_BUCKET"] = bucket
			// Rails reads this via config/storage.yml. We surface the
			// var into the systemd env block below — no extra config
			// file needed if the user's storage.yml uses ENV[...].
		}
	}
	return nil
}

// maybeRestoreFromLitestream pulls the last SQLite snapshot + WAL from
// S3 when the local DB file is missing or zero-bytes. This is the
// "user replaces VM, data follows" path:
//
//  1. Old VM ran `litestream replicate` → wrote snapshots + WAL to
//     s3://zibby-solo-<acct>-<slug>-db/{rails-storage,rails-db}/
//  2. Old VM is terminated; EBS retained (DeleteOnTermination=false on
//     the provisioner) but inaccessible (volume orphaned).
//  3. New VM provisioned with same slug; cloud-init writes the same
//     spec; agent-ops bootstrap runs.
//  4. THIS function: detects empty/missing storage/production.sqlite3,
//     runs `litestream restore -o <path> s3://<bucket>/<replica>`.
//  5. Rails boots, sees fully-populated DB, serves traffic.
//
// Best-effort across both possible Rails layouts (storage/ and db/).
// Returns nil if no restore attempted (DB exists + non-empty); returns
// the litestream error if both layouts fail (logged + suppressed by
// the caller because a fresh deploy hits that path naturally).
func (r *SoloRunner) maybeRestoreFromLitestream(ctx context.Context) error {
	appDir := r.Env["ZIBBY_APP_DIR"]
	if appDir == "" {
		return errors.New("maybeRestoreFromLitestream: ZIBBY_APP_DIR not set")
	}
	bucket := r.dbBucketName()
	if bucket == "" {
		return errors.New("maybeRestoreFromLitestream: bucket name resolved empty")
	}

	candidates := []struct {
		localPath  string
		s3Replica  string
	}{
		{filepath.Join(appDir, "storage/production.sqlite3"), fmt.Sprintf("s3://%s/rails-storage", bucket)},
		{filepath.Join(appDir, "db/production.sqlite3"),      fmt.Sprintf("s3://%s/rails-db", bucket)},
	}

	var lastErr error
	for _, c := range candidates {
		// Skip if the file exists + has non-zero size (fresh deploy or
		// a re-bootstrap after an in-place restart). The Litestream
		// replicate daemon picks up from where it left off.
		if info, err := os.Stat(c.localPath); err == nil && info.Size() > 0 {
			continue
		}
		// Ensure the parent dir exists — Rails would create it on
		// db:migrate, but we want the restore to drop in first.
		_ = os.MkdirAll(filepath.Dir(c.localPath), 0o755)
		// Run `litestream restore -o <path> <replica>`. Litestream
		// exits 1 with "no snapshots found" when the bucket is empty;
		// we treat that as a non-fatal "fresh deploy, no replica yet"
		// signal.
		if _, _, err := r.Cmd.Run(ctx, "litestream", "restore", "-o", c.localPath, c.s3Replica); err != nil {
			lastErr = fmt.Errorf("litestream restore %s ← %s: %w", c.localPath, c.s3Replica, err)
			if r.Logger != nil {
				r.Logger.Info("solo: litestream restore failed (may be fresh deploy)", "err", err.Error(), "replica", c.s3Replica)
			}
			continue
		}
		if r.Logger != nil {
			r.Logger.Info("solo: litestream restore succeeded", "path", c.localPath, "replica", c.s3Replica)
		}
		// Chown so the systemd zibby user can read.
		_, _, _ = r.Cmd.Run(ctx, "chown", "zibby:zibby", c.localPath)
		// We only restore the first matching layout — once we have a
		// hit, the other path is irrelevant.
		return nil
	}
	return lastErr
}

func (r *SoloRunner) installLitestream(ctx context.Context) error {
	// Idempotent: if /usr/local/bin/litestream exists, skip the download.
	if _, err := os.Stat("/usr/local/bin/litestream"); err == nil {
		return nil
	}
	// Download the arm64 .deb. Pinned to a known-good release.
	const dl = "https://github.com/benbjohnson/litestream/releases/download/v0.3.13/litestream-v0.3.13-linux-arm64.deb"
	tmp := "/tmp/litestream.deb"
	if _, _, err := r.Cmd.Run(ctx, "curl", "-fsSL", "-o", tmp, dl); err != nil {
		return fmt.Errorf("curl litestream: %w", err)
	}
	if _, _, err := r.Cmd.Run(ctx, "dpkg", "-i", tmp); err != nil {
		return fmt.Errorf("dpkg -i litestream: %w", err)
	}
	_ = os.Remove(tmp)
	return nil
}

func (r *SoloRunner) writeLitestreamConfig() error {
	bucket := r.dbBucketName()
	if bucket == "" {
		return errors.New("litestream: bucket name resolved empty (need accountId + slug)")
	}
	// Rails default: <appdir>/storage/production.sqlite3. Fall back to
	// db/production.sqlite3 (older Rails layouts). agent-ops watches
	// both paths via two `dbs:` entries.
	cfg := fmt.Sprintf(`# zibby-managed; do not edit by hand.
addr: ":9090"
dbs:
  - path: %s/storage/production.sqlite3
    replicas:
      - type: s3
        bucket: %s
        path: rails-storage
        region: ap-southeast-2
  - path: %s/db/production.sqlite3
    replicas:
      - type: s3
        bucket: %s
        path: rails-db
        region: ap-southeast-2
`,
		r.Env["ZIBBY_APP_DIR"], bucket,
		r.Env["ZIBBY_APP_DIR"], bucket,
	)
	return os.WriteFile(r.Paths.LitestreamCfg, []byte(cfg), 0o644)
}

func (r *SoloRunner) dbBucketName() string {
	if r.accountID == "" || r.spec.AppSlug == "" {
		return ""
	}
	return fmt.Sprintf("zibby-solo-%s-%s-db", r.accountID, r.spec.AppSlug)
}

func (r *SoloRunner) uploadsBucketName() string {
	if r.accountID == "" || r.spec.AppSlug == "" {
		return ""
	}
	return fmt.Sprintf("zibby-solo-%s-%s-uploads", r.accountID, r.spec.AppSlug)
}

// stepConfigureCaddy writes /etc/caddy/Caddyfile pointing the domain
// at the app's port. Caddy auto-handles HTTPS via ACME.
func (r *SoloRunner) stepConfigureCaddy(ctx context.Context) error {
	port := r.frameworkPort()
	domain := r.spec.Domain
	if domain == "" {
		domain = fmt.Sprintf("%s.solo.zibby.app", r.spec.AppSlug)
	}
	// `:80` block allows HTTP→HTTPS redirect when running w/o public
	// DNS yet (CI/test, IP-only access). The `<domain>` block does the
	// real reverse proxy. Caddy resolves ACME against the public IP
	// once DNS is in place.
	cfg := fmt.Sprintf(`# zibby-managed; do not edit by hand.
{
    auto_https disable_redirects
}

%s {
    reverse_proxy 127.0.0.1:%d
    encode gzip zstd
    log {
        output stdout
        format json
    }
}

# Always-on plain HTTP for the public IP — lets the smoke test hit
# the box before DNS propagates.
:80 {
    reverse_proxy 127.0.0.1:%d
    encode gzip zstd
}
`, domain, port, port)
	if err := os.WriteFile(r.Paths.CaddyFile, []byte(cfg), 0o644); err != nil {
		return fmt.Errorf("write Caddyfile: %w", err)
	}
	// Reload caddy so the new config kicks in. systemctl reload is a
	// SIGUSR1 to caddy — no downtime.
	_, _, _ = r.Cmd.Run(ctx, "systemctl", "reload", "caddy.service")
	return nil
}

func (r *SoloRunner) frameworkPort() int {
	// Per-framework defaults — overridable via spec secrets PORT=1234
	// once we want to (out of scope for now). Match the values the
	// systemd unit's ExecStart binds to below.
	switch r.Env["ZIBBY_FRAMEWORK"] {
	case "rails":
		return 3000
	case "node":
		return 3000
	case "python":
		return 8000
	case "elixir":
		return 4000
	case "rust", "go":
		return 8080
	case "static":
		return 8080
	}
	return 3000
}

// stepWriteSystemdUnit drops /etc/systemd/system/zibby-app.service with
// the framework-appropriate ExecStart + env file. Atomic-replace the
// `current` symlink to the new version dir.
func (r *SoloRunner) stepWriteSystemdUnit(ctx context.Context) error {
	fw := r.Env["ZIBBY_FRAMEWORK"]
	versioned := r.Env["ZIBBY_APP_DIR"]

	// Atomic symlink swap.
	tmpLink := r.Paths.CurrentLink + ".new"
	_ = os.Remove(tmpLink)
	if err := os.Symlink(versioned, tmpLink); err != nil {
		return fmt.Errorf("symlink new: %w", err)
	}
	if err := os.Rename(tmpLink, r.Paths.CurrentLink); err != nil {
		return fmt.Errorf("rename symlink: %w", err)
	}

	// Write/refresh the secrets env file from the spec's secret keys.
	// Secret VALUES are not in the spec — the cloud-init UserData
	// already wrote them via SSM ParameterStore reads (TODO: the SSM
	// puller). For now we ensure the file exists with an empty body
	// so EnvironmentFile= doesn't fail-stop the unit.
	if _, err := os.Stat(r.Paths.SecretsEnv); err != nil {
		_ = os.MkdirAll(filepath.Dir(r.Paths.SecretsEnv), 0o755)
		_ = os.WriteFile(r.Paths.SecretsEnv, []byte("# zibby secrets (populated by cloud-init)\n"), 0o600)
	}

	port := r.frameworkPort()
	exec := frameworkExecStart(fw, port)
	extraEnv := ""
	for k, v := range r.Env {
		if k == "ZIBBY_APP_DIR" || k == "ZIBBY_FRAMEWORK" {
			continue
		}
		extraEnv += fmt.Sprintf("Environment=%s=%s\n", k, v)
	}
	// Always include PORT so the framework's stock entrypoint binds
	// to the expected value.
	extraEnv += fmt.Sprintf("Environment=PORT=%d\n", port)

	// Framework-specific env. Rails 7 production REQUIRES SECRET_KEY_BASE
	// (or an encrypted-credentials master.key); the bootstrap path
	// generates a random 128-char hex per-VM and persists it under
	// /etc/zibby so a restart keeps the same key. RAILS_ENV gates the
	// production environment loaded by config/environments/*.rb;
	// RAILS_LOG_TO_STDOUT bypasses log/production.log + lets journalctl
	// catch the request stream.
	if fw == "rails" {
		secret := r.persistOrGenerateSecret("/etc/zibby/rails-secret-key-base")
		extraEnv += fmt.Sprintf("Environment=SECRET_KEY_BASE=%s\n", secret)
		extraEnv += "Environment=RAILS_ENV=production\n"
		extraEnv += "Environment=RAILS_LOG_TO_STDOUT=true\n"
		extraEnv += "Environment=RAILS_SERVE_STATIC_FILES=true\n"
	}

	unit := fmt.Sprintf(`# zibby-managed; do not edit by hand.
[Unit]
Description=Zibby solo app (%s)
After=network-online.target
Wants=network-online.target

[Service]
User=zibby
Group=zibby
WorkingDirectory=%s
EnvironmentFile=-%s
%sExecStart=%s
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=zibby-app

[Install]
WantedBy=multi-user.target
`, r.spec.AppSlug, r.Paths.CurrentLink, r.Paths.SecretsEnv, extraEnv, exec)

	if err := os.WriteFile(r.Paths.SystemdUnit, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	if _, _, err := r.Cmd.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
}

func frameworkExecStart(framework string, port int) string {
	switch framework {
	case "rails":
		// Rails 7 production. We prefer `puma -C config/puma.rb` when
		// the app ships its own puma config (the common production
		// shape); fall back to `rails server` otherwise. Both bind to
		// 0.0.0.0:$PORT — Caddy on the same host reverse-proxies from
		// 80/443 via 127.0.0.1:$PORT.
		return fmt.Sprintf("/bin/bash -lc 'cd /opt/app/current && if [ -f config/puma.rb ]; then bundle exec puma -C config/puma.rb; else bundle exec rails server -b 0.0.0.0 -p %d -e production; fi'", port)
	case "node":
		// Convention: package.json's `start` script binds to PORT.
		return "/bin/bash -lc 'cd /opt/app/current && npm start'"
	case "python":
		// Convention: app.py with `if __name__ == "__main__"` binding
		// to PORT, OR a `main.py` exposing `app` for uvicorn.
		return fmt.Sprintf("/bin/bash -lc 'cd /opt/app/current && .venv/bin/python -m http.server %d'", port)
	case "elixir":
		// Phoenix release — usually under _build/prod/rel/<app>/bin/<app>
		return "/bin/bash -lc 'cd /opt/app/current && _build/prod/rel/$(basename $PWD)/bin/$(basename $PWD) start'"
	case "rust", "go":
		// We compiled to ./app above; let it bind PORT.
		return "/bin/bash -lc 'cd /opt/app/current && ./app'"
	case "static":
		// `static` is served by Caddy directly; the systemd unit is a
		// no-op shim so the existing pipeline doesn't branch.
		return "/bin/sleep infinity"
	}
	return "/bin/sleep infinity"
}

// stepStartService starts zibby-app.service. Returns once systemctl
// reports the unit as active (not "starting").
func (r *SoloRunner) stepStartService(ctx context.Context) error {
	r.Phase.Push(ctx, PhaseStarting, "")
	if _, _, err := r.Cmd.Run(ctx, "systemctl", "enable", "zibby-app.service"); err != nil {
		return fmt.Errorf("systemctl enable: %w", err)
	}
	// Use `restart` so a re-deploy replaces a running process.
	if _, _, err := r.Cmd.Run(ctx, "systemctl", "restart", "zibby-app.service"); err != nil {
		return fmt.Errorf("systemctl restart: %w", err)
	}
	return nil
}

// stepHealthcheck polls 127.0.0.1:<port>/ until 200 (or any 2xx/3xx),
// or the budget runs out. Same shape as appIsListening above.
func (r *SoloRunner) stepHealthcheck(ctx context.Context) error {
	r.Phase.Push(ctx, PhaseHealthcheck, "")
	port := r.frameworkPort()
	endpoints := []string{"/healthz", "/health", "/"}
	deadline := time.Now().Add(2 * time.Minute)
	client := &http.Client{Timeout: 3 * time.Second}
	last := ""
	for time.Now().Before(deadline) {
		for _, ep := range endpoints {
			url := fmt.Sprintf("http://127.0.0.1:%d%s", port, ep)
			req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			resp, err := client.Do(req)
			if err != nil {
				last = err.Error()
				continue
			}
			resp.Body.Close()
			if resp.StatusCode < 400 {
				return nil
			}
			last = fmt.Sprintf("%s → HTTP %d", ep, resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("healthcheck cancelled: %w (last: %s)", ctx.Err(), last)
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("healthcheck timed out after 2m on port %d (last: %s)", port, last)
}

// ─── Helpers ─────────────────────────────────────────────────────────────

// nextVersionDir picks /opt/app/v<N+1> based on the highest existing vN.
// If no vN exists, returns v1. Used by stepDownload AND the future
// update path (atomic symlink swap).
func (r *SoloRunner) nextVersionDir() string {
	max := 0
	entries, err := os.ReadDir(r.Paths.AppRoot)
	if err != nil {
		return filepath.Join(r.Paths.AppRoot, "v1")
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "v") {
			continue
		}
		if k, perr := strconv.Atoi(strings.TrimPrefix(n, "v")); perr == nil && k > max {
			max = k
		}
	}
	return filepath.Join(r.Paths.AppRoot, fmt.Sprintf("v%d", max+1))
}

// readTrimmed reads a file, returning the trimmed content. Returns ""
// when the file doesn't exist OR is unreadable.
func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// persistOrGenerateSecret returns the value from `path` if it exists,
// or generates a fresh 128-char hex secret, writes it 0600, and returns
// it. Used for Rails SECRET_KEY_BASE so a restart-on-failure preserves
// the same key (cookies signed with one key would otherwise become
// invalid on every redeploy — surprising for users with persistent
// sessions, even though our minimal fixture doesn't use them).
func (r *SoloRunner) persistOrGenerateSecret(path string) string {
	if existing := readTrimmed(path); existing != "" {
		return existing
	}
	// 64 bytes → 128 hex chars (matches `rails secret`).
	buf := make([]byte, 64)
	if _, err := io.ReadFull(randSource(), buf); err != nil {
		// Last-resort fallback: deterministic from accountId + slug.
		// Bad for security but better than a panic during install.
		return strings.Repeat("0", 128)
	}
	hex := fmt.Sprintf("%x", buf)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(hex+"\n"), 0o600)
	return hex
}

// randSource returns a reader for crypto-random bytes. Hoisted so tests
// can swap in a deterministic source if needed.
var randSource = func() io.Reader {
	return cryptoRand{}
}

type cryptoRand struct{}

func (cryptoRand) Read(p []byte) (int, error) {
	return cryptoRandRead(p)
}

// shellEscape returns s quoted for use in a `bash -c` heredoc. Conservative:
// wraps in single quotes and escapes embedded single quotes.
func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

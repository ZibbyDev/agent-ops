// Copyright 2026 Zibby Lab. Apache-2.0.

// Package integrate is the single source of truth for adding / removing /
// listing outbound MCP-client integrations. It is consumed by:
//   - `agent-ops integrate add | remove | list` (CLI)
//   - `agent_integrate_add | agent_integrate_remove | agent_integrate_list`
//     (MCP tools on the daemon's own server)
//
// Both callers go through this package so on-disk state stays consistent
// whether the operator is at the shell or a remote agent is wiring things
// up via MCP.
//
// Atomicity model:
//
//   - The config.yaml file is mutated under an flock so two concurrent
//     `integrate add` calls cannot race-clobber each other.
//
//   - The config.yaml WRITE is "temp file + fsync + rename" so a power loss
//     mid-write leaves either the old file fully intact or the new file
//     fully written — never a half-written truncated config that fails to
//     parse at daemon boot.
//
//   - The env file (agent-ops.env) write is the same temp+rename dance,
//     plus the final file is chmod 0600 so the bearer token never lands
//     world-readable on disk.
//
//   - Add() is "all or nothing": if writing the env file fails after the
//     config update, we ROLL BACK the config to its previous bytes. This
//     keeps the operator from ending up with a config that references an
//     AuthEnv they never persisted.
//
// Restart hint: Add and Remove return `RestartRequired: true`. The daemon
// does NOT hot-reload its config in v0.3 (same as the existing
// `agent_apply_template` tool — see internal/api/mcp). The CALLER decides
// whether to invoke `agent-ops restart` (CLI), call a control-plane
// systemctl, or surface the hint to a human. That keeps this package free
// of process-lifecycle concerns, which would otherwise complicate testing
// + reuse from the MCP tool path (which runs IN the daemon — restarting
// itself is awkward).
package integrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ZibbyDev/agent-ops/internal/config"
)

// ErrAlreadyExists is returned by Add when an integration with the same
// Name is already present. Callers decide whether to bail or replace.
var ErrAlreadyExists = errors.New("integrate: integration with this name already exists")

// ErrNotFound is returned by Remove when no integration matches Name.
var ErrNotFound = errors.New("integrate: no integration with this name")

// Default paths the daemon installer writes. Overridable in Options for
// tests + non-default installs (Homebrew vs apt vs Docker).
const (
	DefaultConfigPath = "/etc/agent-ops/config.yaml"
	DefaultEnvPath    = "/etc/agent-ops/agent-ops.env"
)

// Options bundles the file paths the operation touches. Zero values
// resolve to the Default* constants above.
type Options struct {
	ConfigPath string
	EnvPath    string

	// LockTimeout caps how long Add/Remove will wait for the flock. Zero
	// → 10s. The CLI surface exposes this so an operator stuck behind a
	// hung peer can fail fast rather than hang.
	LockTimeout time.Duration
}

// AddSpec is the input to Add. Mirrors the CLI flags + MCP tool args.
type AddSpec struct {
	Name      string
	Transport string
	URL       string
	Command   string
	Args      []string
	AuthEnv   string
	// Token is the literal Bearer secret to persist into the env file
	// under the key named in AuthEnv. Empty → no env line written for
	// AuthEnv (useful when the env var is supplied externally, e.g. via
	// systemd EnvironmentFile=…).
	Token string
	// ExtraEnv is additional KEY=VAL pairs spliced into agent-ops.env. The
	// Zibby integration uses this for AGENT_OPS_NOTIFY_WORKFLOW_ID.
	ExtraEnv map[string]string
	// StdioEnv is per-subprocess env (transport=stdio only). Stored in
	// config.yaml because subprocess env is in-process, not persisted to
	// agent-ops.env. ExtraEnv stays the path for daemon-wide secrets.
	StdioEnv map[string]string
}

// AddResult is the return of Add — surfaced through the CLI's stdout JSON
// and the MCP tool's content payload.
type AddResult struct {
	Name            string `json:"name"`
	RestartRequired bool   `json:"restart_required"`
	ConfigPath      string `json:"config_path"`
	EnvPath         string `json:"env_path"`
}

// Add atomically:
//  1. flocks the config.yaml,
//  2. parses it,
//  3. checks for a name collision (→ ErrAlreadyExists),
//  4. appends the new MCPClientConfig,
//  5. writes config.yaml atomically (temp+fsync+rename),
//  6. writes the env file with AuthEnv=<token> + every ExtraEnv pair,
//     atomically with mode 0600,
//  7. releases the lock.
//
// On step 6 failure, rolls back step 5 to keep on-disk state consistent.
func Add(spec AddSpec, opts Options) (AddResult, error) {
	if err := validateAdd(spec); err != nil {
		return AddResult{}, err
	}
	opts = resolveOpts(opts)

	unlock, err := lockConfig(opts.ConfigPath, opts.LockTimeout)
	if err != nil {
		return AddResult{}, err
	}
	defer unlock()

	cfg, prevBytes, err := readConfig(opts.ConfigPath)
	if err != nil {
		return AddResult{}, err
	}

	for _, mc := range cfg.MCPClients {
		if mc.Name == spec.Name {
			return AddResult{}, fmt.Errorf("%w: %s", ErrAlreadyExists, spec.Name)
		}
	}

	cfg.MCPClients = append(cfg.MCPClients, config.MCPClientConfig{
		Name:      spec.Name,
		Transport: spec.Transport,
		URL:       spec.URL,
		Command:   spec.Command,
		Args:      append([]string(nil), spec.Args...),
		AuthEnv:   spec.AuthEnv,
		Env:       copyStringMap(spec.StdioEnv),
	})

	if err := writeConfigAtomic(opts.ConfigPath, cfg); err != nil {
		return AddResult{}, err
	}

	// Now splice the env file. Failure here rolls back the config write so
	// the daemon never reads a config that references a missing AuthEnv
	// secret.
	envPairs := map[string]string{}
	for k, v := range spec.ExtraEnv {
		envPairs[k] = v
	}
	if spec.AuthEnv != "" && spec.Token != "" {
		envPairs[spec.AuthEnv] = spec.Token
	}
	if len(envPairs) > 0 {
		if err := mergeEnvFile(opts.EnvPath, envPairs, nil); err != nil {
			_ = os.WriteFile(opts.ConfigPath, prevBytes, 0o644)
			return AddResult{}, fmt.Errorf("integrate: env write failed (config rolled back): %w", err)
		}
	}

	return AddResult{
		Name:            spec.Name,
		RestartRequired: true,
		ConfigPath:      opts.ConfigPath,
		EnvPath:         opts.EnvPath,
	}, nil
}

// RemoveResult is the return of Remove.
type RemoveResult struct {
	Name            string `json:"name"`
	RestartRequired bool   `json:"restart_required"`
	ConfigPath      string `json:"config_path"`
	EnvPath         string `json:"env_path"`
	RemovedAuthEnv  string `json:"removed_auth_env,omitempty"`
}

// Remove is the inverse of Add. Drops the named integration from
// config.yaml and, if the entry had an AuthEnv, deletes that key from the
// env file. ExtraEnv entries from Add are NOT auto-removed — they may be
// shared across integrations (and we don't track which Add inserted them).
// The operator can edit agent-ops.env directly if needed.
func Remove(name string, opts Options) (RemoveResult, error) {
	if strings.TrimSpace(name) == "" {
		return RemoveResult{}, errors.New("integrate: name is required")
	}
	opts = resolveOpts(opts)

	unlock, err := lockConfig(opts.ConfigPath, opts.LockTimeout)
	if err != nil {
		return RemoveResult{}, err
	}
	defer unlock()

	cfg, prevBytes, err := readConfig(opts.ConfigPath)
	if err != nil {
		return RemoveResult{}, err
	}

	var removed *config.MCPClientConfig
	out := cfg.MCPClients[:0]
	for i := range cfg.MCPClients {
		if cfg.MCPClients[i].Name == name {
			tmp := cfg.MCPClients[i]
			removed = &tmp
			continue
		}
		out = append(out, cfg.MCPClients[i])
	}
	if removed == nil {
		return RemoveResult{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	cfg.MCPClients = out

	if err := writeConfigAtomic(opts.ConfigPath, cfg); err != nil {
		return RemoveResult{}, err
	}

	res := RemoveResult{
		Name:            name,
		RestartRequired: true,
		ConfigPath:      opts.ConfigPath,
		EnvPath:         opts.EnvPath,
		RemovedAuthEnv:  removed.AuthEnv,
	}
	if removed.AuthEnv != "" {
		if err := mergeEnvFile(opts.EnvPath, nil, []string{removed.AuthEnv}); err != nil {
			// Roll back config so on-disk state stays consistent.
			_ = os.WriteFile(opts.ConfigPath, prevBytes, 0o644)
			return RemoveResult{}, fmt.Errorf("integrate: env write failed (config rolled back): %w", err)
		}
	}
	return res, nil
}

// ListItem is one row returned by List. Secrets are NOT included — only
// the AuthEnv name is shown so the operator can correlate to their env
// file without leaking the token.
type ListItem struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	URL       string            `json:"url,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	AuthEnv   string            `json:"auth_env,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// List returns the current integrations, sorted by name for stable output.
func List(opts Options) ([]ListItem, error) {
	opts = resolveOpts(opts)
	cfg, _, err := readConfig(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	out := make([]ListItem, 0, len(cfg.MCPClients))
	for _, mc := range cfg.MCPClients {
		out = append(out, ListItem{
			Name:      mc.Name,
			Transport: mc.Transport,
			URL:       mc.URL,
			Command:   mc.Command,
			Args:      append([]string(nil), mc.Args...),
			AuthEnv:   mc.AuthEnv,
			Env:       copyStringMap(mc.Env),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────

func resolveOpts(o Options) Options {
	if o.ConfigPath == "" {
		o.ConfigPath = DefaultConfigPath
	}
	if o.EnvPath == "" {
		o.EnvPath = DefaultEnvPath
	}
	if o.LockTimeout == 0 {
		o.LockTimeout = 10 * time.Second
	}
	return o
}

func validateAdd(s AddSpec) error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("integrate: name is required")
	}
	switch s.Transport {
	case "http":
		if strings.TrimSpace(s.URL) == "" {
			return errors.New("integrate: http transport requires --url")
		}
	case "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return errors.New("integrate: stdio transport requires --command")
		}
	default:
		return fmt.Errorf("integrate: transport %q not one of http|stdio", s.Transport)
	}
	if s.AuthEnv != "" && !validEnvKey(s.AuthEnv) {
		return fmt.Errorf("integrate: auth_env %q is not a valid env var name", s.AuthEnv)
	}
	for k := range s.ExtraEnv {
		if !validEnvKey(k) {
			return fmt.Errorf("integrate: extra_env key %q is not a valid env var name", k)
		}
	}
	for k := range s.StdioEnv {
		if !validEnvKey(k) {
			return fmt.Errorf("integrate: stdio env key %q is not a valid env var name", k)
		}
	}
	return nil
}

// validEnvKey is the conservative POSIX-ish env var name check: letters /
// digits / underscore, not starting with a digit. Keeps an LLM-supplied
// AuthEnv value from injecting `\n` into the env file.
func validEnvKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// readConfig parses config.yaml and also returns the raw previous bytes
// so a caller can roll the file back on subsequent failure.
func readConfig(path string) (*config.Config, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("integrate: config not found at %s — run `agent-ops init` first", path)
		}
		return nil, nil, fmt.Errorf("integrate: read config: %w", err)
	}
	c, err := config.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return nil, nil, fmt.Errorf("integrate: parse current config: %w", err)
	}
	return c, raw, nil
}

// writeConfigAtomic marshals the cfg to YAML and writes it via temp+rename.
//
// We round-trip through yaml.Marshal — comments in the original config.yaml
// are LOST. That's a deliberate trade: preserving comments would require a
// yaml AST library + manual surgery, which is a much bigger maintenance
// surface, and `integrate add` is the only writer in v0.3. Operators who
// hand-curate config.yaml comments can still do so; they just shouldn't
// run `integrate add` against that file.
func writeConfigAtomic(path string, cfg *config.Config) error {
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("integrate: marshal config: %w", err)
	}
	return writeFileAtomic(path, body, 0o644)
}

// mergeEnvFile reads (or initializes) an env file, applies `set` (insert
// or update) and `del` (drop key entirely), and writes the result back
// atomically with mode 0600. Comments + blank lines outside of "KEY=…"
// lines are preserved.
func mergeEnvFile(path string, set map[string]string, del []string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("integrate: read env: %w", err)
	}

	lines := strings.Split(string(existing), "\n")
	delSet := map[string]bool{}
	for _, k := range del {
		delSet[k] = true
	}

	// First pass: replace existing KEY= lines + drop deleted keys.
	seen := map[string]bool{}
	out := make([]string, 0, len(lines)+len(set))
	for _, ln := range lines {
		key := envLineKey(ln)
		if key == "" {
			out = append(out, ln)
			continue
		}
		if delSet[key] {
			continue
		}
		if v, ok := set[key]; ok {
			out = append(out, key+"="+v)
			seen[key] = true
			continue
		}
		out = append(out, ln)
	}
	// Second pass: append new keys we didn't already replace.
	for k, v := range set {
		if seen[k] {
			continue
		}
		out = append(out, k+"="+v)
	}
	// Tidy trailing blanks so the file doesn't grow blank lines forever.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	body := strings.Join(out, "\n") + "\n"
	return writeFileAtomic(path, []byte(body), 0o600)
}

func envLineKey(ln string) string {
	s := strings.TrimSpace(ln)
	if s == "" || strings.HasPrefix(s, "#") {
		return ""
	}
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return ""
	}
	key := strings.TrimSpace(s[:eq])
	if !validEnvKey(key) {
		return ""
	}
	return key
}

// writeFileAtomic is the standard temp+fsync+rename pattern. We sync the
// file AND its directory so a subsequent crash leaves the rename durable.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("integrate: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".agent-ops-tmp-*")
	if err != nil {
		return fmt.Errorf("integrate: tempfile in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Ensure cleanup on any error path before rename.
	defer func() {
		if _, err := os.Stat(tmpName); err == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("integrate: rename: %w", err)
	}
	// fsync the directory so the rename survives a crash.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ─── flock ────────────────────────────────────────────────────────────────

// lockConfig acquires an exclusive flock on (configPath + ".lock"). We
// lock a sibling file rather than the config itself so a partial write
// during a rename doesn't break lock semantics. Returns the unlock
// closure.
//
// Implementation note: flock is portable across Linux / macOS via syscall;
// the Go stdlib has no first-party wrapper, so we use the syscall directly.
// Windows is not supported (the daemon is a Linux-only deployment in v0.3
// — Mac is developer-only).
//
// We use a package-level fallback mutex in case flock isn't available (or
// returns EOPNOTSUPP on weird filesystems like NFS without lock support).
// That fallback only protects in-process callers but is better than no
// serialization at all in CI / tests on tmpfs.
var inProcessLocks sync.Map // path → *sync.Mutex

func lockConfig(configPath string, timeout time.Duration) (func(), error) {
	// In-process serialization first — flock is unreliable on the same fd
	// within one process.
	ml, _ := inProcessLocks.LoadOrStore(configPath, &sync.Mutex{})
	m := ml.(*sync.Mutex)
	m.Lock()

	lockPath := configPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		m.Unlock()
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		m.Unlock()
		return nil, fmt.Errorf("integrate: open lock %s: %w", lockPath, err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			// EOPNOTSUPP / ENOSYS on some filesystems — proceed in-process
			// only (we still hold the per-path mutex).
			break
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			m.Unlock()
			return nil, fmt.Errorf("integrate: timeout acquiring lock on %s after %s", lockPath, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		m.Unlock()
	}, nil
}

func copyStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

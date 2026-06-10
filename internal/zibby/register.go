// Copyright 2026 Zibby Lab. Apache-2.0.

// Package zibby contains the small handshake an agent-ops daemon does back
// to a Zibby control plane after the bootstrap task installs an app: it
// detects which port the installed app is now listening on and tells
// Zibby so the control plane can wire ALB routing for `<id>.apps.zibby.dev`.
//
// Skipped silently when none of the integration env vars are set — keeps
// agent-ops standalone-usable outside the Zibby ecosystem.
package zibby

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/appauth"
)

// RegisterPortIfNeeded picks the app's port and POSTs it to Zibby. Priority:
//  1. AGENT_OPS_APP_PORT env var (set by the Zibby catalog wiring when the
//     deterministic install script declares a known port — e.g. n8n=5678,
//     gastown=8080). Honored as-is, no scan. This is the only way to get
//     the *right* port for apps that bind multiple listeners (e.g. node-red
//     opens an admin socket on a random high port BEFORE the user-facing
//     port comes up, and a /proc scan races and picks the wrong one).
//  2. /proc/net/tcp{,6} scan — first LISTEN-state port that isn't the
//     daemon's own MCP. Fallback for free-form (catalog-less) installs.
//
// No-op if ZIBBY_API_BASE_URL / INSTANCE_ID / AGENT_OPS_TOKEN are unset —
// the daemon is then running standalone.
//
// mcpPort is the port the daemon's own MCP server listens on, excluded
// from the scan so we don't accidentally hand it to ALB as the app port.
func RegisterPortIfNeeded(ctx context.Context, mcpPort int) {
	apiBase := strings.TrimRight(os.Getenv("ZIBBY_API_BASE_URL"), "/")
	instanceID := os.Getenv("INSTANCE_ID")
	token := os.Getenv("AGENT_OPS_TOKEN")
	if apiBase == "" || instanceID == "" || token == "" {
		slog.Info("zibby: register-port skipped (no integration env)",
			"have_api_base", apiBase != "", "have_instance_id", instanceID != "", "have_token", token != "")
		return
	}

	port, source, err := resolveAppPort(mcpPort)
	if err != nil {
		slog.Warn("zibby: resolveAppPort failed", "err", err.Error())
		return
	}
	slog.Info("zibby: resolved app listening port", "port", port, "source", source)

	url := fmt.Sprintf("%s/apps/%s/register-port", apiBase, instanceID)
	body, _ := json.Marshal(map[string]int{"port": port})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("zibby: build request failed", "err", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("zibby: register-port POST failed", "err", err.Error(), "url", url)
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		slog.Warn("zibby: register-port rejected",
			"status", resp.StatusCode, "body", strings.TrimSpace(string(rb)))
		return
	}
	slog.Info("zibby: register-port ok", "status", resp.StatusCode, "port", port)

	// Apply the access-control config the control plane returns on every boot.
	// This is how auth (Basic / IP allowlist / token) survives a task restart
	// WITHOUT being baked into the task definition — the daemon enforces it
	// in-process (internal/appauth); live changes arrive via POST
	// /_zibby_ops/auth. Absent/null `auth` → no gate (passthrough).
	var rr struct {
		Auth json.RawMessage `json:"auth"`
	}
	if json.Unmarshal(rb, &rr) == nil && len(rr.Auth) > 0 && string(rr.Auth) != "null" {
		if err := appauth.Apply(rr.Auth); err != nil {
			slog.Warn("zibby: apply access-control from register-port failed", "err", err.Error())
		} else {
			slog.Info("zibby: applied access-control from register-port")
		}
	}
}

// resolveAppPort returns the port to hand to Zibby. AGENT_OPS_APP_PORT wins
// when present (deterministic catalog installs know the answer ahead of
// time); otherwise we scan /proc. Second return value is "env" or "proc-scan"
// for diagnostics so the operator can tell which path produced a wrong port.
func resolveAppPort(mcpPort int) (int, string, error) {
	if raw := strings.TrimSpace(os.Getenv("AGENT_OPS_APP_PORT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, "", fmt.Errorf("AGENT_OPS_APP_PORT=%q is not an int: %w", raw, err)
		}
		if n <= 0 || n > 65535 {
			return 0, "", fmt.Errorf("AGENT_OPS_APP_PORT=%d is out of range", n)
		}
		return n, "env", nil
	}
	p, err := detectAppPort(mcpPort)
	if err != nil {
		return 0, "", err
	}
	return p, "proc-scan", nil
}

// detectAppPort enumerates LISTEN-state TCP sockets from /proc/net/{tcp,tcp6}
// and returns the first port that's not the daemon's own MCP port. We need
// to read both because many Node apps (n8n included) bind to "::" (IPv6
// any) which only appears in tcp6 even though it accepts IPv4 too via
// IPv4-mapped addresses. Reading /proc avoids a `ss`/`netstat` dependency
// in the container image.
func detectAppPort(skipPort int) (int, error) {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if p, ok := scanListenPorts(path, skipPort); ok {
			return p, nil
		}
	}
	return 0, errors.New("no listening port found beyond mcp")
}

// scanListenPorts reads a /proc/net/{tcp,tcp6} table and returns the first
// LISTEN-state port that isn't skipPort. Returns (0, false) when none found
// or the file is unreadable (e.g. tcp6 disabled in the kernel).
func scanListenPorts(path string, skipPort int) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	// File layout (same for tcp + tcp6):
	//   sl  local_address rem_address   st ...
	//   0:  00000000:1622 00000000:0000 0A ...    ← st 0A = LISTEN
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" {
			continue
		}
		colon := strings.LastIndex(fields[1], ":")
		if colon < 0 {
			continue
		}
		portHex := fields[1][colon+1:]
		decoded, derr := hex.DecodeString(portHex)
		var port int
		if derr != nil || len(decoded) != 2 {
			n, perr := strconv.ParseInt(portHex, 16, 32)
			if perr != nil {
				continue
			}
			port = int(n)
		} else {
			port = int(decoded[0])<<8 | int(decoded[1])
		}
		if port == skipPort {
			continue
		}
		return port, true
	}
	return 0, false
}

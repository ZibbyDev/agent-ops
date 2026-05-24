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
)

// RegisterPortIfNeeded scans /proc/net/tcp for listening sockets, picks the
// app's port (the first listener that isn't the agent-ops MCP itself), and
// POSTs it to Zibby. No-op if any of ZIBBY_API_BASE_URL / INSTANCE_ID /
// AGENT_OPS_TOKEN are unset — the daemon is then running standalone.
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

	port, err := detectAppPort(mcpPort)
	if err != nil {
		slog.Warn("zibby: detectAppPort failed", "err", err.Error())
		return
	}
	slog.Info("zibby: detected app listening port", "port", port)

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

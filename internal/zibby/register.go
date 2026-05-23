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

// detectAppPort enumerates LISTEN-state TCP sockets from /proc/net/tcp and
// returns the first port that's not the daemon's own MCP port. /proc/net
// is read-only and free, avoiding a `ss` / `netstat` dependency in the
// container image.
func detectAppPort(skipPort int) (int, error) {
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return 0, fmt.Errorf("read /proc/net/tcp: %w", err)
	}
	// File layout:
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
		decoded, err := hex.DecodeString(portHex)
		if err != nil || len(decoded) != 2 {
			// Fall back to ParseInt in case the hex has odd length
			n, perr := strconv.ParseInt(portHex, 16, 32)
			if perr != nil {
				continue
			}
			if int(n) == skipPort {
				continue
			}
			return int(n), nil
		}
		port := int(decoded[0])<<8 | int(decoded[1])
		if port == skipPort {
			continue
		}
		return port, nil
	}
	return 0, errors.New("no listening port found beyond mcp")
}

// Copyright 2026 Zibby Lab. Apache-2.0.

// agent-opsd is the long-running daemon entrypoint. It is intentionally
// preserved as a separate, dedicated binary so existing systemd / launchd
// unit files (and the in-flight Fargate task defs that bake
// `agent-opsd --config /etc/agent-ops/config.yaml` as their ExecStart) keep
// working unchanged.
//
// The user-facing CLI (init/start/stop/status/logs/doctor/…) lives in the
// sister binary `agent-ops` (cmd/agent-ops); it shells out to systemctl /
// launchctl, which in turn execs THIS binary as the daemon process. End
// users do not type `agent-opsd` directly.
//
// Usage:
//
//	agent-opsd --config /etc/agent-ops/config.yaml
//	agent-opsd version
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/ZibbyHQ/agent-ops/internal/daemon"
)

// version is set via -ldflags by the release pipeline.
var version = "0.2.1"

func main() {
	// Subcommand routing: `agent-opsd version` short-circuits config load.
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}

	cfgPath := flag.String("config", "/etc/agent-ops/config.yaml", "path to YAML config")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := daemon.Run(*cfgPath, version, logger); err != nil {
		logger.Error("daemon: fatal", "error", err)
		os.Exit(1)
	}
}

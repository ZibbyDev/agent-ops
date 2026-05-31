// Copyright 2026 Zibby Lab. Apache-2.0.

// agent-ops is the user-facing CLI: init / start / stop / status / logs /
// doctor / schedule / mcp. It is a thin dispatcher around
// internal/cli — the daemon loop lives in the sibling binary `agent-opsd`,
// which this CLI invokes (or installs as a system service).
//
// Usage:
//
//	agent-ops init                       # interactive setup
//	sudo agent-ops start                 # install + start the daemon
//	agent-ops status                     # service state + last task
//	agent-ops logs -f                    # tail logs
//	agent-ops doctor                     # diagnose config / env / network
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ZibbyHQ/agent-ops/internal/cli"
)

// version is set via -ldflags by the release pipeline.
var version = "0.2.1"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	root := cli.New(version)
	if err := root.ExecuteContext(ctx); err != nil {
		// cobra already prints the error on stderr; we just need the exit code.
		fmt.Fprintln(os.Stderr, "agent-ops:", err)
		os.Exit(1)
	}
}

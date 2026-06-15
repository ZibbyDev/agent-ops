// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"github.com/spf13/cobra"

	"github.com/ZibbyDev/agent-ops/internal/bootstrap"
	"github.com/ZibbyDev/agent-ops/internal/config"
)

func newMCPCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server helpers.",
	}
	c.AddCommand(newMCPTokenCmd())
	return c
}

func newMCPTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "token",
		Short: "Print the bearer token Claude / Cursor / Codex CLIs need.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			// EnsureToken is idempotent: returns the env-var value if set,
			// otherwise the persisted file, otherwise mints one. Reusing the
			// daemon's exact resolution order keeps `agent-ops mcp token` and
			// the live daemon agreed on which token is in force.
			tok, err := bootstrap.EnsureToken(cfg.StateDir, cfg.MCP.TokenEnv)
			if err != nil {
				return err
			}
			printf(cmd, "%s\n", tok)
			return nil
		},
	}
}

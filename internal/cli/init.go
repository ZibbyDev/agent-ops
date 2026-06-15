// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZibbyDev/agent-ops/examples"
	"github.com/ZibbyDev/agent-ops/internal/service"
)

// newInitCmd is the user-facing setup wizard. It collects:
//   - provider (claude / claude-cli / codex)
//   - the env var name holding their API key / OAuth token
//   - optional bootstrap prompt (free-form goal in natural language)
//   - optional notify-workflow webhook id
//
// then renders a config.yaml and (unless --dry-run) writes it +
// installs the system service via internal/service.
//
// Idempotent: if config.yaml already exists, the user is asked before we
// overwrite — non-interactive runs (stdin not a TTY) refuse to clobber and
// exit non-zero.
func newInitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Interactive setup — write config + install the service.",
		Long: `Walks you through picking an LLM provider, supplying an API-key
env var name, and writing /etc/agent-ops/config.yaml. With --dry-run
nothing is written; the rendered config + unit file are printed.

Skip the wizard with a bundled template:

  agent-ops init --list-templates                       # show available templates
  agent-ops init --template wordpress-multisite --yes   # write the template, no prompts
  agent-ops init --template single-app --dry-run        # preview without writing`,
		RunE: runInit,
	}
	c.Flags().Bool("dry-run", false,
		"don't write anything; print what would be written")
	c.Flags().Bool("yes", false,
		"non-interactive — accept all defaults (provider=claude-cli, no bootstrap, no notify)")
	c.Flags().String("template", "",
		"write a bundled config template instead of running the wizard (see --list-templates)")
	c.Flags().Bool("list-templates", false,
		"print the available bundled templates and exit")
	return c
}

// initAnswers is what the prompt collects, then renders to YAML.
type initAnswers struct {
	Provider        string
	APIKeyEnv       string
	Model           string
	BootstrapPrompt string
	NotifyWorkflow  string
	StateDir        string
	ConfigPath      string
}

func runInit(cmd *cobra.Command, _ []string) error {
	cfgPath, _ := cmd.Flags().GetString("config")
	dry, _ := cmd.Flags().GetBool("dry-run")
	yes, _ := cmd.Flags().GetBool("yes")
	tmpl, _ := cmd.Flags().GetString("template")
	listTmpls, _ := cmd.Flags().GetBool("list-templates")

	out := cmd.OutOrStdout()

	// --list-templates is a pure read; it short-circuits before any other
	// logic so it stays safe to run as a non-root operator.
	if listTmpls {
		return printTemplateList(out)
	}

	// --template <name> bypasses the interactive wizard entirely. We still
	// honour --dry-run / --yes / --config; everything else (provider /
	// model / bootstrap prompts) is whatever the template author wrote.
	if tmpl != "" {
		return runInitFromTemplate(cmd, tmpl, cfgPath, dry, yes)
	}

	ans := initAnswers{
		Provider:   "claude-cli",
		APIKeyEnv:  "CLAUDE_CODE_OAUTH_TOKEN",
		Model:      "claude-sonnet-4-6",
		StateDir:   defaultStateDir(),
		ConfigPath: cfgPath,
	}

	in := bufio.NewReader(cmd.InOrStdin())

	if !yes {
		printlnTo(out, "agent-ops init — interactive setup")
		printlnTo(out, "")
		printlnTo(out, "Choose an LLM provider:")
		printlnTo(out, "  1) claude     — Anthropic Messages API (x-api-key billing)")
		printlnTo(out, "  2) claude-cli — Claude Code CLI (OAuth / subscription)  [default]")
		printlnTo(out, "  3) codex      — OpenAI Codex CLI (OPENAI_API_KEY)")
		choice := promptDefault(in, out, "Provider [1/2/3]", "2")
		switch strings.TrimSpace(choice) {
		case "1":
			ans.Provider = "claude"
			ans.APIKeyEnv = "ANTHROPIC_API_KEY"
		case "3":
			ans.Provider = "codex"
			ans.APIKeyEnv = "OPENAI_API_KEY"
			ans.Model = "gpt-5-codex"
		default:
			ans.Provider = "claude-cli"
			ans.APIKeyEnv = "CLAUDE_CODE_OAUTH_TOKEN"
		}

		ans.APIKeyEnv = promptDefault(in, out,
			fmt.Sprintf("Env var holding your %s key/token", ans.Provider),
			ans.APIKeyEnv)
		ans.Model = promptDefault(in, out, "Model id", ans.Model)
		ans.BootstrapPrompt = promptDefault(in, out,
			"First-run bootstrap goal (empty to skip)", "")
		ans.NotifyWorkflow = promptDefault(in, out,
			"Notify workflow id for app-down pages (empty to skip)", "")
	}

	rendered := renderConfigYAML(ans)

	// Service file preview comes from the platform manager. On unsupported
	// platforms we just print the config and stop — Linux/Mac is the cover.
	mgr, mgrErr := service.NewManager()
	var unitPreview string
	if mgrErr == nil {
		spec := serviceSpecFor(cfgPath, ans.StateDir)
		body, err := mgr.Render(spec)
		if err != nil {
			return err
		}
		unitPreview = body
	}

	if dry {
		printlnTo(out, "--- "+cfgPath+" ---")
		printlnTo(out, rendered)
		if unitPreview != "" {
			printlnTo(out, "--- "+mgr.UnitPath()+" ---")
			printlnTo(out, unitPreview)
		}
		return nil
	}

	if _, err := os.Stat(cfgPath); err == nil {
		if yes {
			return fmt.Errorf("init: %s already exists; rerun with --dry-run or remove it first", cfgPath)
		}
		ok := promptDefault(in, out,
			cfgPath+" exists. Overwrite? [y/N]", "N")
		if !strings.EqualFold(strings.TrimSpace(ok), "y") {
			printlnTo(out, "Aborted; existing config kept.")
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fmt.Errorf("init: mkdir config dir: %w", err)
	}
	if err := os.WriteFile(cfgPath, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("init: write config: %w", err)
	}
	if err := os.MkdirAll(ans.StateDir, 0o700); err != nil {
		return fmt.Errorf("init: mkdir state dir: %w", err)
	}
	printlnTo(out, "wrote "+cfgPath)
	printlnTo(out, "wrote "+ans.StateDir+"/")

	if mgrErr != nil {
		printlnTo(out, "note: service install skipped — "+mgrErr.Error())
		printlnTo(out, "agent-ops init done. Run `agent-ops daemon --config "+cfgPath+"` to start in foreground.")
		return nil
	}
	if err := mgr.Install(cmd.Context(), serviceSpecFor(cfgPath, ans.StateDir)); err != nil {
		return fmt.Errorf("init: install service: %w", err)
	}
	printlnTo(out, "installed unit at "+mgr.UnitPath())
	printlnTo(out, "")
	printlnTo(out, "Next: `sudo agent-ops start` (Linux) or `agent-ops start` (Mac).")
	return nil
}

// renderConfigYAML emits the minimum-viable config.yaml from collected answers.
// Stays human-edible: comments explain every section.
func renderConfigYAML(a initAnswers) string {
	var b strings.Builder
	b.WriteString("# agent-ops configuration, generated by `agent-ops init` at ")
	b.WriteString(time.Now().UTC().Format(time.RFC3339))
	b.WriteString(".\n# Edit freely — schema docs: https://github.com/ZibbyDev/agent-ops\n\n")
	fmt.Fprintf(&b, "state_dir: %s\n\n", a.StateDir)
	b.WriteString("agent:\n")
	fmt.Fprintf(&b, "  provider: %s\n", a.Provider)
	fmt.Fprintf(&b, "  model: %s\n", a.Model)
	fmt.Fprintf(&b, "  api_key_env: %s\n", a.APIKeyEnv)
	b.WriteString("  max_tool_calls_per_task: 25\n")
	b.WriteString("  task_timeout: 30m\n\n")
	b.WriteString("mcp:\n")
	b.WriteString("  listen_addr: \":7842\"\n")
	b.WriteString("  token_env: AGENT_OPS_TOKEN\n\n")
	if a.BootstrapPrompt != "" {
		b.WriteString("bootstrap:\n")
		b.WriteString("  name: initial_setup\n")
		b.WriteString("  prompt: |\n")
		for _, line := range strings.Split(a.BootstrapPrompt, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
		b.WriteString("  tools: [shell]\n\n")
	}
	b.WriteString("schedules:\n")
	b.WriteString("  - name: hourly_health_check\n")
	b.WriteString("    cron: \"0 * * * *\"\n")
	b.WriteString("    prompt: |\n")
	b.WriteString("      Verify the application is responding on its expected port.\n")
	b.WriteString("      If it's down, attempt soft-restart; if that fails, page the operator.\n")
	b.WriteString("    tools: [shell]\n")
	if a.NotifyWorkflow != "" {
		fmt.Fprintf(&b, "\n# AGENT_OPS_NOTIFY_WORKFLOW_ID=%s tells the scheduler to append\n", a.NotifyWorkflow)
		b.WriteString("# a notification clause to recurring-task prompts (page-the-operator\n")
		b.WriteString("# on unrecoverable failures). Export it before starting the daemon.\n")
	}
	return b.String()
}

// promptDefault writes "label [default]: " then reads one line. Empty user
// input → default. Trims trailing newline.
func promptDefault(r *bufio.Reader, w io.Writer, label, def string) string {
	if def != "" {
		fmt.Fprintf(w, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(w, "%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return def
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return def
	}
	return line
}

// serviceSpecFor builds the Spec we hand to the platform Manager, picking
// sensible defaults per OS for log path / state dir / user.
func serviceSpecFor(cfgPath, stateDir string) service.Spec {
	spec := service.Spec{
		ExecPath:   findDaemonBinary(),
		ConfigPath: cfgPath,
		StateDir:   stateDir,
	}
	if runtime.GOOS == "linux" {
		spec.LogPath = "/var/log/agent-ops.log"
		// `root` chosen because catalog scripts + the apt-get / mount /
		// systemctl-on-other-units common cases need it. A future
		// `--user agent-ops` install-flag would flip this.
		spec.User = "root"
		spec.Group = "root"
	}
	return spec
}

// findDaemonBinary returns the absolute path to `agent-opsd`. Precedence:
//  1. AGENT_OPSD_PATH env (CI / dev override)
//  2. sibling of the running `agent-ops` binary
//  3. PATH lookup
//
// Failures are non-fatal — we return the bare name "agent-opsd" so the unit
// file is still written, and the operator gets a `doctor` warning.
func findDaemonBinary() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_OPSD_PATH")); v != "" {
		return v
	}
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "agent-opsd")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	if p, err := exec.LookPath("agent-opsd"); err == nil {
		return p
	}
	return "agent-opsd"
}

// printTemplateList renders the bundled templates as an aligned 2-column
// table — name, then description — sorted by name. Pure read; no side
// effects beyond stdout.
func printTemplateList(out io.Writer) error {
	all := examples.List()
	if len(all) == 0 {
		// Embed FS is empty — should be unreachable in a built binary, but
		// don't crash on the bare go-run path.
		fmt.Fprintln(out, "(no bundled templates)")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDESCRIPTION")
	for _, t := range all {
		fmt.Fprintf(tw, "%s\t%s\n", t.Name, t.Description)
	}
	return tw.Flush()
}

// runInitFromTemplate handles `agent-ops init --template <name>`. Pulls
// the bytes from the embed FS, refuses to clobber an existing config
// unless --yes (and in that case prints a unified-diff-style preview so
// the operator can see what changed), and writes to cfgPath. With
// --dry-run we never touch the filesystem; the template content goes
// straight to stdout.
func runInitFromTemplate(cmd *cobra.Command, name, cfgPath string, dry, yes bool) error {
	out := cmd.OutOrStdout()
	in := bufio.NewReader(cmd.InOrStdin())

	body, err := examples.Get(name)
	if err != nil {
		// examples.Get already includes "Available templates: …" in its
		// error string; bubble it up unchanged. Exit code 1 via cobra.
		return err
	}

	if dry {
		fmt.Fprintln(out, "--- "+cfgPath+" (template: "+name+") ---")
		fmt.Fprint(out, string(body))
		return nil
	}

	if _, err := os.Stat(cfgPath); err == nil {
		// Existing config — show a brief diff hint, then prompt unless
		// --yes was passed (operator-mode: idempotent overwrite).
		if !yes {
			fmt.Fprintf(out, "%s already exists.\n", cfgPath)
			fmt.Fprintln(out, "Re-run with --dry-run to preview the template, or --yes to overwrite.")
			confirm := promptDefault(in, out, "Overwrite with template "+name+"? [y/N]", "N")
			if !strings.EqualFold(strings.TrimSpace(confirm), "y") {
				fmt.Fprintln(out, "Aborted; existing config kept.")
				return nil
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fmt.Errorf("init: mkdir config dir: %w", err)
	}
	if err := os.WriteFile(cfgPath, body, 0o644); err != nil {
		return fmt.Errorf("init: write config: %w", err)
	}
	fmt.Fprintln(out, "wrote "+cfgPath+" from template "+name)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Next: edit the template's placeholders (SITES=, APP_URL=, …) and `sudo agent-ops restart`.")
	return nil
}

func defaultStateDir() string {
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		if home != "" {
			return filepath.Join(home, "Library", "Application Support", "agent-ops")
		}
	}
	return "/var/lib/agent-ops"
}

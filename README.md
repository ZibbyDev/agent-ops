# agent-ops

> An autonomous DevOps engineer that lives next to your application.

> WARNING — Experimental. Do NOT run on production hosts you can't afford
> to lose. This is a research project exploring "what if a small autonomous
> LLM agent acted as the operator of a single host?" — it can run arbitrary
> shell commands on the box. The API, config schema, on-disk state format,
> and security model will all change between minor versions. Pin to a
> commit / tag if you depend on a specific behavior.

`agent-ops` is a small open-source daemon that wraps an LLM agent (Claude,
Codex; Gemini and Ollama next) and runs scheduled + on-demand operations
tasks on a host. You bring an API key (or an OAuth token); you tell it what
should be true in natural language; it uses tools (shell today, more on the
roadmap) to keep things that way.

**Status: experimental (v0.2).** Single-host MVP. The abstractions are
cluster-ready so v1.0 *could* grow into a multi-node design — that's the
hypothesis we're testing, not a delivery commitment.

---

## Install

### macOS / Linux — Homebrew

```bash
brew install zibbyhq/tap/agent-ops
```

### Debian / Ubuntu — APT (signed repo on dl.zibby.app)

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://dl.zibby.app/apt/key.gpg \
  | sudo gpg --dearmor -o /etc/apt/keyrings/zibby.gpg
echo "deb [signed-by=/etc/apt/keyrings/zibby.gpg] https://dl.zibby.app/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/zibby.list
sudo apt update && sudo apt install agent-ops
```

Subsequent `apt upgrade` runs pick up new releases as they ship.

### Anywhere — direct tarball (dl.zibby.app)

```bash
# Auto-detects OS + arch from `uname`.
curl -fsSL "https://dl.zibby.app/agent-ops/latest/agent-ops_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz" \
  | sudo tar -xz -C /usr/local/bin

# Or pin a specific version:
curl -fsSL https://dl.zibby.app/agent-ops/v0.2.0/agent-ops_linux_amd64.tar.gz \
  | sudo tar -xz -C /usr/local/bin
```

### Anywhere — direct tarball (GitHub Releases)

If you'd rather not go through dl.zibby.app, every release is also
attached directly to its GitHub Release:

```bash
curl -fsSL https://github.com/ZibbyHQ/agent-ops/releases/latest/download/agent-ops_linux_amd64.tar.gz \
  | sudo tar -xz -C /usr/local/bin
# Same URL pattern for: agent-ops_linux_arm64 / agent-ops_darwin_amd64 / agent-ops_darwin_arm64
```

### Docker

```bash
docker run -d \
  --name agent-ops \
  -p 7842:7842 \
  -v /var/lib/agent-ops:/var/lib/agent-ops \
  -v $(pwd)/config.yaml:/etc/agent-ops/config.yaml:ro \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e AGENT_OPS_TOKEN=$(openssl rand -hex 32) \
  ghcr.io/zibbyhq/agent-ops:latest
```

---

## Choose a starting template

`agent-ops` ships three bundled config templates so you don't have to
write YAML by hand. Pick the one closest to your stack — every template
includes liveness checks, housekeeping, and weekly security patching;
they differ in the web stack they target and how they discover sites.

| Template | Best for | Highlights |
|---|---|---|
| `single-app` | One WordPress + MySQL site on a budget VPS | Hourly liveness, nightly mysqldump, weekly `-security` updates |
| `wordpress-multisite` | Apache + multiple WordPress installs on one host | 5-min per-site curl + auto-heal, daily cert renewal + WP integrity scan |
| `nodejs-server` | Single Node.js app under PM2, nginx in front | PM2 health + crash-loop detection, `npm audit`, log rotation |

```bash
agent-ops init --list-templates                       # see them all
agent-ops init --template wordpress-multisite --yes   # write straight to /etc/agent-ops/config.yaml
agent-ops init --template single-app --dry-run        # preview without writing
```

Each template's source is in [`examples/`](./examples) — readable on
GitHub, embedded in the binary so the CLI works offline. If you're
driving the daemon from a remote AI agent (Claude Code, Cursor, …), the
MCP server exposes the same surface via `agent_list_templates`,
`agent_get_template`, and `agent_apply_template` — see the MCP table
below.

## Quickstart

After install, three commands set up the daemon under systemd (Linux) or
launchd (macOS):

```bash
sudo agent-ops init        # interactive: provider, token env, optional goal
sudo agent-ops start       # install service unit + start
agent-ops status           # check it's alive
agent-ops logs -f          # tail
```

Stop / restart / uninstall live where you expect:

```bash
agent-ops stop
agent-ops restart
agent-ops uninstall        # removes unit; --purge to also remove config/state
```

Diagnose problems:

```bash
agent-ops doctor
```

(checks: config valid? API key env set? provider CLI on PATH? state dir
writable? MCP port free? upstream LLM reachable?)

---

## Quickstart: WordPress + MySQL health monitoring

A concrete scenario — agent-ops keeps a small WordPress install
self-healing.

The full, copy-pasteable config for a real-world WordPress + Apache + MySQL
host (multiple sites, 1 GB RAM, WooCommerce-friendly) lives at
[`examples/wordpress-multisite.yaml`](examples/wordpress-multisite.yaml).
That example covers all of:

- 5-minute liveness check on Apache + MySQL + per-site `curl`, auto-restart
  on OOM
- Hourly system housekeeping (memory, disk, runaway PHP-FPM workers,
  abandoned MySQL connections, log rotation)
- Daily 02:30 audit (mysqldump freshness, Let's Encrypt cert renewal,
  WordPress file integrity — flags suspicious PHP under uploads)
- Weekly security patches (ubuntu `-security` only, conservative)

Copy it in place of `config.yaml`, edit the `SITES=` / `DBS=` lines for
your domains, `agent-ops restart`. Done.

Don't need the multi-site machinery? Use the stripped-down equivalent —
[`examples/single-app.yaml`](./examples/single-app.yaml) — which covers
the same three jobs (liveness, nightly mysqldump, weekly security
patches) for a one-site WordPress + MySQL + Nginx host:

```bash
sudo agent-ops init --template single-app --yes   # writes /etc/agent-ops/config.yaml
sudo agent-ops start
```

Now your WordPress will self-heal on minor failures, get backed up nightly,
and security-patched weekly — and you'll get a notification via your
configured webhook if anything goes wrong that the agent can't recover from.

### Wiring the notify webhook

Set `AGENT_OPS_NOTIFY_WORKFLOW_ID` in `/etc/agent-ops/agent-ops.env` (read
by the systemd unit). The scheduler appends a clause to recurring-task
prompts telling the LLM to shell-out to your notification tool — typically
the Zibby CLI (`zibby workflow trigger notify-app-down …`) — only after
recovery attempts have failed. Set it to whatever id your tool expects;
non-Zibby setups can swap the shell-out for `curl https://hooks.slack.com/…`
in the prompt directly.

---

## Configuration

A complete `config.yaml` lives at
[`config.example.yaml`](./config.example.yaml). Schema highlights:

| Section | Required | Purpose |
|---|---|---|
| `agent.provider` | yes | `claude` / `claude-cli` / `codex` / `gemini` / `ollama` (last two stubs) |
| `agent.model` | for claude | Model id (e.g. `claude-sonnet-4-6`) |
| `agent.api_key_env` | for cloud | Name of env var holding the API key / OAuth token |
| `schedules[]` | optional | Cron-fired prompts + tool allowlist |
| `bootstrap` | optional | One-shot prompt run on first daemon start |
| `mcp.listen_addr` | optional | Defaults to `:7842` |

Per-schedule `model:` overrides the daemon-wide default so you can route
cheap checks to Haiku and reserve Sonnet for upgrades / incident response.

---

## MCP tool surface

Each daemon exposes a streamable-HTTP MCP server. Point your editor's AI
chat (Claude Code, Cursor, Codex CLI, Gemini CLI) at it:

```json
{
  "mcpServers": {
    "agent-ops-prod": {
      "url": "http://your-host:7842/mcp",
      "headers": { "Authorization": "Bearer YOUR_AGENT_OPS_TOKEN" }
    }
  }
}
```

(Grab the token with `agent-ops mcp token`.)

| Tool | Purpose |
|---|---|
| `agent_status` | Daemon health + last run |
| `agent_list_tasks` / `agent_get_task` / `agent_set_task` | Manage scheduled tasks |
| `agent_run_now` | Trigger an ad-hoc run |
| `agent_history` | Recent runs |
| `agent_logs` | Per-line log of one run |
| `agent_list_templates` / `agent_get_template` / `agent_apply_template` | Pick + install a bundled config template (mirrors `agent-ops init --template`) |
| `host_shell` | Direct shell exec (skip the LLM) |

---

## Architecture

```
┌── compute host (Linux box, VPS, EC2, container, Pi, …) ────────────┐
│                                                                    │
│   ┌── agent-ops daemon (Go, ~15MB) ───────────────────────────┐    │
│   │                                                            │    │
│   │   Scheduler  ─►  Task Runner  ─►  Driver (Claude/Codex)   │    │
│   │                                    │                       │    │
│   │                                    ▼  tool calls           │    │
│   │                              Tool registry (shell …)       │    │
│   │                                                            │    │
│   │   MCP server (:7842)  ◄── Claude Code / Cursor / Codex     │    │
│   │   SQLite state (event-log + tables)                        │    │
│   └───────────────────────────────────────────────────────────┘    │
│                                                                    │
│   ┌── your application ──────────────────────────────────────┐    │
│   │ WordPress / Postgres / n8n / whatever                    │    │
│   └──────────────────────────────────────────────────────────┘    │
└────────────────────────────────────────────────────────────────────┘
```

- **Two binaries**, ~15MB and ~19MB:
  - `agent-opsd` — the long-running daemon. Started by systemd / launchd.
  - `agent-ops` — the user-facing CLI (init / start / stop / status / logs / doctor / schedule / mcp).
- **SQLite + event log** — every state change is appended to `events`
  before its table is updated. v1.0's Raft layer will ship the same events
  to replicas without touching domain tables.
- **Sidecar by design** — doesn't replace your app, lives next to it.

---

## What's not done yet

- **No clustering**. v0.2 is single-node. Cluster mode (Raft consensus,
  pilot/worker, leader election) is on the v0.5 roadmap.
- **Three LLM drivers** (Claude REST API, Claude Code CLI, OpenAI Codex
  CLI). Gemini / Ollama are stub interfaces.
- **One tool** (`shell`). `http`, `fs`, `docker`, `kubectl`, `git` are in
  the design but not implemented — agent-ops talks to vendor CLIs via
  shell-out for now (e.g. the Zibby flavour image ships `@zibby/cli` so the
  agent can run `zibby workflow trigger …` directly).
- **No telemetry export**. Internal slog only. OTel exporter in v0.3.
- **No Windows**. Service install supports systemd + launchd only.

See [`ROADMAP.md`](./ROADMAP.md) for the post-v0.2 plan.

---

## Relationship to Zibby

This is part of [Zibby](https://zibby.dev)'s open ecosystem. The Zibby
control plane uses `agent-ops` as the sidecar inside every hosted
application instance, and the Zibby CLI / MCP (`@zibby/mcp-cli`) provisions
instances + handles token lifecycle. Run agent-ops standalone too: it has
no required runtime dependency on Zibby.

---

## Contributing

PRs welcome. We require a [CLA](./CONTRIBUTING.md) for non-trivial
contributions so the project stays cleanly Apache-2.0 licensable.

For security reports, see [`SECURITY.md`](./SECURITY.md).

---

## Experiment, not a product

This repo is an active design experiment. Specifically we want to find out:

1. Does an LLM agent on a single host actually replace a human ops person
   for small workloads? Or does it churn out plausible-looking actions that
   quietly drift state.
2. Is "natural language mission journal" a stable abstraction? Or do users
   want stricter Resource specs (Kubernetes-CRD style)?
3. Where's the right line between in-process tools (`shell`) and outbound
   integrations (Slack, GitHub, …)? And how does the agent learn that line.
4. How does the cluster shape land? v0.x reserves Node + Resource +
   event-log seams for a future Raft pilot/worker design — we want to know
   if that shape survives real multi-node usage.

If those questions don't pan out, the project may pivot or shut down. If
they do, v1.0 freezes a stable API.

In the meantime:
- Expect breaking changes between every minor version
- Expect bugs in the agent's judgment, not just its code
- Don't point this at anything you can't lose
- Telemetry from your runs (when implemented) goes only to your own
  backend — we are not collecting anything

---

## License

[Apache 2.0](./LICENSE) — © 2026 Zibby Lab.

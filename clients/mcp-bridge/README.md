# @zibby/agent-ops-mcp

> stdio MCP bridge — connect a local AI agent (Claude Code, Cursor, OpenAI Codex CLI, Gemini CLI) to a remote agent-ops daemon.

The [`agent-ops`](https://github.com/ZibbyDev/agent-ops) daemon speaks MCP over Streamable HTTP at `/mcp`. In theory every modern MCP client supports remote HTTP transport — in practice their support varies (auth-header propagation, SSE channel survival, HTTPS strictness, version drift). The stdio transport, on the other hand, is universally supported.

This package is a tiny (~250 line, zero-dep) stdio MCP server that lives in your AI agent's local process. Every JSON-RPC frame it receives, it forwards as one HTTPS POST to your remote agent-ops daemon, then writes the response back on stdout. No translation. Full pass-through.

## Install + configure

You don't install this manually. Your AI agent installs it on demand via `npx -y` whenever you point at the bridge in your config.

### Claude Code (`~/.claude/settings.json`)

```json
{
  "mcpServers": {
    "ops-my-opendesign": {
      "command": "npx",
      "args": ["-y", "@zibby/agent-ops-mcp"],
      "env": {
        "AGENT_OPS_URL": "https://my-instance.apps.zibby.app:7842/mcp",
        "AGENT_OPS_TOKEN": "ao_xxxxxxxx"
      }
    }
  }
}
```

### Cursor (`~/.cursor/mcp.json`)

```json
{
  "mcpServers": {
    "ops-my-opendesign": {
      "command": "npx",
      "args": ["-y", "@zibby/agent-ops-mcp"],
      "env": {
        "AGENT_OPS_URL": "https://my-instance.apps.zibby.app:7842/mcp",
        "AGENT_OPS_TOKEN": "ao_xxxxxxxx"
      }
    }
  }
}
```

### OpenAI Codex CLI (`~/.codex/config.toml`)

```toml
[mcp_servers.ops-my-opendesign]
command = "npx"
args    = ["-y", "@zibby/agent-ops-mcp"]

[mcp_servers.ops-my-opendesign.env]
AGENT_OPS_URL   = "https://my-instance.apps.zibby.app:7842/mcp"
AGENT_OPS_TOKEN = "ao_xxxxxxxx"
```

### Gemini CLI (`~/.gemini/settings.json`)

Same as Claude Code.

## Where do `AGENT_OPS_URL` + `AGENT_OPS_TOKEN` come from?

Two paths:

1. **Self-hosted agent-ops daemon** — read `<state_dir>/mcp.token` on the host (default `/var/lib/agent-ops/mcp.token`); the URL is wherever your daemon is reachable on port 7842.

2. **Zibby-hosted agent-ops sidecar** — after you `zibby_deploy_app(...)` via `@zibby/mcp-cli`, the deploy response includes both fields; or call `zibby_app_mcp_config({instanceId})` later to retrieve them.

## Environment variables

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `AGENT_OPS_URL` | yes | — | Full `https://host:7842/mcp` endpoint |
| `AGENT_OPS_TOKEN` | yes | — | Bearer token issued by the daemon on first boot |
| `AGENT_OPS_TIMEOUT_MS` | no | `30000` | Per-request timeout in ms. Minimum 1000. |

## What it does NOT do

- **No translation** — the daemon is the source of truth for tool names, schemas, and behavior. Whatever changes there shows up here automatically.
- **No retries** — single attempt per request. Failures surface as JSON-RPC error frames (codes `-32603` network, `-32001` auth, `-32700` parse).
- **No SSE** — the daemon doesn't push server-initiated notifications in v0.1; if/when it does, a future version of this bridge will open a GET stream alongside.
- **No multi-instance multiplex** — one bridge process per agent-ops instance. If you have 5 instances, add 5 entries to your agent config.

## Tests

```bash
npm test
```

Runs node's built-in test runner against:
- Unit tests for `makeErrorResponse`, `readConfig`, `forwardOne` (with a local httptest mock)
- Integration tests for `runBridge` with in-memory streams
- End-to-end tests that spawn the bridge as a real subprocess and talk to it through real stdio pipes

All 21 tests must pass before publish.

## Versioning

`@zibby/agent-ops-mcp@<x.y.z>` tracks the agent-ops daemon's API. The bridge is intentionally thin — it should keep working across daemon versions because all it does is forward bytes. We bump only on (rare) wire-protocol changes.

## License

Apache 2.0 — © 2026 Zibby Lab.

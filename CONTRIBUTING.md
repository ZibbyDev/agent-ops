# Contributing to agent-ops

Thanks for considering a contribution. This project is small, opinionated, and OSS by design — your patches are welcome.

## CLA

For non-trivial contributions (anything beyond a typo or doc tweak) we ask contributors to sign a CLA via [cla-assistant.io](https://cla-assistant.io). The bot will comment on your first PR with a one-click signing flow. We use this so the project stays cleanly Apache-2.0 — no accidental copyleft, no contributor-by-contributor licensing audits later.

If you can't sign (employer policy etc.), open an issue first; we can usually find a path forward.

## Conventional commits

PR titles + commits should follow [Conventional Commits](https://www.conventionalcommits.org/) so we can auto-generate changelogs:

```
feat(driver): add Codex driver
fix(scheduler): drop in-flight tasks on shutdown
docs(readme): clarify MCP token rotation
```

Types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `perf`.

## Local development

```bash
# clone
git clone https://github.com/ZibbyDev/agent-ops.git
cd agent-ops

# install deps + build
go build ./...

# run all tests
go test ./...

# run the daemon against a local config (set your Anthropic key)
cp config.example.yaml config.local.yaml
export ANTHROPIC_API_KEY=sk-ant-...
go run ./cmd/agent-opsd --config config.local.yaml
```

## What to send

We're happy to look at:

- Bug fixes (please include a regression test)
- New drivers (`codex`, `gemini`, `ollama`) — see `internal/driver/driver.go` for the interface
- New host tools — see `internal/tool/tool.go` for the interface, `shell.go` as the reference implementation
- Doc / example improvements
- Performance fixes with benchmarks

We're more cautious about:

- Sprawling refactors of core types (state, scheduler, MCP). Open an issue first; these shapes need to survive the cluster transition (see `ROADMAP.md`)
- New top-level dependencies. The dep budget is intentionally small (cron, sqlite, yaml). Justify the addition in the PR.
- Changes to the public MCP tool surface. These are a stable API in v1.0; we don't churn them.

## Code style

- `gofmt`-clean (CI rejects unformatted code)
- `go vet ./...` passes
- Avoid `interface{}` / `any` in public APIs except where the JSON wire shape requires it
- Comments explain *why*, not *what* (the code already says what)

## Reporting security issues

Don't open a public issue. See [`SECURITY.md`](./SECURITY.md).

## Code of Conduct

See [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md).

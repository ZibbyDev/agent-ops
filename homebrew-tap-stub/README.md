# homebrew-tap (stub layout)

This directory is the **template** for the
[`ZibbyDev/homebrew-tap`](https://github.com/ZibbyDev/homebrew-tap) GitHub repo
that goreleaser PRs into on every release.

The operator creates the actual GitHub repo manually (the release subagent
intentionally does NOT touch GitHub from CI), then copies the layout below
into it. Subsequent releases overwrite `Casks/agent-ops.rb` automatically —
this stub is only needed for the first tag, so `brew tap` has something to
clone.

## Layout

```
homebrew-tap/
├── README.md          (the operator's README — tells users `brew install …`)
└── Casks/
    └── agent-ops.rb   (stub; goreleaser overwrites on each release)
```

## Operator setup steps (one-time)

1. `gh repo create ZibbyDev/homebrew-tap --public --description "Homebrew tap for ZibbyDev tools"`
2. Copy `README.md` + `Casks/agent-ops.rb` from this directory into the new repo.
3. Push to `main`.
4. Add a fine-grained PAT with write access to `ZibbyDev/homebrew-tap` and
   expose it as the `HOMEBREW_TAP_TOKEN` secret on the `agent-ops` repo's
   Actions settings — goreleaser uses this to PR the cask update on each
   tag push.
5. Tag `v0.2.0` in `agent-ops` to trigger the first real release; verify
   `Casks/agent-ops.rb` got overwritten with real SHA256s + version.

Users then install with:

```bash
brew tap zibbyhq/tap
brew install agent-ops
```

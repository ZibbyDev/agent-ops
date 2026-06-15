# Stub — goreleaser will overwrite this file on every tag push.
# Until the first release of agent-ops v0.2.0 lands, this stub is the only
# thing Homebrew sees when a user runs `brew install zibbyhq/tap/agent-ops`,
# so it's wired to fail loudly with a clear message rather than installing
# stale binaries from a previous tag.
cask "agent-ops" do
  version "0.0.0-stub"

  url "https://github.com/ZibbyDev/agent-ops/releases"
  name "agent-ops"
  desc "Autonomous DevOps engineer for a single host."
  homepage "https://github.com/ZibbyDev/agent-ops"

  # Intentionally fail until the first real release lands.
  preflight do
    odie "agent-ops has not been released yet. Watch the GitHub repo for v0.2.0."
  end
end

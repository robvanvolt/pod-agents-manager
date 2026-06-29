# Homebrew tap for pod-agents-manager

This directory holds the Homebrew formula
([`pod-agents-manager.rb`](pod-agents-manager.rb)) under version control. The
formula itself must live in a **separate tap repository** for `brew install` to
find it.

## One-time: create the tap repo

A Homebrew tap is just a GitHub repo named `homebrew-<tap>`:

```bash
# 1. Create a repo named  robvanvolt/homebrew-tap  on GitHub, then:
git clone https://github.com/robvanvolt/homebrew-tap.git
cd homebrew-tap
mkdir -p Formula
cp /path/to/pod-agents-manager/packaging/homebrew/pod-agents-manager.rb Formula/
git add Formula/pod-agents-manager.rb
git commit -m "pod-agents-manager"
git push
```

Users then install with:

```bash
brew install robvanvolt/tap/pod-agents-manager
# before the first tagged release, install straight from main:
brew install --HEAD robvanvolt/tap/pod-agents-manager
```

## On each release

The `url`/`sha256` in the formula pin a release tarball, so update them when you
tag a new version:

```bash
# after `git tag v0.6.1 && git push --tags` on the main repo:
VER=0.6.1
curl -fsSL -o /tmp/pam.tgz \
  "https://github.com/robvanvolt/pod-agents-manager/archive/refs/tags/v${VER}.tar.gz"
shasum -a 256 /tmp/pam.tgz      # → paste into the formula's sha256

# in the tap repo, bump `url` (vX.Y.Z) + `sha256`, commit, push.
```

`brew bump-formula-pr` can automate this once the tap is established.

## How the formula works

- Installs the entrypoint (`.pod_agents`) and the whole `.pod_agents_config`
  tree into Homebrew's `libexec` (read-only, replaced on upgrade).
- Puts a thin wrapper on PATH as **`pod-agents`** (not `pod`, to avoid
  shadowing CocoaPods). The wrapper sets `POD_AGENTS_DIST_DIR` to the libexec
  tree and execs the entrypoint.
- The entrypoint runs in *executed* mode: `_pod_bootstrap_from_dist`
  materializes `~/.pod_agents_config` from the libexec tree on first run, and
  refreshes the code dirs (`lib/`, `server/`) when the distribution version
  changes — so `brew upgrade` takes effect — while preserving the user's
  `.env` and custom agents/flavors/volumes.
- Installs bash completion for `pod-agents` (and `pod`).

The curl|bash installer (`install.sh`) and the sourced-function model are
unaffected: the entrypoint only bootstraps when executed with
`POD_AGENTS_DIST_DIR` set.

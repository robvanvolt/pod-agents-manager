# Contributing to pod-agents-manager

Thanks for considering a contribution! This project aims to stay small,
readable, and reliable. Here's everything you need to land a good change.

## Quick start

```bash
git clone https://github.com/robvanvolt/pod-agents-manager.git
cd pod-agents-manager

# Install shellcheck once (used by the test suite)
brew install shellcheck      # macOS
sudo apt install shellcheck  # Debian/Ubuntu

# Run the full test suite — no Podman, no systemd needed
bash tests/run.sh
```

CI runs the same suite on every push and pull request via
[`.github/workflows/tests.yml`](.github/workflows/tests.yml).

## Source layout

```
.pod_agents                              entrypoint — defines the `pod` function
.pod_agents_config/
  ├─ version.conf                        single source of truth for the version
  ├─ lib/NN-*.sh                         numbered modules sourced in order
  ├─ agents/<name>.sh                    pluggable agent definitions
  ├─ flavors/*.containerfile             optional Containerfile snippets
  ├─ volumes/*.volumes                   reusable named-volume bundles
  ├─ skills/<skill>/                     read-only-mounted shared skills
  └─ server/                             Go LAN dashboard
tests/run.sh                             single-file lint + sham test suite
.github/workflows/tests.yml              CI
docs/                                    GitHub Pages site
```

## How the lib loader works (read this before adding a module)

The entrypoint sources every `lib/NN-*.sh` in numeric order from inside the
`pod()` function. Each module ends with the sentinel `return 99`, which the
entrypoint loop interprets as **"fell off the end, continue to the next
module"**. Any other exit status is treated as an explicit early return and
propagates as the result of `pod()`.

This is needed because in Bash, `return` from a sourced file only exits the
`source` command — not the loop in the calling function. The sentinel is the
only reliable way to distinguish "I'm done, hand control back to pod()" from
"keep loading the rest".

When adding a new module:

1. Pick a numeric prefix that orders correctly relative to existing modules
   (the test suite asserts numeric prefixes and that lex order = numeric).
2. Write the body assuming you're inside `pod()` — `local`, positional args,
   and earlier-module variables (`$action`, `$config_dir_root`, etc.) are
   available.
3. End the file with `return 99`.

Helpful conventions:

- Use `_pod_` as the prefix for internal helper functions.
- Put PASS/WARN/FAIL diagnostic output through stderr only when it's an error
  the user must act on; otherwise stdout is fine.
- For diagnostic-only commands (think `pod doctor`), add the action to the
  auto-config skip-list in [`10-env.sh`](.pod_agents_config/lib/10-env.sh) so
  it never triggers an interactive prompt.

## Writing an agent plugin

See the [Agent plugins section in the README](README.md#writing-an-agent-plugin)
for the full template. In short: define `agent_build_containerfile` and
`agent_generate_config`, set a few `AGENT_*` env vars, drop the file into
`.pod_agents_config/agents/`. Auto-discovered the next time you run `pod`.

## Releasing (bumping the version)

`.pod_agents_config/version.conf` is the **single source of truth**. The
README badge, `pod --version`, the dashboard topbar, batch logs, and the
self-update remote-vs-local check all read from this one file.

```bash
# edit version.conf — e.g. 0.2.2n → 0.2.3
echo 'POD_AGENTS_VERSION="0.2.3"' > .pod_agents_config/version.conf
bash tests/run.sh
git commit -am "version 0.2.3" && git push
```

The test suite includes a regression check that `pod --version` matches the
value in `version.conf`, so a future refactor can't silently fall back to the
panic-default in [`.pod_agents`](.pod_agents).

## Pull request checklist

- [ ] `bash tests/run.sh` passes locally
- [ ] No new shellcheck errors (warnings are OK)
- [ ] If you added a `lib/` module, it has a numeric prefix and ends with
      `return 99`
- [ ] If you added a user-facing command, it's wired into the help text in
      [`25-cli-help-sync.sh`](.pod_agents_config/lib/25-cli-help-sync.sh),
      the bash completion line in [`.pod_agents`](.pod_agents:36), and the
      interactive menu in
      [`40-interactive-menu.sh`](.pod_agents_config/lib/40-interactive-menu.sh)
- [ ] The README and `docs/index.html` are updated if behaviour or surface
      area changed
- [ ] Commit messages explain *why*, not just *what*

## Filing issues

Use the templates in `.github/ISSUE_TEMPLATE/`. For bugs, please include:

- `pod doctor` output (great snapshot of host state)
- `pod --version`
- `podman --version`
- The exact command you ran and its full output

## License

By contributing, you agree your code is released under the project's
[Apache 2.0 license](LICENSE).

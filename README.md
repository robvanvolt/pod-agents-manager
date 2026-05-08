<div align="center">

<img src="logo.svg" alt="Pod Agents Manager" width="180" />

# Pod Agents Manager

**A rootless Podman + Quadlet fleet manager for running and orchestrating local AI coding agents.**

[![Version](https://img.shields.io/badge/dynamic/regex?url=https%3A%2F%2Fraw.githubusercontent.com%2Frobvanvolt%2Fpod-agents-manager%2Fmain%2F.pod_agents_config%2Fversion.conf&search=POD_AGENTS_VERSION%3D%22%28%5B%5E%22%5D%2B%29%22&replace=%241&label=version&color=informational)](.pod_agents_config/version.conf)
[![Tests](https://github.com/robvanvolt/pod-agents-manager/actions/workflows/tests.yml/badge.svg)](https://github.com/robvanvolt/pod-agents-manager/actions/workflows/tests.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Docs](https://img.shields.io/badge/docs-GitHub%20Pages-68e1fd.svg)](https://robvanvolt.github.io/pod-agents-manager/)
[![Shell: Bash](https://img.shields.io/badge/Shell-Bash-1f425f.svg)](https://www.gnu.org/software/bash/)
[![Container: Podman](https://img.shields.io/badge/Container-Podman-892ca0.svg)](https://podman.io/)

</div>

---

`pod` turns any Linux box with rootless Podman into a multi-tenant home for local coding agents — Claude Code, OpenCode, Crush, Pi, Hermes, Nanocoder, and anything else you wrap. Each instance lives in its own isolated container with a persistent workspace, talks to your local OpenAI-compatible inference server, and is started, joined, mirrored across `tmux`, batch-prompted, or torn down with one command.

A small Go-backed web dashboard exposes the same control surface over the LAN.

## Highlights

- **Single-file CLI, modular internals.** One `~/.pod_agents` Bash function loads numbered helper modules from `~/.pod_agents_config/lib/*.sh`. No daemons, no extra runtimes — Quadlet generates the systemd units, Podman runs them rootless under your user.
- **Pluggable agents.** Drop a `<name>.sh` into `~/.pod_agents_config/agents/`; it's auto-discovered, gets its own image, and gains a CLI verb. Ships with Claude, OpenCode, Crush, Pi, Hermes, Nanocoder, Little-Coder.
- **Composable Containerfile flavors and bases.** `bun`, `uv`, etc. layer onto the base image automatically. Pick `node:current-alpine` (default, fast) or `node:current-trixie-slim` (Debian, broader compatibility) per pod or via `pod base`.
- **Batch prompting.** `pod batch prompts.txt` fans a list of prompts across every running pod, sequentially or `--concurrent`. Live progress, log tailing in `tmux`, stop/resume per batch.
- **`tmux` grid view.** `pod tmux` opens a tiled grid with one pane per running pod — instant visual telemetry across the fleet.
- **Native LAN dashboard.** `pod server start` runs a small static Go binary on the host (no nested containers, uses host Podman directly). Bound on `0.0.0.0`, prints every reachable IP, exposes JSON APIs for stats, info, action, and create.
- **Skills are first-class.** `~/.pod_agents_config/skills/` is read-only-mounted into every pod at `/srv/skills`, then symlinked into each agent's expected path. Update once, every agent sees it.
- **Self-diagnosing.** `pod doctor` reports podman + systemd readiness, lib/agents/flavors layout, env, port + endpoint reachability — so a misconfigured host fails fast with a clear hint instead of deep inside `pod start`.
- **Persistence done right.** Per-instance workspaces live at `~/Developer/<agent>-pods/<instance>/`. `remove` keeps the data; `delete` wipes it.

## Architecture

```
┌─ host (Debian / Alpine / anything with rootless Podman + systemd) ────────┐
│                                                                            │
│   ~/.pod_agents              ← single-file entrypoint (the `pod` function) │
│   ~/.pod_agents_config/                                                    │
│     ├─ .env                  ← POD_OPENAI_BASE_URL, POD_DEFAULT_MODEL, ... │
│     ├─ version.conf          ← POD_AGENTS_VERSION (single source of truth) │
│     ├─ lib/NN-*.sh           ← numbered modules sourced in order           │
│     ├─ agents/<name>.sh      ← agent plugins (build + config)              │
│     ├─ flavors/*.containerfile   ← optional Containerfile snippets         │
│     ├─ volumes/*.volumes     ← reusable named volume bundles               │
│     ├─ skills/<skill>/       ← shared, read-only-mounted skills            │
│     ├─ batch/<id>/           ← batch state, logs, progress                 │
│     └─ server/               ← Go dashboard (binary runs on host)          │
│                                                                            │
│   ~/Developer/<agent>-pods/<instance>/   ← per-pod persistent workspace    │
│   ~/.config/containers/systemd/<agent>@.container   ← Quadlet units        │
│                                                                            │
│   pod-<agent>-<instance>     ← running rootless container (Podman)         │
│        └─ /workspace ↔ host workspace                                      │
│        └─ /srv/skills (ro)   ↔ host skills dir                             │
│        └─ env: OPENAI_BASE_URL, ANTHROPIC_BASE_URL, LLM, ...               │
└────────────────────────────────────────────────────────────────────────────┘
```

The shell function generates a Quadlet `*.container` template per agent, lets `systemctl --user daemon-reload` materialize it into a transient unit, and starts the pod via `systemctl --user start <agent>@<instance>.service`. Builds are cached at `~/.cache/podman-containers/`; image tags include both flavor and base, so cache hits are exact.

`pod update` rebuilds and restarts agent images. `pod self-update` refreshes the manager itself by downloading the latest repository snapshot and updating files in `~/.pod_agents` and `~/.pod_agents_config/` (your `.env` and any custom plugins are left in place).

## Requirements

| Component | Minimum | Notes |
|---|---|---|
| Linux | any modern distro | tested on Debian 12, Ubuntu 22.04+, Fedora 39+ |
| Podman | 4.4+ recommended, 5.8+ ideal | `pod` masks the buggy `podman-user-wait-network-online.service` on 5.0–5.7 automatically |
| systemd (user) | yes | rootless Podman uses `systemctl --user` and Quadlet |
| Bash | 4+ on the runtime host | the lint/test suite runs on 3.2+ for macOS dev machines |
| `tmux` | optional | needed for `pod tmux` and `pod batch tmux` |
| `go` | not required on host | dashboard binary is built in a transient `golang:alpine` builder if Go is missing |

An OpenAI-compatible inference endpoint is what each agent talks to. On first `pod` start, missing `POD_*` values are prompted once and saved to `~/.pod_agents_config/.env`; later changes go through `pod config` or by editing that file.

## Installation

```bash
curl -fsSL https://raw.githubusercontent.com/robvanvolt/pod-agents-manager/main/install.sh | bash
exec bash -l

pod doctor          # verify the host is ready
pod start pi dev    # first start prompts once for missing POD_* values
pod prebuild        # (optional) prebuild every agent's image
```

Manual install from a clone:

```bash
git clone https://github.com/robvanvolt/pod-agents-manager.git
cd pod-agents-manager
bash ./install.sh
exec bash -l
```

Development channel install from the `dev` branch:

```bash
curl -fsSL https://raw.githubusercontent.com/robvanvolt/pod-agents-manager/dev/install-dev.sh | bash
exec bash -l

pod doctor
```

Use the dev channel for testing the next release before it is promoted to
`main`. It writes the same `~/.pod_agents` and `~/.pod_agents_config` managed
files as the regular installer, but fetches `POD_AGENTS_REF=dev`.

Tab-completion is registered automatically. Type `pod ` and hit `<Tab>`.

Upgrade later without touching your `.env` or custom plugins:

```bash
pod self-update
```

## Quickstart

```bash
pod start pi dev                      # spin up a Pi agent on the alpine base
pod join pi dev                       # join its tmux session
pod tmux                              # watch every running pod side-by-side
pod server start                      # bring up the LAN dashboard (0.0.0.0:1337)
pod batch prompts.txt                 # fan prompts across every running pod
pod batch pi prompts.txt --concurrent # …or only across `pi` pods, in parallel
pod stop pi dev                       # stop (keeps the workspace)
pod delete pi dev                     # stop + remove the workspace
```

Run `pod` with no args for an interactive menu.

## Command reference

```
Lifecycle      pod start | stop | restart | status | stats
               pod remove | delete | remove-all | delete-all
Images         pod prebuild [agent] [flavor] [volumes] [base]
               pod update   [agent] [instance]
               pod self-update | cache-clean
Interaction    pod join | enter | it [agent] [instance]
               pod config | tmux [instance]
Batch          pod batch [agent [instance]] <prompts.txt> [--concurrent]
               pod batch tmux | stats | list | stop [id]
Dashboard      pod server start | stop | restart | status | logs | build
Diagnostics    pod doctor
Defaults       pod base <alpine|trixie-slim|...>
```

Every action accepts the same positional contract:

```
pod <action> [agent] [instance] [flavor] [volumes] [base]
```

Anything past `<action>` is optional; the interactive menu prompts for what's missing.

## Writing an agent plugin

Each file in `~/.pod_agents_config/agents/<name>.sh` defines two functions and a few env vars:

```bash
# ~/.pod_agents_config/agents/my-agent.sh

AGENT_VOLUME_CONFIG_PATH="/root/.config/my-agent"
AGENT_SKILLS_SUBPATH="agent/skills"           # optional
AGENT_BATCH_INVOKE='my-agent --print "$PROMPT"' # optional

agent_build_containerfile() {
    local build_dir="$1"; local flavor="$2"; local base="$3"
    write_base_node_containerfile "$build_dir" "$flavor" "$base"
    cat <<'EOF' >> "$build_dir/Containerfile"
RUN npm install -g my-agent && npm cache clean --force
CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"; local action="$2"
    [ "$action" = "update" ] && return 0
    cat <<EOF > "$config_dir/config.json"
{ "baseUrl": "$OPENAI_BASE_URL", "apiKey": "$OPENAI_API_KEY", "model": "$DEFAULT_MODEL" }
EOF
}

# Optional: runs once per `pod update` cycle (e.g. to pull a base image)
agent_pre_update() { podman pull docker.io/myorg/my-agent:latest; }
```

Auto-discovered the next time you run `pod`. No restart, no registry, no boilerplate.

## The dashboard

`pod server start` builds a static Go binary (in a throwaway `golang:alpine` builder if your host has no Go), then runs it natively on the host so it talks to your real Podman directly — no podman-in-podman, no socket bind-mounts, no UID gymnastics.

| Route | Purpose |
|---|---|
| `GET /` | Single-page dashboard |
| `GET /api/stats` | Cached `podman stats --all --no-stream` JSON, refreshed every 3s |
| `GET /api/info` | Hostname, LAN IPs, server time |
| `GET /api/agents` | Available agents, flavors, volumes, bases |
| `POST /api/action` | `start \| stop \| restart \| delete \| remove` an existing pod |
| `POST /api/create` | Create a brand-new pod from agent + instance + flavor + volumes + base |

`GET /api/stats` also adds `ActivityState` and `ActivityDetail` to managed pods
so the dashboard can show whether an agent looks idle or busy. The first-pass
heuristic checks CPU activity and the foreground tmux command in the pod's
`bot` session.

All identifiers are validated, ops are whitelisted, ANSI escapes are stripped on the way out. `start` prints every reachable LAN URL so you can hand the link to a teammate.

## Batch processing

`pod batch` fans a prompt list across the fleet:

```bash
pod batch prompts.txt                       # every running pod
pod batch pi prompts.txt                    # only `pi` pods, sequentially
pod batch pi dev prompts.txt --concurrent   # one pod, all prompts in parallel
pod batch tmux                              # live log per active runner
pod batch stats                             # progress + status per runner
pod batch list                              # batch ids
pod batch stop <id>                         # SIGTERM all runners for a batch
```

State lives at `~/.pod_agents_config/batch/<id>/` (input copy, meta, runners, pids, per-pod progress + logs, completion markers). Runners are detached with `nohup` and survive the parent shell exiting.

## Roadmap

Pod Agents Manager is moving toward a small, reliable orchestration layer for
local agent fleets: still shell-native, still rootless-first, but much better at
showing what agents are doing and letting you steer them from the dashboard.

### 0.3 — dashboard awareness and notification foundations

- Show whether each pod appears **idle**, **running**, or **unknown** in the LAN dashboard.
- Add a normalized pod API with agent, instance, image, ports, workspace path, service state, container state, activity state, and last refresh time.
- Add a server-sent events stream so the dashboard can update without constant polling.
- Add basic write protection for dashboard actions before expanding the API surface.
- Prepare PWA notification support: browser subscription storage, test notifications, and a notification-ready event model.
- Add the first question/instruction APIs:
  - ask a multiple-choice question such as “Should the app be red or blue?”
  - collect the answer from a notification-enabled dashboard/PWA
  - queue new instructions for an idle pod
- Test release candidates through the `dev` channel on a disposable Linux host before promoting to `main`.

### 0.4 — agent inbox and human-in-the-loop workflows

- Add an instruction inbox per pod, backed by simple local files first.
- Add CLI commands such as `pod inbox`, `pod ask`, and `pod instruct`.
- Let the dashboard send follow-up instructions to idle pods.
- Add notification rules for “pod became idle”, “batch completed”, “agent asks a question”, and “pod failed”.
- Add a batch dashboard with progress, ETA, logs, stop controls, and result summaries.
- Support per-pod notes, tags, and favorite workspaces.

### 0.5 — safer multi-user and remote operations

- Add passkey-native dashboard login with SimpleWebAuthn:
  - `@simplewebauthn/browser` in the dashboard/PWA
  - `@simplewebauthn/server` for registration and authentication verification
  - local credential storage under `~/.pod_agents_config/server/`
  - no external identity provider required
- Keep `pod server token rotate` as a recovery/bootstrap path for headless hosts and first-time passkey setup.
- Add read-only and operator modes for LAN sharing.
- Add exportable diagnostics bundles for bug reports.
- Add stronger `pod status --json` and `pod doctor --json` APIs.
- Improve update/release tooling: changelog generation, preflight checks, and rollback hints.
- Add more agent plugin examples and a compatibility matrix for common local inference servers.

### 0.6 — broader container backend support

- Explore Docker support as an optional backend while keeping Podman + Quadlet as the primary, best-supported path.
- Define a backend interface for lifecycle, stats, logs, exec, port publishing, and volume mounts.
- Support Docker Compose or systemd-managed Docker units only if they can preserve the project’s core goals: per-pod isolation, persistent workspaces, simple upgrades, and clear diagnostics.
- Document feature differences between Podman and Docker instead of pretending every backend behaves identically.

### Later ideas

- Mobile-first PWA polish: install prompt, offline shell, notification center, and quick actions.
- Policy presets for model, endpoint, permissions, workspace mount mode, and allowed tools.
- Pod templates for common workflows such as web app coding, repo triage, research, and batch refactors.
- Lightweight scheduling: start pods, run batches, or send prompts at planned times.
- Optional metrics history so the dashboard can show agent activity over time.
- Plugin registry conventions for community agents, flavors, volume bundles, and skills.

## Development & contributing

Source layout:

- `.pod_agents` — entrypoint that defines the `pod` function and sources the modules in numeric order.
- `.pod_agents_config/lib/NN-*.sh` — numbered helper modules (env, build/pick, help/sync, early-flags, interactive menu, arg-parse, doctor, server, batch, lifecycle).
- `.pod_agents_config/agents/`, `flavors/`, `volumes/`, `skills/`, `server/` — pluggable extension points.
- `tests/run.sh` — single-file lint + sham-test runner.
- `.github/workflows/tests.yml` — runs the suite on every push and PR.

Run the suite locally (no Podman or systemd needed — the tests sandbox a fake `$HOME`):

```bash
bash tests/run.sh
```

The suite covers `bash -n` syntax, `shellcheck` errors, the lib loader contract (numeric prefixes + sentinel exit codes), install/self-update regression guards, sandboxed smoke tests for `--help` / `--version` / `doctor`, helper-function unit tests, and a regression check that `pod --version` matches `version.conf`.

The proposed 0.3 plan lives in [docs/ROADMAP-0.3.md](docs/ROADMAP-0.3.md).

**Releasing.** `.pod_agents_config/version.conf` is the single source of truth for the version. Bumping it (e.g. `0.2.2n` → `0.2.2o`), committing, and pushing is the entire release flow — the version badge in this README is read live from that file, and the test suite asserts `pod --version` agrees with it.

**Branching.** Keep `main` as the stable install channel, use `dev` for small
release-prep changes, and open named feature branches for larger work such as
PWA notifications or new orchestration APIs. Merge large branches into `dev`
first, test on a disposable Linux host, then fast-forward or PR `dev` into
`main` for release.

PRs welcome for additional flavors, agents, skills, and bug fixes.

## License

Licensed under the Apache License, Version 2.0 — see [LICENSE](LICENSE).

## Acknowledgements

- [Podman](https://podman.io/) and [Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html) — rootless, daemonless, systemd-native containers.
- The agent CLIs themselves: [Claude Code](https://docs.claude.com/en/docs/claude-code/overview), [OpenCode](https://github.com/opencode-ai/opencode), [Crush](https://github.com/charmbracelet/crush), [Pi](https://github.com/mariozechner/pi-coding-agent), [Hermes](https://nousresearch.com/), [Nanocoder](https://github.com/Nano-Collective/nanocoder), [Little-Coder](https://github.com/itayinbarr/little-coder).
- Local-inference projects that made running these agents on your own hardware viable: [llama.cpp](https://github.com/ggerganov/llama.cpp), [vLLM](https://github.com/vllm-project/vllm), [LM Studio](https://lmstudio.ai/), [Ollama](https://ollama.com/).

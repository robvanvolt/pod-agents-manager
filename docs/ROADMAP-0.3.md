# Pod Agents Manager Roadplan

This file used to track the 0.3 dashboard-awareness release. The project has
moved on: the `dev` branch currently declares `POD_AGENTS_VERSION="0.5.1"`, so
this is now the living roadplan for the next work.

## Current status

- Refresh context: this update started from a clean `dev` branch tracking
  `origin/dev`.
- Current version source: `.pod_agents_config/version.conf`.
- Shipped agent plugins: Claude Code, Codex, Command Code, OpenCode, Crush, Pi,
  Hermes, and Nanocoder.
- Current dashboard surface: stats, info, auth, passkeys, terminal overlay,
  inbox/instruct, lifecycle actions, pod creation, and sham `/v1/*` endpoints.
- Current CLI surface: lifecycle, build/update, `tmux`, batch, inbox, ask,
  instruct, server auth/token helpers, doctor, and agent sham testing.
- Current 0.6 progress: dashboard audit writes now rotate to timestamped
  `audit.*.jsonl` archives with configurable size and retention limits.

## Shipped milestones

### 0.3: dashboard awareness

- Stable `dev` channel and `install-dev.sh`.
- CI on `main` and `dev`.
- Host-native Go dashboard launched by `pod server start`.
- Activity enrichment on `GET /api/stats` through `ActivityState` and
  `ActivityDetail`.
- Activity heuristics that combine CPU, tmux foreground command, and recent
  pane output so low-CPU agents waiting at prompts read as idle.
- Basic server tests and docs for the release process.

### 0.4: human-in-the-loop queueing

- Local per-pod JSONL inbox under
  `~/.pod_agents_config/inbox/<agent>-<instance>.jsonl`.
- CLI commands: `pod inbox`, `pod instruct`, and `pod ask`.
- Dashboard instruction queueing.
- REST-shaped instruction endpoint:
  `POST /api/pods/{agent}/{instance}/instructions`.
- Shared queue model that does not require a database or always-on worker.

### 0.5: safe LAN sharing

- Viewer/operator roles for the dashboard.
- Bootstrap-token login through `pod server token rotate`.
- HttpOnly operator sessions.
- Local passkey registration and login using vendored
  `@simplewebauthn/browser` assets and native Go ES256 verification.
- Same-origin protection for dashboard writes.
- Rate limiting for token and passkey login attempts.
- Conditional secure cookies for TLS or `POD_SERVER_FORCE_SECURE_COOKIE=1`.
- Local audit log at `~/.pod_agents_config/server/audit.jsonl`.

### 0.6 started: audit retention

- Automatic audit rotation when `audit.jsonl` reaches
  `POD_SERVER_AUDIT_MAX_BYTES` (default: 1 MiB).
- Timestamped archive files under `~/.pod_agents_config/server/audit.*.jsonl`.
- Archive pruning controlled by `POD_SERVER_AUDIT_MAX_ARCHIVES` (default: 5).

### 0.5.1: agent smoke testing

- Built-in sham OpenAI-compatible endpoints:
  `GET /v1/models`, `POST /v1/chat/completions`, and `POST /v1/completions`.
- Built-in sham Anthropic-compatible endpoint: `POST /v1/messages`.
- `pod test <agent>` and `pod test --all`.
- Dedicated `<agent>-shamtest` pods for fixture-backed CLI smoke tests.
- Agent invocation fixes for the current plugin set.

## Next: 0.6 polish and demoability

0.6 should make the shipped core easier to demo, operate, and trust from a
phone or LAN browser.

Must-have:

- Batch dashboard page with progress, ETA, logs, stop controls, and result
  summaries.
- Dashboard log/journal viewer for `journalctl --user -u <pod>.service`.
- Passkey management UI for deleting and renaming local credentials.
- HTTPS/reverse-proxy docs, including `POD_SERVER_FORCE_SECURE_COOKIE=1`.
- Dashboard install/update card showing local version, active channel, and
  latest `main`/`dev` version.
- Release checklist that includes README, GitHub Pages, roadplan, shell tests,
  Go tests, and a disposable Linux host verification pass.

Nice-to-have:

- Pod templates for common workflows such as web-app coding, repo triage,
  research, and batch refactors.
- Mobile-first PWA shell: install prompt, offline app shell, quick actions, and
  notification center layout.
- Notification foundation: browser subscription storage, test notification
  delivery, and local rule definitions for "pod became idle", "batch completed",
  "agent asks a question", and "pod failed".
- Per-pod notes, tags, and favorite workspaces stored beside each workspace.
- Per-pod resource limits surfaced through the CLI and dashboard create form.

## 0.7 benchmark suite and agent comparison

0.7 should turn the pluggable-agent story into reproducible comparisons.

- Pluggable benchmark tasks under `~/.pod_agents_config/benchmarks/<name>/`.
- Canonical starter task: "write a single-file portfolio app in one HTML file."
- Configurable judge LLM through `POD_BENCH_JUDGE_MODEL` and
  `POD_BENCH_JUDGE_BASE_URL`.
- `pod bench run <task>` fan-out using existing batch infrastructure.
- Metrics: wall-clock time, cached/new input tokens, output tokens, judge score,
  and judge rationale.
- CLI: `pod bench list`, `pod bench run <task> [agent]`,
  `pod bench results <task>`, and `pod bench export <task>`.
- Dashboard comparison table sortable by score, time, and token use.
- Results stored under `~/.pod_agents_config/benchmarks/<task>/runs/<id>/`.

## 1.0 stable public release

1.0 is consolidation. The project should not reach it by adding a large new
subsystem at the last minute.

- Stable CLI surface:
  `pod <action> [agent] [instance] [flavor] [volumes] [base]`.
- Stable plugin contract around `agent_build_containerfile`,
  `agent_generate_config`, and `AGENT_*` env vars.
- Stable on-disk layout under `~/.pod_agents_config/` and
  `~/Developer/<agent>-pods/<instance>/`.
- JSON output for external tooling: at minimum `pod status --json` and
  `pod doctor --json`.
- Redacted diagnostics bundle through `pod doctor --bundle`.
- Documented upgrade path from any 0.x release to 1.0.
- Demo assets: dashboard GIF, `tmux` grid GIF, batch-run GIF, sample prompts,
  and a reproducible Friday-refactor showcase.
- Public benchmark scorecard from 0.7 covering at least three agents on the
  canonical task.

## Post-1.0

- Inter-pod messaging through structured inbox entries.
- Metrics history for activity, CPU, memory, and state changes over time.
- Plugin registry conventions for community agents, flavors, volume bundles,
  and skills.
- Compatibility matrix for llama.cpp, vLLM, LM Studio, Ollama, and other
  OpenAI-compatible local servers.
- Release tooling improvements: changelog generation, preflight checks, and
  rollback hints.

## Non-goals

- No Docker backend. Rootless Podman plus Quadlet is the identity of the
  project.
- No built-in scheduler. Cron, systemd timers, and `at` already exist.
- No remote control plane. This is a single-host fleet manager.

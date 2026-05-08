# Pod Agents Manager 0.3 Roadmap

0.3 should be a dashboard and orchestration maturity release: the CLI already
does useful fleet work, so the next step is making pods easier to observe,
interrupt, and steer from a phone or LAN browser.

## Release goals

- Stable `dev` channel for release candidates and daily testing.
- Dashboard shows whether each managed pod appears idle or busy.
- Notification-ready server architecture without requiring a daemon rewrite.
- Safer LAN control surface before browser notifications and instructions land.
- Stronger docs, contribution flow, and release checklist.

## Already shipped on `dev`

The `dev` channel, `install-dev.sh`, CI on both branches, and the first pass of
dashboard `ActivityState` / `ActivityDetail` enrichment are already in. What
follows is the work still needed to cut 0.3.0.

## Non-goals for 0.3

These are deliberately deferred so 0.3 stays shippable:

- **No passkey login yet.** Token-based write protection is enough for 0.3; passkeys land in 0.5 (see `README.md` roadmap).
- **No CLI inbox commands yet.** The `POST /api/pods/.../instructions` endpoint goes in, but `pod inbox` / `pod ask` / `pod instruct` are 0.4 work.
- **No notification delivery.** 0.3 only stores subscriptions and exposes the question/instruction APIs. Web Push delivery rules ("pod became idle", "batch completed") are 0.4.
- **No Docker backend, no scheduling, no remote control plane.** See the README "Non-goals" section.

## Must-have for 0.3.0

- Add token-based write protection for `POST /api/action` and `POST /api/create`.
- Add a read-only `GET /api/pods` endpoint that normalizes pod name, agent,
  instance, service state, container state, image, ports, workspace path,
  activity state, and last refresh time.
- Add an activity probe contract:
  - `idle`: pod is running, but the agent tmux pane is at a shell or no active
    bot session exists.
  - `running`: pod has meaningful CPU activity or a non-shell foreground
    command in the agent tmux pane.
  - `unknown`: the dashboard cannot probe the pod quickly.
- Add dashboard filtering by agent, instance, status, and activity.
- Add a server-side event stream, `GET /api/events`, for dashboard updates.
- Add basic server tests for identifier validation, activity normalization, and
  read-only API JSON shape.
- Test the release candidate only on the disposable `ssh nuc` host.

## Notification foundation

Use a feature branch for this work, for example `feature/pwa-notifications`.
The first notification-capable API can stay local-file backed:

- `POST /api/notifications/subscribe`
  - Stores a browser Web Push subscription under
    `~/.pod_agents_config/server/subscriptions.json`.
- `POST /api/notifications/test`
  - Sends a test notification to enabled browsers.
- `POST /api/questions`
  - Creates a multiple-choice question with title, body, options, target pod,
    and optional timeout.
- `GET /api/questions/pending`
  - Returns unanswered questions for the dashboard/PWA.
- `POST /api/questions/{id}/answer`
  - Stores the selected answer and, when appropriate, queues an instruction for
    a pod.
- `POST /api/pods/{agent}/{instance}/instructions`
  - Queues a new instruction for an idle pod.

The instruction queue can start as JSONL files under
`~/.pod_agents_config/inbox/<agent>-<instance>.jsonl`. The CLI can later grow
`pod inbox`, `pod ask`, and `pod instruct` commands that use the same files.

## Nice-to-have

- Passkey-native dashboard login in the 0.5 line, using
  `@simplewebauthn/browser` in the dashboard/PWA and
  `@simplewebauthn/server` for registration/authentication verification, with
  credentials stored locally under `~/.pod_agents_config/server/` and no
  external identity provider required.
- `pod server token rotate` as a recovery/bootstrap path for headless hosts and
  first-time passkey setup.
- `pod status --json` so the Go dashboard does not need to infer everything
  from Podman output.
- Batch dashboard page with progress, logs, ETA, and stop controls.
- Per-pod notes/tags stored beside each workspace.
- Dashboard install/update card showing local version versus latest main/dev.
- Release notes generated from commits since the last version tag.

## Release checklist

- Merge feature branches into `dev`.
- Run `bash tests/run.sh`.
- Install the dev channel on `ssh nuc`.
- On `ssh nuc`, verify `pod doctor`, `pod server start`, pod creation, start,
  stop, restart, delete, and dashboard activity states.
- Update README, GitHub Pages, and this roadmap.
- Bump `.pod_agents_config/version.conf` to `0.3.0`.
- Merge `dev` to `main`.

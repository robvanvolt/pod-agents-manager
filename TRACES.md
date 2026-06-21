# Agent Traces & History Formats

How each shipped agent persists its conversation history (the "trace") — where
on disk it lands, in what format, and how user prompts, thinking/reasoning,
tool calls, and tool results are encoded.

## How this was measured

Every agent was run on a real pod (rootless podman 5.8.2) against the default
local model in `.env` — `Qwen3.6-35B-A3B-8bit`, a Qwen3 thinking model that
emits `<think>` reasoning natively — with one identical prompt that exercises
**thinking + a tool call**:

> *Roll a 20-sided dice for me using the roll-dice skill, then tell me the result.*

The `roll-dice` skill resolves to a single bash call (`echo $((RANDOM % 20 + 1))`),
so each trace contains: a user prompt, one or more reasoning blocks, a tool
call, a tool result, and a final assistant message. The snippets below are
verbatim from those runs.

## Where traces live on the host

Two pod directories are bind-mounted from the host and therefore **survive pod
stop/restart/recreate**:

| In-container path                 | Host path                                          |
| --------------------------------- | -------------------------------------------------- |
| `/workspace`                      | `~/Developer/<agent>-pods/<instance>/workspace/`   |
| `$AGENT_VOLUME_CONFIG_PATH`       | `~/Developer/<agent>-pods/<instance>/config/`      |

Anything an agent writes **outside** those two paths (e.g. under
`/root/.local/share/…`) lives only in the container's ephemeral layer and is
**lost when the pod is recreated** (Quadlet rebuilds the container from the
image on each `pod start`). This matters for **opencode** — see below.

## Summary

| Agent          | Persists trace? | Format                  | Trace path (in container)                                              | Survives recreate? |
| -------------- | --------------- | ----------------------- | --------------------------------------------------------------------- | ------------------ |
| **claude**     | ✅              | JSONL (one event/line)  | `/root/.claude/projects/<dir-slug>/<uuid>.jsonl`                       | ✅ (config mount)  |
| **codex**      | ✅              | JSONL rollout           | `/root/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`           | ✅ (config mount)  |
| **crush**      | ✅              | SQLite                  | `/workspace/.crush/crush.db` (table `messages`)                       | ✅ (workspace)     |
| **opencode**   | ✅              | SQLite (Drizzle, WAL)   | `/root/.local/share/opencode/opencode.db` (tables `message`/`part`)   | ⚠️ **ephemeral**   |
| **pi**         | ✅              | JSONL (one event/line)  | `/root/.pi/agent/sessions/<dir-slug>/<ts>_<uuid>.jsonl`               | ✅ (config mount)  |
| **hermes**     | ✅              | single JSON document    | `/opt/data/sessions/session_<id>.json`                                | ✅ (config mount)  |
| **nanocoder**  | ❌ memory-only  | —                       | (no conversation persisted; history file is `{}`)                     | —                  |
| **command-code** | 🔒 needs login | unknown                | requires `cmd login` (cloud auth) — not traceable headless            | —                  |
| **antigravity**  | 🔒 needs login | unknown                | requires Google Sign-In — not traceable headless                     | —                  |

---

## claude

- **Path:** `/root/.claude/projects/<dir-slug>/<session-uuid>.jsonl`
  (the cwd is slugified: `/workspace` → `-workspace`). Config mount → persisted.
- **Format:** JSONL, one event per line. Each line has a top-level `type`.
  Observed types: `user`, `assistant`, `attachment`, `last-prompt`,
  `queue-operation`. Conversation turns are `user` / `assistant`, each wrapping
  an Anthropic-style `message` object whose `content` is an array of typed blocks.

| Element       | Encoding |
| ------------- | -------- |
| User prompt   | `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]}}` |
| Thinking      | `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"\nThe user wants me to roll a 20-sided die…\n"}]}}` |
| Tool call     | `{"type":"tool_use","name":"Bash","input":{"command":"echo $((RANDOM % 20 + 1))","description":"Roll a 20-sided die"}}` (also a `Skill` tool: `{"name":"Skill","input":{"skill":"roll-dice","args":"d20"}}`) |
| Tool result   | `{"type":"user","message":{"content":[{"type":"tool_result","content":"2"}]}}` |

Thinking is stored as structured `thinking` blocks (the model's `<think>` output
is parsed out of the text, not kept as raw tags).

---

## codex

- **Path:** `/root/.codex/sessions/YYYY/MM/DD/rollout-<ISO-ts>-<uuid>.jsonl`
  (date-partitioned dirs). Config mount → persisted. Also writes
  `state_*.sqlite`, `goals_*.sqlite`, `memories_*.sqlite` alongside.
- **Format:** JSONL. Every line is `{"timestamp","type","payload"}`. The
  `type` is the **envelope category**; `payload.type` is the specific record.
  Codex stores **two parallel streams**: a high-level UI event log (`event_msg`)
  **and** the raw model API records (`response_item`).

| Envelope `type`   | `payload.type` values |
| ----------------- | --------------------- |
| `session_meta`    | (session id, cwd, cli_version, model_provider…) |
| `turn_context`    | (cwd, approval_policy, sandbox_policy…) |
| `event_msg`       | `task_started`, `user_message`, `agent_reasoning`, `agent_message`, `token_count`, `task_complete` |
| `response_item`   | `message`, `reasoning`, `function_call`, `function_call_output` |

| Element       | Encoding |
| ------------- | -------- |
| User prompt   | `event_msg`: `{"payload":{"type":"user_message","message":"Roll a 20-sided dice…"}}` — and `response_item`: `{"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"…"}]}}` |
| Thinking      | `event_msg`: `{"payload":{"type":"agent_reasoning","text":"\nThe user wants me to roll…\n"}}` — and `response_item`: `{"payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"…"}],"encrypted_content":"…"}}` |
| Tool call     | `{"payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\": \"cat /srv/skills/roll-dice/SKILL.md\"}","call_id":"call_ccebab98"}}` |
| Tool result   | `{"payload":{"type":"function_call_output","call_id":"call_ccebab98","output":"…\nProcess exited with code 0\nOutput:\n…"}}` |

> **Config note:** codex ≥ 0.140 dropped the old `[profiles.*]` / `profile = "local"`
> scheme. The agent plugin now binds the local provider via top-level
> `model` + `model_provider` keys (no `--profile`).

---

## crush

- **Path:** `/workspace/.crush/crush.db` — a SQLite database in the **workspace**
  (not the config dir), so it lives next to the project. Also
  `/root/.local/share/crush/projects.json` (project registry, ephemeral).
- **Format:** SQLite. Key tables: `sessions` (one row per conversation) and
  `messages`. Each `messages` row has `role` (`user` / `assistant` / `tool`)
  and a `parts` column = a JSON **array of typed blocks**.

`parts` block types (`{"type":…,"data":{…}}`):

| Element       | Encoding |
| ------------- | -------- |
| User prompt   | `{"type":"text","data":{"text":"Roll a 20-sided dice…"}}` |
| Thinking      | `{"type":"reasoning","data":{"thinking":"\nThe user wants…\n","signature":"","started_at":…,"finished_at":…}}` |
| Tool call     | `{"type":"tool_call","data":{"id":"call_9f32ab46","name":"bash","input":"{\"command\": \"echo $((RANDOM % 20 + 1))\"}","finished":true}}` (input is a JSON **string**) |
| Tool result   | `{"type":"tool_result","data":{"tool_call_id":"call_9f32ab46","name":"bash","content":"20\n\n\n<cwd>/workspace</cwd>","metadata":"{…}"}}` (role = `tool`) |
| Turn boundary | `{"type":"finish","data":{"reason":"end_turn","time":…}}` |

Inspect with: `sqlite3 /workspace/.crush/crush.db 'select role, parts from messages'`.

---

## opencode

- **Path:** `/root/.local/share/opencode/opencode.db` — SQLite via the Drizzle
  ORM, **WAL mode** (copy `opencode.db-wal` + `opencode.db-shm` alongside it or
  you'll see an empty DB). Also `storage/session_diff/ses_*.json`.
- ⚠️ **Not persisted across pod recreate** — this path is under
  `/root/.local/share`, not the bind-mounted config dir (`/root/.config/opencode`).
  History is lost when the pod is rebuilt.
- **Format:** three tables model the conversation:
  - `session` — id, directory, title, version…
  - `message` — `id`, `session_id`, `data` (JSON envelope: `role`, `model`,
    `tokens`, `finish`).
  - `part` — `id`, `message_id`, `session_id`, `data` (JSON typed block).

`part.data` block types:

| Element       | Encoding |
| ------------- | -------- |
| User prompt   | `{"type":"text","text":"\"Roll a 20-sided dice…\"\n"}` |
| Thinking      | `{"type":"reasoning","text":"\nThe user wants to roll a 20-sided dice…\n","time":{"start":…,"end":…}}` |
| Tool call+result | `{"type":"tool","tool":"bash","callID":"call_…","state":{"status":"completed","input":{…},"output":"…","metadata":{…}}}` (call and result are one combined part) |
| Step markers  | `{"type":"step-start"}` / `{"type":"step-finish"}` |

`message.data` envelope example (assistant): `{"role":"assistant","agent":"build","tokens":{"total":11201,"input":11149,"output":52,"reasoning":0},"modelID":"Qwen3.6-35B-A3B-8bit","providerID":"rms","finish":"tool-calls"}`.

---

## pi

- **Path:** `/root/.pi/agent/sessions/<dir-slug>/<ts>_<uuid>.jsonl`
  (cwd slugified: `/workspace` → `--workspace--`). Config mount → persisted.
- **Format:** JSONL, one event per line, each with a top-level `type`. Event
  types: `session` (header: cwd, id, version), `model_change`,
  `thinking_level_change`, then `message`. A `message` event wraps
  `{"role","content":[blocks]}`.

| Element       | Encoding |
| ------------- | -------- |
| User prompt   | `message` → `{"role":"user","content":[{"type":"text","text":"Roll a 20-sided dice…"}]}` |
| Thinking      | `{"type":"thinking","thinking":"\nThe user wants to roll…\n","thinkingSignature":"reasoning_content"}` |
| Tool call     | `{"type":"toolCall","id":"call_5972d6db","name":"bash","arguments":{"command":"echo $((RANDOM % 20 + 1))"}}` |
| Tool result   | `message` with `role:"toolResult"` → `content:[{"type":"text","text":"11\n"}]` |

Assistant turns interleave `thinking` + `text` + `toolCall` blocks in one message.

---

## hermes

- **Path:** `/opt/data/sessions/session_<id>.json` — one JSON document per
  session (`/opt/data` is hermes's config mount → persisted). Also
  `/opt/data/state.db` (SQLite state) and `/opt/data/logs/agent.log`.
  (The `~/.local/.../tirith/*` files are hermes's security sidecar, not the trace.)
- **Format:** a single JSON object with metadata + a `messages` array:
  `{"session_id","model","base_url","system_prompt","tools","message_count","messages":[…]}`.

| Element       | Encoding |
| ------------- | -------- |
| User prompt   | `{"role":"user","content":"Roll a 20-sided dice…"}` |
| Thinking      | on assistant messages, in **both** `reasoning` and `reasoning_content`: `{"role":"assistant","content":"","reasoning":"\nThe user wants…\n","reasoning_content":"…","finish_reason":"tool_calls","tool_calls":[…]}` |
| Tool call     | OpenAI-style `tool_calls` array on the assistant message |
| Tool result   | `{"role":"tool","name":"terminal","content":"{\"output\": \"15\", \"exit_code\": 0}","tool_call_id":"call_acd3f761"}` (content is a JSON string) |

This is the closest to a raw OpenAI chat-completions message log of all the agents.

---

## nanocoder — no persistent trace

nanocoder keeps the conversation **in memory only**. After a headless
`nanocoder run`, the on-disk files are empty:

- `/root/.config/nanocoder/.nano-coder-history` → `{}` (readline history, not conversation)
- `/workspace/.nanocoder/tasks.json` → `[]` (task list, not conversation)

There is no conversation/trace file to inspect. (Headless tool execution also
requires `--mode yolo` or `--mode auto-accept`; without it nanocoder stops at
"Tool approval required".)

---

## command-code & antigravity — cloud-tied, not traceable headless

Both require an interactive cloud sign-in before they will run a prompt, so no
local trace can be produced non-interactively:

- **command-code** (`cmd`): `cmd --print …` returns
  *"Not authenticated. Please run `cmd login` first."* Config dir:
  `/root/.commandcode` (`updates.json` is written, but no session without auth).
- **antigravity** (`agy`): prints a Google OAuth device-code URL and waits.
  Config dir: `/root/.config/antigravity`. Runs Google's cloud models, not the
  local `.env` server.

Once signed in interactively (`pod join <agent> <instance>`), they should write
sessions under their respective config dirs, but the format is not documented
here because it couldn't be captured against the local model.

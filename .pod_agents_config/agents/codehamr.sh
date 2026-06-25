# codehamr — https://github.com/codehamr/codehamr · https://codehamr.com
#
# A minimal, local-first terminal coding agent built specifically for local
# LLMs: a single static Go binary that runs a tool loop until the task is done.
# Deliberately tiny — "no router, no sub-agents, no skill system, no MCP".
#
# Install: curl -fsSL https://codehamr.com/install.sh | PREFIX=/usr/local bash
# Binary:  codehamr   (static Go binary — runs on Alpine, no glibc needed)
#
# ── Interactive only ─────────────────────────────────────────────────────────
# codehamr is a TUI. Its only CLI surface is bare `codehamr` (interactive) and
# `codehamr --version` — there is NO headless / one-shot / batch mode. So this
# agent has no AGENT_BATCH_INVOKE: `pod batch codehamr` / `pod test codehamr`
# won't work. Use it interactively:
#     pod join codehamr <instance>
# Inside the TUI: /models to list/switch models, /clear to reset, ctrl+d quits.
# ─────────────────────────────────────────────────────────────────────────────
#
# Local-LLM routing:
#   codehamr reads `.codehamr/config.yaml` RELATIVE TO THE CWD. Pods run in
#   /workspace, so we point this agent's config-dir bind-mount at
#   /workspace/.codehamr (see AGENT_VOLUME_CONFIG_PATH) and have
#   agent_generate_config write a `local` profile there. The config persists
#   across restarts (it's the config mount) and codehamr finds it on launch.
#
#   codehamr appends `/v1/chat/completions` + `/v1/models` to the profile's
#   `url`, so the url must be the base WITHOUT the trailing /v1 — we strip it
#   from $OPENAI_BASE_URL. Auth is `Authorization: Bearer <key>`.

# codehamr's per-project config dir, which (because pods cd to /workspace) is
# exactly where codehamr looks. Bind-mounting the config volume here makes the
# generated config.yaml both discoverable and persistent.
AGENT_VOLUME_CONFIG_PATH="/workspace/.codehamr"

# No AGENT_BATCH_INVOKE — codehamr has no non-interactive mode (see header).

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"

    # Static Go binary → the default alpine base is fine.
    write_base_node_containerfile "$build_dir" "$flavor"
    cat <<'EOF' >> "$build_dir/Containerfile"
# Install codehamr (single static Go binary) via the official installer.
# PREFIX=/usr/local puts it on PATH; CODEHAMR_NO_UPDATE_CHECK keeps the pod
# from phoning home for self-updates on every launch.
ENV CODEHAMR_NO_UPDATE_CHECK=1
RUN curl -fsSL https://codehamr.com/install.sh | PREFIX=/usr/local bash \
    && codehamr --version

CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"
    local action="$2"

    # Preserve user edits (model/url/context tweaks) on `pod update`.
    [ "$action" = "update" ] && return 0

    echo -e "\033[36mGenerating codehamr config.yaml...\033[0m"
    mkdir -p "$config_dir" 2>/dev/null || true

    # First comma-separated model is the default.
    local first_model="${DEFAULT_MODEL%%,*}"
    first_model=$(echo "$first_model" | xargs)

    # codehamr appends /v1/... itself, so hand it the base URL with /v1 removed.
    local base_url="${OPENAI_BASE_URL%/v1}"

    # context_size should match the server's ACTUAL window. Reuse the same
    # POD_DEFAULT_MODEL_CONTEXT_SIZE that codex's model_context_window uses.
    local ctx="${DEFAULT_MODEL_CONTEXT_SIZE:-32768}"

    cat <<EOF > "$config_dir/config.yaml"
# Pod Agents Manager: auto-generated. Edits are preserved by \`pod update\`;
# reset on \`pod start\` / \`pod restart\`. codehamr appends /v1/chat/completions
# to 'url', so 'url' is the base WITHOUT /v1.
active: local
models:
    local:
        llm: ${first_model}
        url: ${base_url}
        key: ${OPENAI_API_KEY}
        context_size: ${ctx}
    hamrpass:
        llm: hamrpass
        url: https://codehamr.com
        key: ""
EOF
}

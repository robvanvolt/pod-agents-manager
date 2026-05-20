# OpenAI Codex CLI — https://github.com/openai/codex
# https://developers.openai.com/codex/config-reference
#
# OpenAI's official coding-agent CLI. Unlike most provider-locked agents, Codex
# is designed to talk to ANY OpenAI-compatible endpoint via [model_providers]
# blocks in ~/.codex/config.toml — exactly the integration point this project
# was built around.
#
# Install: `npm i -g @openai/codex`
# Binary:  `codex`
# Non-interactive: `codex exec --profile <name> "<prompt>"` (one-shot, exits)
#
# Local-LLM routing:
#   The config.toml this plugin writes registers a `local` model_provider
#   pointing at $OPENAI_BASE_URL with env_key = "OPENAI_API_KEY", plus a
#   `local` profile using $DEFAULT_MODEL, plus a top-level `profile = "local"`
#   so bare `codex` and `codex exec` both default to local inference without
#   the user having to remember --profile.
#
# Safety bypass (mirrors claude.sh):
#   Codex by default refuses to run outside a trusted git repo and asks for
#   per-action approvals. For a pod sandbox we want the same "trust everything,
#   no prompts" behavior the claude agent has via --dangerously-skip-permissions.
#   The Codex equivalents are --skip-git-repo-check + --dangerously-bypass-
#   approvals-and-sandbox; both have global = true in the codex CLI, so a
#   single wrapper script that always prepends them works for `codex`,
#   `codex exec`, and every subcommand.

AGENT_VOLUME_CONFIG_PATH="/root/.codex"
AGENT_SKILLS_SUBPATH="skills"

# Non-interactive prompt mode for `pod batch` and `pod test`. The wrapper
# installed by agent_build_containerfile auto-adds --skip-git-repo-check and
# --dangerously-bypass-approvals-and-sandbox, so AGENT_BATCH_INVOKE stays
# clean: just exec subcommand + --profile + prompt.
AGENT_BATCH_INVOKE='codex exec --profile local "$PROMPT"'

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"

    write_base_node_containerfile "$build_dir" "$flavor"
    cat <<'EOF' >> "$build_dir/Containerfile"
# Install OpenAI Codex CLI via npm
RUN npm install -g @openai/codex && npm cache clean --force

# Wrap the codex binary so every invocation — interactive (`pod join codex
# dev`) AND batch (`pod batch codex …`) — bypasses the git-repo check and the
# per-action approval / sandbox prompts. Mirrors the pattern in claude.sh.
# Both flags are clap-global so they're accepted before OR after any
# subcommand.
RUN mv /usr/local/bin/codex /usr/local/bin/codex-original && \
    printf '#!/bin/sh\nexec /usr/local/bin/codex-original --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox "$@"\n' > /usr/local/bin/codex && \
    chmod +x /usr/local/bin/codex

CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"
    local action="$2"

    # Preserve user customizations on `pod update`.
    [ "$action" = "update" ] && return 0

    echo -e "\033[36mGenerating codex config.toml...\033[0m"
    mkdir -p "$config_dir" 2>/dev/null || true

    # Pick the first comma-separated model as the profile's default. Users
    # can edit ~/.codex/config.toml inside the workspace to add more profiles
    # or switch models per-conversation with `codex --profile <name>`.
    local first_model="${DEFAULT_MODEL%%,*}"
    first_model=$(echo "$first_model" | xargs)

    # Codex doesn't auto-discover model metadata from a custom provider's
    # /v1/models endpoint. For known cloud models its built-in catalog kicks
    # in; for everything else (every local model) it warns
    #   "Model metadata for X not found. Defaulting to fallback metadata"
    # and falls back to a conservative context window. Setting
    # `model_context_window` top-level overrides that fallback explicitly.
    # The value comes from POD_DEFAULT_MODEL_CONTEXT_SIZE in .env (default
    # 131072 = 128k tokens), so users with larger-context models can bump
    # it once in .env and every codex pod picks it up.
    local ctx="${DEFAULT_MODEL_CONTEXT_SIZE:-131072}"
    cat <<EOF > "$config_dir/config.toml"
# Pod Agents Manager: auto-generated. Edits to this file are preserved by
# \`pod update\`; they're reset on \`pod start\` / \`pod restart\`.

# Top-level default — \`codex\` and \`codex exec\` use this profile unless
# overridden with --profile. AGENT_BATCH_INVOKE still passes --profile local
# explicitly so batch behavior is independent of this default.
profile = "local"

# Context window for codex's built-in metadata fallback. Sourced from
# POD_DEFAULT_MODEL_CONTEXT_SIZE in ~/.pod_agents_config/.env.
model_context_window = ${ctx}

[model_providers.local]
name = "Pod Agents local OpenAI-compatible"
base_url = "${OPENAI_BASE_URL}"
env_key = "OPENAI_API_KEY"

[profiles.local]
model_provider = "local"
model = "${first_model}"
EOF
}

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

AGENT_VOLUME_CONFIG_PATH="/root/.codex"
AGENT_SKILLS_SUBPATH="skills"

# Non-interactive prompt mode for `pod batch` and `pod test`. `codex exec` is
# the documented one-shot subcommand; `--profile local` is explicit so this
# stays correct even if the user edits config.toml later.
AGENT_BATCH_INVOKE='codex exec --profile local "$PROMPT"'

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"

    write_base_node_containerfile "$build_dir" "$flavor"
    cat <<'EOF' >> "$build_dir/Containerfile"
# Install OpenAI Codex CLI via npm
RUN npm install -g @openai/codex && npm cache clean --force

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

    cat <<EOF > "$config_dir/config.toml"
# Pod Agents Manager: auto-generated. Edits to this file are preserved by
# \`pod update\`; they're reset on \`pod start\` / \`pod restart\`.

# Top-level default — \`codex\` and \`codex exec\` use this profile unless
# overridden with --profile. AGENT_BATCH_INVOKE still passes --profile local
# explicitly so batch behavior is independent of this default.
profile = "local"

[model_providers.local]
name = "Pod Agents local OpenAI-compatible"
base_url = "${OPENAI_BASE_URL}"
env_key = "OPENAI_API_KEY"

[profiles.local]
model_provider = "local"
model = "${first_model}"
EOF
}

# Command Code — https://github.com/CommandCodeAI/command-code
# https://commandcode.ai/docs
#
# Frontier coding-agent CLI, installed via `npm i -g command-code`. The binary
# is named `cmd`. Headless one-shot mode is `cmd --print "<prompt>" --yolo`,
# which is exactly what `pod batch` needs: autonomous execution inside the pod
# sandbox, then exit back to the shell.
#
# Local-LLM note: Command Code's documented providers are
#   1. Command Code (their hosted backend; needs `cmd login` for OAuth)
#   2. Anthropic
# It does not (as of writing) document an OpenAI-compatible-baseUrl override.
# The Quadlet template already exports ANTHROPIC_BASE_URL + ANTHROPIC_API_KEY
# pointed at the user's POD_OPENAI_BASE_URL, so for local-inference use:
#
#   pod start command-code dev
#   pod join command-code dev
#   /provider                  # pick "Anthropic" — Anthropic SDK honors
#                              # ANTHROPIC_BASE_URL, transparently routing to
#                              # the user's local server
#
# If Command Code later adds a documented openai/custom-endpoint provider,
# this plugin should be updated to write that config directly.

AGENT_VOLUME_CONFIG_PATH="/root/.commandcode"

# Command Code scans ~/.commandcode/skills for user skills. The lifecycle
# module links this path to the shared read-only /srv/skills mount.
AGENT_SKILLS_SUBPATH="skills"

# Non-interactive prompt mode for `pod batch` and `pod test`.
AGENT_BATCH_INVOKE='cmd --print "$PROMPT" --yolo'

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"

    write_base_node_containerfile "$build_dir" "$flavor"
    cat <<'EOF' >> "$build_dir/Containerfile"
# Install Command Code via npm
RUN npm install -g command-code && npm cache clean --force

CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"
    local action="$2"

    # Treat "update" as "preserve existing config" — every shipped agent does
    # this so `pod update` doesn't clobber user customization.
    [ "$action" = "update" ] && return 0

    echo -e "\033[36mGenerating command-code config...\033[0m"
    mkdir -p "$config_dir" 2>/dev/null || true

    # Command Code's config-file schema isn't publicly documented yet, so we
    # only seed the bind-mount root and let the binary create whatever it
    # needs on first run. The ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY env vars
    # exported by the Quadlet are what actually route inference; the user
    # picks the "Anthropic" provider from the in-CLI `/provider` selector
    # the first time they launch.
    cat <<EOF > "$config_dir/README.txt"
This directory is bind-mounted from the host as Command Code's config dir
(\$AGENT_VOLUME_CONFIG_PATH=/root/.commandcode).

To use a local OpenAI-compatible inference server, after \`pod join command-code <inst>\`:
  1. Run \`cmd\`
  2. Type \`/provider\` and pick "Anthropic"
  3. The container already has ANTHROPIC_BASE_URL=${OPENAI_BASE_URL%/v1}
     and ANTHROPIC_API_KEY=<your local key> exported, so all requests will
     route to your local server transparently.

Default model: ${DEFAULT_MODEL}
EOF
}

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
#   pointing at $OPENAI_BASE_URL with env_key = "OPENAI_API_KEY", and selects
#   it via TOP-LEVEL `model` + `model_provider` keys (NOT a [profiles.*] table).
#   Recent codex versions (>= 0.140) made the old `profile = "local"` /
#   `[profiles.local]` scheme "legacy" and incompatible with `--profile local`
#   ("legacy profile ... is no longer supported"), so this plugin uses the
#   direct top-level form and AGENT_BATCH_INVOKE drops `--profile`.
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
# clean. No --profile: the local provider is the top-level default in
# config.toml (see agent_generate_config + the routing note above).
AGENT_BATCH_INVOKE='codex exec "$PROMPT"'

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"

    write_base_node_containerfile "$build_dir" "$flavor"

    # Wrap the codex binary so `pod batch codex …` (which uses `codex exec`)
    # gets the auto-bypass flags without the user having to remember them.
    # The two flags --skip-git-repo-check and --dangerously-bypass-approvals-
    # and-sandbox live ONLY on the `exec` subcommand's clap definition; the
    # root command (bare `codex` / interactive TUI) errors out with
    # "unexpected argument" if they're passed unconditionally. So the
    # wrapper is context-aware: it injects the flags only when the first
    # positional is `exec`, and passes everything else through untouched.
    # The interactive case is handled by the [projects."<work_dir>"]
    # trust_level = "trusted" block written into config.toml by
    # agent_generate_config below — that's codex's documented config-level
    # escape hatch from the "Not inside a trusted directory" check.
    #
    # Writing the wrapper to a separate file + COPY (rather than building it
    # inline via printf in a Dockerfile RUN) avoids every shell-escape
    # pitfall around backslash line-continuations getting double-escaped.
    cat <<'WRAPPER' > "$build_dir/codex-wrapper.sh"
#!/bin/sh
# /usr/local/bin/codex — pod-agents-manager wrapper around /usr/local/bin/codex-original.
# Injects batch-mode safety flags ONLY when invoking the `exec` subcommand,
# because they don't exist on codex's root command and would error out there.
if [ "$1" = "exec" ]; then
    sub="$1"; shift
    exec /usr/local/bin/codex-original "$sub" \
        --skip-git-repo-check \
        --dangerously-bypass-approvals-and-sandbox "$@"
fi
exec /usr/local/bin/codex-original "$@"
WRAPPER
    chmod +x "$build_dir/codex-wrapper.sh"

    cat <<'EOF' >> "$build_dir/Containerfile"
# Install bubblewrap so codex finds `bwrap` on PATH. Codex prefers the system
# bubblewrap over its bundled fallback; without it codex prints
#   "⚠ Codex could not find bubblewrap on PATH. […] Codex will use the bundled
#    bubblewrap in the meantime."
# on every invocation. The wrapper below passes --dangerously-bypass-approvals-
# and-sandbox so the sandbox isn't actually exercised, but installing bwrap
# silences the warning and lets codex use it for any internal sub-sandboxing.
# Base-image agnostic — works on both alpine (apk) and trixie-slim (apt).
RUN if command -v apk >/dev/null 2>&1; then \
        apk add --no-cache bubblewrap; \
    elif command -v apt-get >/dev/null 2>&1; then \
        DEBIAN_FRONTEND=noninteractive apt-get update \
            && apt-get install -y --no-install-recommends bubblewrap \
            && rm -rf /var/lib/apt/lists/*; \
    else \
        echo "WARNING: no known package manager — bubblewrap not installed" >&2; \
    fi

# Install OpenAI Codex CLI via npm
RUN npm install -g @openai/codex && npm cache clean --force

# Install the subcommand-aware wrapper (see codex-wrapper.sh in the build dir).
RUN mv /usr/local/bin/codex /usr/local/bin/codex-original
COPY codex-wrapper.sh /usr/local/bin/codex
RUN chmod +x /usr/local/bin/codex

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

    # Pick the first comma-separated model as the default. Users can edit
    # ~/.codex/config.toml in the workspace to point at a different model
    # or add their own [model_providers.*] block.
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

    # Mirror the Quadlet's WorkingDir computation (see ensure_quadlet_template
    # in 70-lifecycle.sh) so the [projects."<work_dir>"] block matches
    # whatever directory codex is actually launched in. Without this, the
    # interactive `codex` TUI refuses to run with "Not inside a trusted
    # directory" — and the wrapper can't inject --skip-git-repo-check at the
    # root level (that flag exists only on the `exec` subcommand).
    local work_dir="/workspace"
    if [ -n "${WORKSPACE_DIR_OVERRIDE:-}" ]; then
        if [[ "$WORKSPACE_DIR_OVERRIDE" == /* ]]; then
            work_dir="$WORKSPACE_DIR_OVERRIDE"
        else
            work_dir="/workspace/$WORKSPACE_DIR_OVERRIDE"
        fi
    fi

    cat <<EOF > "$config_dir/config.toml"
# Pod Agents Manager: auto-generated. Edits to this file are preserved by
# \`pod update\`; they're reset on \`pod start\` / \`pod restart\`.

# Default model + provider, selected at the TOP LEVEL (not via a
# [profiles.*] table). codex >= 0.140 rejects the legacy profile scheme with
# "legacy profile ... is no longer supported", so we bind the local provider
# directly here. \`codex\` and \`codex exec\` both use it with no --profile.
model = "${first_model}"
model_provider = "local"

# Context window for codex's built-in metadata fallback. Sourced from
# POD_DEFAULT_MODEL_CONTEXT_SIZE in ~/.pod_agents_config/.env.
model_context_window = ${ctx}

# Mark the pod's working directory as a trusted project so the interactive
# \`codex\` TUI doesn't refuse to launch with "Not inside a trusted directory".
# The matching --skip-git-repo-check flag on the \`exec\` subcommand is
# applied by the wrapper at /usr/local/bin/codex, but exists only there;
# the root command needs this config-level trust declaration.
[projects."${work_dir}"]
trust_level = "trusted"

[model_providers.local]
name = "Pod Agents local OpenAI-compatible"
base_url = "${OPENAI_BASE_URL}"
env_key = "OPENAI_API_KEY"
EOF
}

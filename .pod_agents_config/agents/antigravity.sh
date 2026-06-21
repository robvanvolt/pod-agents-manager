# Google Antigravity CLI — https://antigravity.google
# https://github.com/google-antigravity/antigravity-cli
#
# Google's terminal coding agent (`agy`), the successor to Gemini CLI and the
# same engine as the Antigravity desktop app. Single Go binary, installed via
# Google's curl|bash bootstrapper (NOT npm).
#
# ── IMPORTANT: this agent is different from the others ───────────────────────
# Unlike codex / pi / opencode / crush, Antigravity is a CLOUD-TIED agent:
#
#   * Auth is Google Sign-In. There is no API-key env var that bypasses it —
#     ANTIGRAVITY_API_KEY / ANTIGRAVITY_TOKEN do NOT work (verified against
#     agy 1.0.10). Over SSH/headless it prints a device-code URL to complete
#     sign-in in a browser. So the FIRST run must be interactive:
#         pod join antigravity dev      # then follow the printed URL + code
#     Auth then persists in the bind-mounted config dir across restarts.
#
#   * It runs Google's own models (Gemini 3.x, Claude via Google, GPT-OSS).
#     It does NOT talk to your local POD_OPENAI_BASE_URL server. The blog
#     posts claiming a `base_url` override route to self-hosted models do not
#     reflect agy 1.0.10 — `base_url` there is the editor sidecar, not an
#     LLM-provider swap. So this agent ignores OPENAI_BASE_URL/_API_KEY.
#
#   * `pod batch antigravity …` only works AFTER a successful interactive
#     sign-in, and bills against your Google/Antigravity account.
#
# If/when Google publishes an OpenAI-compatible provider override (or a
# service-account / GOOGLE_APPLICATION_CREDENTIALS non-interactive path that
# fits a pod), update agent_generate_config to write it.
# ─────────────────────────────────────────────────────────────────────────────
#
# Binary:  agy
# Install: curl -fsSL https://antigravity.google/cli/install.sh | bash -s -- --dir /usr/local/bin
# Headless one-shot: agy --print --dangerously-skip-permissions "<prompt>"
#   --print (-p)                     run a single prompt non-interactively, exit
#   --dangerously-skip-permissions   auto-approve all tool actions (no prompts)
#   --model <m>                      override the session model
#
# Base image: MUST be Debian/glibc (trixie-slim). Google publishes
# linux_amd64 (glibc) but NOT linux_amd64_musl, and the installer's musl
# detection on Alpine requests a manifest that 404s. (Same reason opencode
# forces trixie-slim.)

AGENT_VOLUME_CONFIG_PATH="/root/.config/antigravity"
AGENT_SKILLS_SUBPATH="skills"

# Non-interactive prompt mode for `pod batch` / `pod test`. Works only after a
# one-time interactive Google sign-in (see header). --print and
# --dangerously-skip-permissions are both root-level flags, so no wrapper /
# subcommand dispatch is needed (unlike codex).
AGENT_BATCH_INVOKE='agy --print --dangerously-skip-permissions "$PROMPT"'

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"

    # Force the Debian/glibc base — there is no musl build of agy yet.
    write_base_node_containerfile "$build_dir" "$flavor" "trixie-slim"
    cat <<'EOF' >> "$build_dir/Containerfile"
# Install Google Antigravity CLI (`agy`) via the official bootstrapper.
# --dir installs straight onto PATH; the staging/checksum steps need curl +
# ca-certificates (already present in the trixie-slim node base, but pinned
# here for safety). The bootstrapper detects glibc and pulls cli_linux_x64.
RUN curl -fsSL https://antigravity.google/cli/install.sh \
      | bash -s -- --dir /usr/local/bin \
    && /usr/local/bin/agy --version

CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"
    local action="$2"

    # Preserve any signed-in session / user config on `pod update`.
    [ "$action" = "update" ] && return 0

    echo -e "\033[36mPreparing antigravity config dir...\033[0m"
    mkdir -p "$config_dir" 2>/dev/null || true

    # There's no useful local-endpoint config to seed (see header). Drop a
    # README so the user finds the sign-in instructions on first run instead
    # of being confused by a cloud-auth prompt.
    cat <<EOF > "$config_dir/README.txt"
Google Antigravity CLI (agy) — bind-mounted config dir
(\$AGENT_VOLUME_CONFIG_PATH=/root/.config/antigravity).

This agent is CLOUD-TIED and needs a one-time Google sign-in:

  1. pod join antigravity ${instance:-<instance>}
  2. agy            # launches the CLI; over SSH it prints a URL + one-time code
  3. Complete sign-in in your browser. The session persists in this directory,
     so subsequent \`pod join\` / \`pod batch\` runs reuse it.

It runs Google's models (Gemini / Claude-via-Google), NOT your local
POD_OPENAI_BASE_URL server. Non-interactive batch usage:

  agy --print --dangerously-skip-permissions "your prompt"

Default local model (${DEFAULT_MODEL}) is ignored by this agent.
EOF
}

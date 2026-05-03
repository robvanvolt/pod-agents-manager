#!/usr/bin/env bash
set -euo pipefail

REPO="${POD_AGENTS_REPO:-robvanvolt/pod-agents-manager}"
REF="${POD_AGENTS_REF:-main}"
ARCHIVE_URL="${POD_AGENTS_ARCHIVE_URL:-https://codeload.github.com/${REPO}/tar.gz/refs/heads/${REF}}"
CONFIG_ROOT="${HOME}/.pod_agents_config"
# Allow non-interactive override (e.g. CI / scripted installs):
#   POD_AGENTS_CMD=pods curl ...|bash
CMD_NAME_OVERRIDE="${POD_AGENTS_CMD:-}"

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "Missing required command: $1" >&2
        exit 1
    }
}

merge_tree() {
    local src="$1"
    local dest="$2"
    [ -d "$src" ] || return 0
    mkdir -p "$dest"
    cp -R "$src"/. "$dest"/
}

detect_shell_rc() {
    case "$(basename "${SHELL:-bash}")" in
        zsh) printf '%s\n' "${HOME}/.zshrc" ;;
        *) printf '%s\n' "${HOME}/.bashrc" ;;
    esac
}

require_cmd curl
require_cmd tar

# ----------------------------------------------------------------------------
# Resolve the user-facing command name. Defaults to `pod`. If something else
# already provides `pod` on PATH (e.g. CocoaPods at /opt/homebrew/bin/pod),
# offer the user an alternative so we don't silently shadow it.
# ----------------------------------------------------------------------------
existing_pod_path=""
if command -v pod >/dev/null 2>&1; then
    existing_pod_path="$(command -v pod 2>/dev/null || true)"
fi
# A previous install of ours that defined pod() as a function isn't a
# collision worth re-prompting for; only worry about external binaries.
if [ -n "$existing_pod_path" ] && [ -f "$existing_pod_path" ] && [ "$existing_pod_path" != "$HOME/.pod_agents" ]; then
    if [ -n "$CMD_NAME_OVERRIDE" ]; then
        chosen_cmd="$CMD_NAME_OVERRIDE"
        echo "An existing 'pod' was detected at $existing_pod_path; installing under '$chosen_cmd' (POD_AGENTS_CMD)."
    elif [ -t 0 ] && [ -t 1 ]; then
        echo "An existing 'pod' command was detected at $existing_pod_path"
        echo "  (probably CocoaPods or another tool). Installing pod-agents-manager"
        echo "  under that name would shadow it in your shell."
        echo
        read -r -p "Choose a command name for pod-agents-manager [pods]: " chosen_cmd
        chosen_cmd="${chosen_cmd:-pods}"
    else
        # Non-interactive (curl|bash) without override: pick a safe default
        # rather than silently shadowing.
        chosen_cmd="pods"
        echo "An existing 'pod' was detected at $existing_pod_path; installing under '$chosen_cmd'."
        echo "  (Set POD_AGENTS_CMD=<name> before re-running install.sh to choose a different name.)"
    fi
else
    chosen_cmd="${CMD_NAME_OVERRIDE:-pod}"
fi

# Validate the chosen name to avoid eval'ing junk later.
if ! [[ "$chosen_cmd" =~ ^[a-zA-Z_][a-zA-Z0-9_-]*$ ]]; then
    echo "Invalid command name '$chosen_cmd' — must match [a-zA-Z_][a-zA-Z0-9_-]*" >&2
    exit 1
fi

tmp_dir=$(mktemp -d)
cleanup() {
    rm -rf "$tmp_dir"
}
trap cleanup EXIT

echo "Fetching ${REPO}@${REF}..."
curl -fsSL "$ARCHIVE_URL" | tar -xzf - -C "$tmp_dir"

src_root=$(find "$tmp_dir" -mindepth 1 -maxdepth 1 -type d | head -n 1)
[ -n "$src_root" ] || {
    echo "Failed to unpack repository archive." >&2
    exit 1
}

mkdir -p "$CONFIG_ROOT"
cp "$src_root/.pod_agents" "${HOME}/.pod_agents"
if [ ! -f "$CONFIG_ROOT/.env" ]; then
    cp "$src_root/.pod_agents_config/.env.example" "$CONFIG_ROOT/.env"
fi
cp "$src_root/.pod_agents_config/.env.example" "$CONFIG_ROOT/.env.example"
cp "$src_root/.pod_agents_config/version.conf" "$CONFIG_ROOT/version.conf"
merge_tree "$src_root/.pod_agents_config/agents" "$CONFIG_ROOT/agents"
merge_tree "$src_root/.pod_agents_config/flavors" "$CONFIG_ROOT/flavors"
merge_tree "$src_root/.pod_agents_config/volumes" "$CONFIG_ROOT/volumes"
merge_tree "$src_root/.pod_agents_config/skills" "$CONFIG_ROOT/skills"
merge_tree "$src_root/.pod_agents_config/lib" "$CONFIG_ROOT/lib"
merge_tree "$src_root/.pod_agents_config/server" "$CONFIG_ROOT/server"
rm -f "$CONFIG_ROOT/server/static/favicon.ico"

# Persist the chosen command name so .pod_agents picks it up on every shell start.
printf '%s\n' "$chosen_cmd" > "$CONFIG_ROOT/.cmd_name"

rc_file=$(detect_shell_rc)
source_line='[ -f "$HOME/.pod_agents" ] && source "$HOME/.pod_agents"'
touch "$rc_file"
if ! grep -Fqx "$source_line" "$rc_file"; then
    printf '\n%s\n' "$source_line" >> "$rc_file"
fi

echo
echo "Installed pod-agents-manager into ${HOME} as command '${chosen_cmd}'."
echo "Shell init updated: ${rc_file}"
echo "Next steps:"
echo "  exec $(basename "${SHELL:-bash}") -l"
echo "  ${chosen_cmd} doctor                      # verify host readiness"
echo "  ${chosen_cmd} start <agent> <instance>    # prompts once if POD_* values are unset"
echo "  ${chosen_cmd} config                      # optional: adjust saved values later"
echo "  ${chosen_cmd} prebuild"
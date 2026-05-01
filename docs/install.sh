#!/usr/bin/env bash
set -euo pipefail

REPO="${POD_AGENTS_REPO:-robvanvolt/pod-agents-manager}"
REF="${POD_AGENTS_REF:-main}"
ARCHIVE_URL="${POD_AGENTS_ARCHIVE_URL:-https://codeload.github.com/${REPO}/tar.gz/refs/heads/${REF}}"
CONFIG_ROOT="${HOME}/.pod_agents_config"

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
[ -f "$CONFIG_ROOT/defaults.conf" ] || cp "$src_root/.pod_agents_config/defaults.conf" "$CONFIG_ROOT/defaults.conf"
cp "$src_root/.pod_agents_config/version.conf" "$CONFIG_ROOT/version.conf"
merge_tree "$src_root/.pod_agents_config/agents" "$CONFIG_ROOT/agents"
merge_tree "$src_root/.pod_agents_config/flavors" "$CONFIG_ROOT/flavors"
merge_tree "$src_root/.pod_agents_config/volumes" "$CONFIG_ROOT/volumes"
merge_tree "$src_root/.pod_agents_config/skills" "$CONFIG_ROOT/skills"
merge_tree "$src_root/.pod_agents_config/server" "$CONFIG_ROOT/server"
rm -f "$CONFIG_ROOT/server/static/favicon.ico"

rc_file=$(detect_shell_rc)
source_line='[ -f "$HOME/.pod_agents" ] && source "$HOME/.pod_agents"'
touch "$rc_file"
if ! grep -Fqx "$source_line" "$rc_file"; then
    printf '\n%s\n' "$source_line" >> "$rc_file"
fi

echo
echo "Installed pod-agents-manager into ${HOME}."
echo "Shell init updated: ${rc_file}"
echo "Next steps:"
echo "  exec $(basename "${SHELL:-bash}") -l"
echo "  \${EDITOR:-vi} ~/.pod_agents_config/defaults.conf"
echo "  pod prebuild"
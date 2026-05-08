#!/usr/bin/env bash
set -euo pipefail

REPO="${POD_AGENTS_REPO:-robvanvolt/pod-agents-manager}"
REF="${POD_AGENTS_REF:-dev}"

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "Missing required command: $1" >&2
        exit 1
    }
}

require_cmd curl

tmp_dir=$(mktemp -d)
cleanup() {
    rm -rf "$tmp_dir"
}
trap cleanup EXIT

echo "Installing pod-agents-manager development channel from ${REPO}@${REF}..."
curl -fsSL "https://raw.githubusercontent.com/${REPO}/${REF}/install.sh" -o "$tmp_dir/install.sh"

POD_AGENTS_REPO="$REPO" POD_AGENTS_REF="$REF" bash "$tmp_dir/install.sh"

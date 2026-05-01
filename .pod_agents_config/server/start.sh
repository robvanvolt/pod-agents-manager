#!/usr/bin/env bash
# Convenience wrapper. Prefer `pod server start` from .pod_agents — same logic,
# plus PID management, status, logs, restart.
set -euo pipefail

if ! type pod >/dev/null 2>&1; then
    # shellcheck disable=SC1090
    source "$HOME/.pod_agents"
fi

exec pod server start

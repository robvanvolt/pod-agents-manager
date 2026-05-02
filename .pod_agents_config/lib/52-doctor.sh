    # `pod doctor` — diagnose host setup before users hit confusing errors
    # deep inside `pod start`. Each check prints PASS / WARN / FAIL with a
    # one-line hint. WARN does not fail; FAIL bumps the exit code.
    if [ "$action" = "doctor" ]; then
        local _doc_pass=0 _doc_warn=0 _doc_fail=0

        _doc_pass() { printf '  \033[32m[PASS]\033[0m %s\n' "$1"; _doc_pass=$((_doc_pass+1)); }
        _doc_warn() {
            printf '  \033[33m[WARN]\033[0m %s\n' "$1"
            [ -n "${2:-}" ] && printf '         hint: %s\n' "$2"
            _doc_warn=$((_doc_warn+1))
        }
        _doc_fail() {
            printf '  \033[31m[FAIL]\033[0m %s\n' "$1"
            [ -n "${2:-}" ] && printf '         hint: %s\n' "$2"
            _doc_fail=$((_doc_fail+1))
        }

        echo -e "\033[1;36m==> pod-agents-manager doctor\033[0m"
        echo "    config: $config_dir_root"
        echo "    version: ${POD_AGENTS_VERSION:-unknown}"
        echo

        # --- 1. podman ------------------------------------------------------
        if command -v podman >/dev/null 2>&1; then
            local _pv
            _pv=$(podman --version 2>/dev/null | awk '{print $3}')
            _doc_pass "podman installed (${_pv:-unknown})"

            # Rootless check: prefer podman's own report, fall back to euid.
            local _rootless
            _rootless=$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)
            if [ "$_rootless" = "true" ]; then
                _doc_pass "podman is running rootless"
            elif [ "$(id -u)" -ne 0 ]; then
                _doc_warn "podman rootless flag not reported, but running as non-root user"
            else
                _doc_fail "podman is running as root" \
                    "rootless mode is required; run as your user, not via sudo"
            fi
        else
            _doc_fail "podman not installed" \
                "install podman 4.4+ (5.x recommended) — https://podman.io/docs/installation"
        fi

        # --- 2. systemd / quadlet ------------------------------------------
        if command -v systemctl >/dev/null 2>&1; then
            if systemctl --user show-environment >/dev/null 2>&1; then
                _doc_pass "systemctl --user is available"
            else
                _doc_fail "systemctl --user did not respond" \
                    "ensure your user has a systemd session (loginctl enable-linger \$USER)"
            fi
        else
            _doc_fail "systemctl not found" \
                "pod-agents-manager requires systemd; macOS/BSD are not supported runtime hosts"
        fi

        # Quadlet shipped with podman 4.4 and is standard from 5.0+.
        local _quadlet_bin=""
        for _qpath in /usr/libexec/podman/quadlet /usr/lib/podman/quadlet /usr/local/libexec/podman/quadlet; do
            [ -x "$_qpath" ] && { _quadlet_bin="$_qpath"; break; }
        done
        if [ -n "$_quadlet_bin" ]; then
            _doc_pass "quadlet binary present ($_quadlet_bin)"
        elif command -v podman >/dev/null 2>&1; then
            _doc_warn "quadlet binary not found in standard locations" \
                "older podman? upgrade to 4.4+ — quadlet generates the systemd units"
        fi

        # --- 3. lib + config layout ----------------------------------------
        local _lib_count=0
        if [ -d "$config_dir_root/lib" ]; then
            for _f in "$config_dir_root/lib"/*.sh; do
                [ -f "$_f" ] && _lib_count=$((_lib_count+1))
            done
        fi
        if [ "$_lib_count" -gt 0 ]; then
            _doc_pass "lib/ modules loaded ($_lib_count files)"
        else
            _doc_fail "no lib/ modules found in $config_dir_root/lib" \
                "reinstall via install.sh, or run \`pod self-update\`"
        fi

        local _agent_count=0
        for _f in "$config_dir_agents"/*.sh; do
            [ -f "$_f" ] && _agent_count=$((_agent_count+1))
        done
        if [ "$_agent_count" -gt 0 ]; then
            _doc_pass "agents/ contains $_agent_count agent plugin(s)"
        else
            _doc_warn "agents/ is empty" \
                "drop a <name>.sh into $config_dir_agents — see docs/"
        fi

        local _flavor_count=0
        for _f in "$config_dir_flavors"/*.containerfile; do
            [ -f "$_f" ] && _flavor_count=$((_flavor_count+1))
        done
        if [ "$_flavor_count" -gt 0 ]; then
            _doc_pass "flavors/ contains $_flavor_count flavor snippet(s)"
        else
            _doc_warn "flavors/ is empty (only the base image will be built)"
        fi

        # --- 4. env / config -----------------------------------------------
        if [ -f "$pod_env_file" ]; then
            _doc_pass ".env present at $pod_env_file"
        else
            _doc_warn ".env missing at $pod_env_file" "run \`pod config\` to create it"
        fi

        if _pod_has_missing_env; then
            _doc_warn "one or more POD_* variables look unset/placeholder" \
                "run \`pod config --missing-only\`"
        else
            _doc_pass "POD_* env populated (base url, key, model, cache, base image)"
        fi

        # --- 5. workspace + cache roots ------------------------------------
        if [ -n "${WORKSPACES_ROOT:-}" ] && [ -d "$WORKSPACES_ROOT" ] && [ -w "$WORKSPACES_ROOT" ]; then
            _doc_pass "workspaces root writable: $WORKSPACES_ROOT"
        elif [ -n "${WORKSPACES_ROOT:-}" ]; then
            _doc_warn "workspaces root missing or not writable: $WORKSPACES_ROOT" \
                "create it (\`mkdir -p $WORKSPACES_ROOT\`) or change POD_WORKSPACES_ROOT"
        fi

        if [ -n "${IMAGE_CACHE_ROOT:-}" ]; then
            mkdir -p "$IMAGE_CACHE_ROOT" 2>/dev/null || true
            if [ -w "$IMAGE_CACHE_ROOT" ]; then
                _doc_pass "image cache root writable: $IMAGE_CACHE_ROOT"
            else
                _doc_warn "image cache root not writable: $IMAGE_CACHE_ROOT"
            fi
        fi

        # --- 6. dashboard port ---------------------------------------------
        local _dash_port="${POD_SERVER_PORT:-1337}"
        local _holder=""
        if command -v ss >/dev/null 2>&1; then
            _holder=$(ss -ltn "sport = :${_dash_port}" 2>/dev/null | awk 'NR>1 {print; exit}')
        elif command -v lsof >/dev/null 2>&1; then
            _holder=$(lsof -nP -iTCP:"${_dash_port}" -sTCP:LISTEN 2>/dev/null | awk 'NR>1 {print; exit}')
        fi
        if [ -z "$_holder" ]; then
            _doc_pass "dashboard port $_dash_port is free"
        else
            _doc_warn "dashboard port $_dash_port already in use" \
                "stop the holder, or set POD_SERVER_PORT to a free port"
        fi

        # --- 7. inference endpoint -----------------------------------------
        # Best-effort, short timeout. A reachable endpoint is great; a
        # timeout/error is informational, not fatal — the user may be
        # offline or pointing at a not-yet-running local server.
        if [ -n "${POD_OPENAI_BASE_URL:-}" ]; then
            if command -v curl >/dev/null 2>&1; then
                local _http_code
                _http_code=$(curl -sS -o /dev/null -w '%{http_code}' \
                    --max-time 3 "$POD_OPENAI_BASE_URL/models" 2>/dev/null || echo "000")
                case "$_http_code" in
                    2*|3*|401|403)
                        _doc_pass "inference endpoint reachable: $POD_OPENAI_BASE_URL ($_http_code)"
                        ;;
                    000)
                        _doc_warn "inference endpoint unreachable: $POD_OPENAI_BASE_URL" \
                            "check the URL or that your local server is running"
                        ;;
                    *)
                        _doc_warn "inference endpoint returned HTTP $_http_code: $POD_OPENAI_BASE_URL"
                        ;;
                esac
            else
                _doc_warn "curl not installed; skipping inference endpoint check"
            fi
        fi

        # --- 8. recommended host CLIs --------------------------------------
        local _missing_opt=()
        for _cmd in tmux jq curl; do
            command -v "$_cmd" >/dev/null 2>&1 || _missing_opt+=("$_cmd")
        done
        if [ ${#_missing_opt[@]} -eq 0 ]; then
            _doc_pass "recommended host tools present (tmux, jq, curl)"
        else
            _doc_warn "missing recommended host tools: ${_missing_opt[*]}" \
                "install via your package manager — used by tmux grid, batch json, self-update"
        fi

        echo
        printf '\033[1m==> Summary:\033[0m \033[32m%d passed\033[0m, \033[33m%d warning(s)\033[0m, \033[31m%d failed\033[0m\n' \
            "$_doc_pass" "$_doc_warn" "$_doc_fail"

        [ "$_doc_fail" -eq 0 ] && return 0 || return 1
    fi

    return 99  # sentinel: fell off end, continue to next lib

    # 3. Standard Variables & Execution
    local action=$1
    local agent="${2:-}"
    local instance="${3:-}"
    local flavor="${4:-all}"
    local volumes="${5:-all}"
    local base_in="${6:-$BASE_IMAGE}"

    # Backward-compatible alias: treat "standard" as "all".
    [ "$flavor" = "standard" ] && flavor="all"
    [ "$volumes" = "standard" ] && volumes="all"

    _resolve_base_image "$base_in"
    
    if [ "$action" = "cache-clean" ]; then
        echo -e "\033[33mRemoving build cache...\033[0m"
        rm -rf "$IMAGE_CACHE_ROOT"
        podman images --format "{{.Repository}}:{{.Tag}}" | grep "localhost/.*-agent-.*" | xargs -r podman image rm -f
        echo -e "\033[32mCache cleanup complete.\033[0m"
        return 0
    fi

    if [ "$action" = "self-update" ]; then
        local tmp_dir src_root remote_version
        tmp_dir=$(mktemp -d)
        src_root=$(_pod_fetch_repo_snapshot "$tmp_dir") || {
            rm -rf "$tmp_dir"
            echo -e "\033[31mSelf-update failed while fetching the repository snapshot.\033[0m"
            return 1
        }

        # Read remote version without polluting the current shell
        remote_version=$(grep '^POD_AGENTS_VERSION=' "$src_root/.pod_agents_config/version.conf" 2>/dev/null \
            | head -n1 | cut -d'"' -f2)
        : "${remote_version:=$pod_version_default}"

        if [ "$remote_version" = "$POD_AGENTS_VERSION" ]; then
            rm -rf "$tmp_dir"
            echo -e "\033[32mMost recent version is ${POD_AGENTS_VERSION} — already up-to-date.\033[0m"
            return 0
        fi

        echo -e "\033[36mNew version found. Update from \033[1m${POD_AGENTS_VERSION}\033[0m\033[36m → \033[1m${remote_version}\033[0m\033[36m?\033[0m"
        if ! _pod_prompt_yes_no "Apply update?"; then
            rm -rf "$tmp_dir"
            echo -e "\033[33mUpdate cancelled.\033[0m"
            return 0
        fi

        _pod_sync_managed_files "$src_root" || {
            rm -rf "$tmp_dir"
            echo -e "\033[31mSelf-update failed while applying managed files.\033[0m"
            return 1
        }
        rm -rf "$tmp_dir"

        # shellcheck disable=SC1090
        source "$HOME/.pod_agents"
        if [ -f "$config_dir_root/version.conf" ]; then
            # shellcheck disable=SC1090
            source "$config_dir_root/version.conf"
        fi
        : "${POD_AGENTS_VERSION:=$pod_version_default}"

        echo -e "\033[32mUpdated pod-agents-manager to ${POD_AGENTS_VERSION}.\033[0m"
        echo -e "\033[36m.env and any extra custom files were left in place.\033[0m"
        return 0
    fi

    if [ "$action" = "config" ]; then
        local config_mode="interactive"
        case "$agent" in
            "" ) config_mode="interactive" ;;
            --missing-only) config_mode="missing" ;;
            *)
                echo "Usage: pod config [--missing-only]"
                return 1
                ;;
        esac
        _pod_configure_env "$config_mode"
        return $?
    fi

    if [ "$action" = "uninstall" ]; then
        echo -e "\033[1;31mUninstall pod-agents-manager\033[0m"
        echo ""
        echo -e "\033[36mWhat would you like to remove?\033[0m"
        echo "  1) Manager only        — removes ~/.pod_agents and the shell rc source line"
        echo "  2) + Config & images   — also removes ~/.pod_agents_config and all pod images"
        echo "  3) + All pod volumes   — also removes ${WORKSPACES_ROOT}/*-pods (DESTRUCTIVE: all agent workspaces gone)"
        echo "  0) Cancel"
        echo ""
        local uninstall_level=""
        while true; do
            printf 'Choice [0-3]: ' > /dev/tty
            IFS= read -r uninstall_level < /dev/tty || { echo "Cancelled."; return 0; }
            case "$uninstall_level" in
                0) echo "Cancelled."; return 0 ;;
                1|2|3) break ;;
                *) echo "Please enter 0, 1, 2, or 3." ;;
            esac
        done

        local confirm_msg="Remove pod-agents-manager"
        [ "$uninstall_level" = "2" ] && confirm_msg="Remove pod-agents-manager + config, cache and images"
        [ "$uninstall_level" = "3" ] && confirm_msg="Remove EVERYTHING including all pod volumes (DESTRUCTIVE)"
        _pod_prompt_yes_no "$confirm_msg — are you sure?" "N" || { echo "Cancelled."; return 0; }

        # Level 1: manager script + shell rc source line
        echo -e "\033[33mRemoving ~/.pod_agents...\033[0m"
        rm -f "$HOME/.pod_agents"
        local _source_line='[ -f "$HOME/.pod_agents" ] && source "$HOME/.pod_agents"'
        local _rcfile
        for _rcfile in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.bash_profile" "$HOME/.profile"; do
            [ -f "$_rcfile" ] || continue
            if grep -qF "$_source_line" "$_rcfile"; then
                grep -vF "$_source_line" "$_rcfile" > "${_rcfile}.pod_uninstall_bak"
                mv "${_rcfile}.pod_uninstall_bak" "$_rcfile"
                echo -e "\033[33mRemoved source line from $_rcfile\033[0m"
            fi
        done

        if [ "$uninstall_level" -ge 2 ]; then
            echo -e "\033[33mRemoving $config_dir_root...\033[0m"
            rm -rf "$config_dir_root"
            echo -e "\033[33mRemoving pod agent container images...\033[0m"
            podman images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null \
                | grep '^localhost/.*-agent-' \
                | while IFS= read -r _img; do
                    podman image rm -f "$_img" 2>/dev/null && echo "  Removed image: $_img" || true
                done
        fi

        if [ "$uninstall_level" -ge 3 ]; then
            echo -e "\033[31mStopping all pod services and removing pod volumes...\033[0m"
            local _u_agent _u_inst
            for _u_agentdir in "$WORKSPACES_ROOT/"*-pods; do
                [ -d "$_u_agentdir" ] || continue
                _u_agent="$(basename "$_u_agentdir" | sed 's/-pods$//')"
                for _u_instdir in "$_u_agentdir"/*/; do
                    [ -d "$_u_instdir" ] || continue
                    _u_inst=$(basename "$_u_instdir")
                    systemctl --user disable --now "${_u_agent}@${_u_inst}.service" 2>/dev/null || true
                done
                rm -f "$HOME/.config/containers/systemd/${_u_agent}@.container" 2>/dev/null || true
                rm -rf "$_u_agentdir"
                echo "  Removed: $_u_agentdir"
            done
            systemctl --user daemon-reload 2>/dev/null || true
        fi

        echo ""
        echo -e "\033[32mUninstall complete.\033[0m"
        case "$uninstall_level" in
            1) echo "  Tip: config and pod data in ~/.pod_agents_config and ~/Developer/*-pods are still intact." ;;
            2) echo "  Tip: pod workspace volumes in ~/Developer/*-pods are still intact." ;;
            3) echo "  All pod-agents-manager data has been removed from this system." ;;
        esac
        return 0
    fi

    if [ "$action" = "base" ]; then
        local new_base="${2:-}"
        if [ -z "$new_base" ]; then
            echo "Current default base image: ${BASE_IMAGE} (resolved: ${BASE_IMAGE_FULL})"
            echo "Usage: pod base <alpine|trixie-slim|...>"
            return 0
        fi
        _resolve_base_image "$new_base"
        if [ -f "$pod_env_file" ]; then
            if grep -q '^POD_BASE_IMAGE=' "$pod_env_file"; then
                sed -i.bak -E "s|^POD_BASE_IMAGE=.*|POD_BASE_IMAGE=\"${new_base}\"|" "$pod_env_file"
                rm -f "$pod_env_file.bak"
            else
                printf '\nPOD_BASE_IMAGE="%s"\n' "$new_base" >> "$pod_env_file"
            fi
        fi
        echo -e "\033[32mDefault base image set to '${new_base}' (${BASE_IMAGE_FULL}).\033[0m"
        return 0
    fi

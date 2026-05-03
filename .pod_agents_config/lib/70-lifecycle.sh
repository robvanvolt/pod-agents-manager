    remove_all_managed_pods() {
        local wipe_workspace_data="$1" # true for delete, false for remove
        local action_label="remove"
        [ "$wipe_workspace_data" = "true" ] && action_label="delete"

        echo -e "\033[1;33mRunning '$action_label --all' across all managed pods...\033[0m"
        local found_any="false"

        for agent_dir in "$WORKSPACES_ROOT/"*-pods; do
            [ -d "$agent_dir" ] || continue
            local current_agent
            current_agent="$(basename "$agent_dir" | sed 's/-pods$//')"

            for inst_dir in "$agent_dir"/*/; do
                [ -d "$inst_dir" ] || continue
                found_any="true"
                local current_instance
                current_instance="$(basename "$inst_dir")"
                local current_service="${current_agent}@${current_instance}.service"

                echo -e "\033[36mDisabling $current_service...\033[0m"
                systemctl --user disable --now "$current_service" 2>/dev/null || true

                if [ "$wipe_workspace_data" = "true" ]; then
                    echo -e "\033[31mDeleting workspace: $inst_dir\033[0m"
                    rm -rf "$inst_dir"
                fi
            done
        done

        [ "$found_any" = "false" ] && echo -e "\033[33mNo managed pods found under $WORKSPACES_ROOT.\033[0m"
        echo -e "\033[32mCompleted '$action_label --all'.\033[0m"
        return 0
    }

    if { [ "$action" = "remove" ] || [ "$action" = "delete" ]; } && [ "$agent" = "--all" ]; then
        [ "$action" = "delete" ] && remove_all_managed_pods "true" || remove_all_managed_pods "false"
        return $?
    fi

    if { [ "$action" = "join" ] || [ "$action" = "enter" ] || [ "$action" = "it" ]; } && [ -z "$instance" ]; then
        local found_name=""
        if [ -n "$agent" ]; then
            found_name=$(podman ps --format '{{.Names}}' | grep "^${agent}-" | head -n 1)
        else
            for _a in "${available_agents[@]}"; do
                found_name=$(podman ps --format '{{.Names}}' | grep "^${_a}-" | head -n 1)
                [ -n "$found_name" ] && break
            done
        fi

        if [ -n "$found_name" ]; then
            if [ -z "$agent" ]; then
                for _a in "${available_agents[@]}"; do
                    if [[ "$found_name" == "${_a}-"* ]]; then
                        agent="$_a"
                        break
                    fi
                done
            fi
            instance="${found_name#${agent}-}"
            echo -e "\033[36mAuto-selected pod: ${found_name}\033[0m"
        else
            echo -e "\033[31mNo running pods found to $action.\033[0m"
            return 1
        fi
    fi

    if [ "$action" = "start" ] && [ -z "$instance" ] && [ -n "$agent" ]; then
        local names=("dev" "alpha" "beta" "gamma" "delta" "epsilon" "zeta" "eta" "theta" "iota" "kappa" "lambda" "mu" "nu" "xi" "omicron" "pi" "rho" "sigma" "tau" "upsilon" "phi" "chi" "psi" "omega")
        for n in "${names[@]}"; do
            if [ ! -d "$WORKSPACES_ROOT/${agent}-pods/$n" ]; then
                instance="$n"
                break
            fi
        done
        if [ -z "$instance" ]; then
             echo -e "\033[31mError: All auto-names (dev + greek letters) are taken.\033[0m"
             return 1
        fi
        echo -e "\033[36mAuto-selected instance name: ${instance}\033[0m"
    fi

    if [ -n "$agent" ] && [ "$action" != "tmux" ]; then
        if [ ! -f "$config_dir_agents/${agent}.sh" ]; then
            echo -e "\033[31mError: Agent config '${agent}.sh' not found in $config_dir_agents.\033[0m"
            return 1
        fi
        unset -f agent_build_containerfile agent_generate_config agent_pre_update 2>/dev/null || true
        unset AGENT_SKILLS_SUBPATH 2>/dev/null || true
        source "$config_dir_agents/${agent}.sh"
    fi

    local service_name="${agent}@${instance}.service"
    local container_name="${agent}-${instance}"
    local quadlet_file="$HOME/.config/containers/systemd/${agent}@.container"
    local workspace_root="$WORKSPACES_ROOT/${agent}-pods"
    local config_dir="$workspace_root/${instance}/config"
    local image_name="localhost/${agent}-agent-${flavor}-${BASE_IMAGE_TAG}:latest"
    local volume_pack="$volumes"

    ensure_quadlet_template() {
        if [ -z "$agent" ]; then
            echo -e "\033[31mensure_quadlet_template: refusing to write template with empty agent name.\033[0m" >&2
            return 1
        fi

        # --- THE BULLETPROOF VOLUME PARSER ---
        local safe_vol_dir="$config_dir_volumes"
        # Force fallback to "all" if $volume_pack is somehow empty or unset
        local safe_vpack="${volume_pack:-all}" 

        local dynamic_volumes=""
        if [ "$safe_vpack" != "none" ] && [ -d "$safe_vol_dir" ]; then
            local raw_lines=""
            if [ "$safe_vpack" = "all" ]; then
                # cat all files, use grep to strip comments and empty lines
                raw_lines=$(cat "$safe_vol_dir"/*.volumes 2>/dev/null | grep -E -v '^[[:space:]]*(#|$)')
            elif [ -f "$safe_vol_dir/${safe_vpack}.volumes" ]; then
                raw_lines=$(cat "$safe_vol_dir/${safe_vpack}.volumes" 2>/dev/null | grep -E -v '^[[:space:]]*(#|$)')
            fi

            # Safely append each line using a here-string (which natively fixes missing newlines!)
            if [ -n "$raw_lines" ]; then
                while IFS= read -r line; do
                    [ -n "$line" ] && dynamic_volumes+="Volume=${line}"$'\n'
                done <<< "$raw_lines"
            fi
        fi
        # -------------------------------------
        
        mkdir -p "$(dirname "$quadlet_file")"
        cat <<EOF > "$quadlet_file"
[Unit]
Description=${agent^} Agent Sandbox (%i)

[Container]
Image=${image_name}
Pull=never
ContainerName=${agent}-%i
Volume=${WORKSPACES_ROOT}/${agent}-pods/%i/workspace:/workspace:Z
Volume=${WORKSPACES_ROOT}/${agent}-pods/%i/config:${AGENT_VOLUME_CONFIG_PATH}:Z
Volume=%h/.pod_agents_config/skills:/srv/skills:ro,z
${dynamic_volumes}WorkingDir=/workspace
Environment=OPENAI_BASE_URL=${OPENAI_BASE_URL}
Environment=OPENAI_API_BASE=${OPENAI_BASE_URL}
Environment=OPENAI_API_KEY=${OPENAI_API_KEY}
Environment=ANTHROPIC_BASE_URL=${OPENAI_BASE_URL%/v1}
Environment=ANTHROPIC_API_KEY=${OPENAI_API_KEY}
Environment=ANTHROPIC_MODEL=${DEFAULT_MODEL}
Environment=LLM=${DEFAULT_MODEL}
NoNewPrivileges=true
Exec=sleep infinity

[Service]
TimeoutStartSec=15
ExecStartPre=/usr/bin/mkdir -p ${WORKSPACES_ROOT}/${agent}-pods/%i/workspace ${WORKSPACES_ROOT}/${agent}-pods/%i/config

[Install]
WantedBy=default.target
EOF
    }

    # 4. Action Execution Logic
    case "$action" in
        stats)
            if [ -n "$agent" ] && [ -n "$instance" ]; then
                echo -e "\033[36mStreaming stats for $container_name (Ctrl+C to exit)\033[0m"; podman stats "$container_name"
            else
                echo -e "\033[36mStreaming stats for all running pods (Ctrl+C to exit)\033[0m"; podman stats
            fi
            ;;

        tmux)
            local target_instance="$agent" # Shifted variable: 'pod tmux alpha' means $2 is the instance
            local target_pods=()
            
            # 1. Discovery Logic
            if [ -n "$target_instance" ]; then
                # User specified an exact instance name (e.g. 'alpha')
                for a in "${available_agents[@]}"; do
                    local p_name="${a}-${target_instance}"
                    # Only map if container actually exists and is running
                    if [ "$(podman inspect -f '{{.State.Running}}' "$p_name" 2>/dev/null)" == "true" ]; then
                        target_pods+=("$a:$target_instance")
                    fi
                done
            else
                # Default: Use all agents, but take only the FIRST running pod of each
                for a in "${available_agents[@]}"; do
                    local first_running=$(podman ps --format '{{.Names}}' | grep "^${a}-" | head -n 1)
                    if [ -n "$first_running" ]; then
                        local inst="${first_running#${a}-}"
                        target_pods+=("$a:$inst")
                    fi
                done
            fi

            local count=${#target_pods[@]}
            if [ "$count" -eq 0 ]; then
                echo -e "\033[31mNo running pods found to attach to.\033[0m"
                return 1
            fi

            if ! command -v tmux >/dev/null 2>&1; then
                echo -e "\033[31mtmux is not installed on this host.\033[0m"
                echo -e "  Install:  \033[36msudo apt install tmux\033[0m  (or your distro equivalent)"
                echo ""
                echo -e "\033[33mRunning pods:\033[0m"
                for _tp in "${target_pods[@]}"; do
                    echo "  ${_tp%:*}-${_tp#*:}"
                done
                echo ""
                echo -e "  Use \033[36mpod join <agent> <instance>\033[0m to connect to a pod directly."
                return 1
            fi

            local session_name="fleet_${target_instance:-grid_all}"
            
            # Kill existing session by this name if we are regenerating it
            tmux kill-session -t "$session_name" 2>/dev/null || true

            echo -e "\033[36mBuilding tmux grid for $count pods...\033[0m"

            for (( i=0; i<count; i++ )); do
                local curr_agent="${target_pods[$i]%:*}"
                local curr_inst="${target_pods[$i]#*:}"
                local c_name="${curr_agent}-${curr_inst}"
                
                # Trick to bypass intense nested quotes: auto-generating a self-destructing wrapper in /tmp
                local wrapper="/tmp/pod_tmux_${c_name}_${RANDOM}.sh"
                cat <<'EOF' > "$wrapper"
#!/bin/bash
podman exec -it -e TERM=xterm-256color -e COLORTERM=truecolor -e POD_AGENT="$1" "$2" bash -lc 'cmd="$POD_AGENT"; eval "$cmd" || true; exec bash'
rm -f "$0"
EOF
                chmod +x "$wrapper"

                if [ "$i" -eq 0 ]; then
                    tmux new-session -d -s "$session_name" -n "grid" "$wrapper \"$curr_agent\" \"$c_name\""
                else
                    tmux split-window -t "${session_name}:grid" "$wrapper \"$curr_agent\" \"$c_name\""
                    # Rebalance immediately to ensure we don't run out of space for the next split
                    tmux select-layout -t "${session_name}:grid" tiled
                fi
            done

            # 2. Grid Layout Engine
            if [ "$count" -le 3 ]; then
                # Lines them up side-by-side (1, 1-1, or 1-1-1)
                tmux select-layout -t "${session_name}:grid" even-horizontal
            else
                # Natively applies the perfect dynamic n-grid ratio based on terminal geometry (2x2, 3x2, 3x3, etc.)
                tmux select-layout -t "${session_name}:grid" tiled
            fi

            # 3. Connection
            if [ -n "$TMUX" ]; then
                tmux switch-client -t "$session_name"
            else
                tmux attach-session -t "$session_name"
            fi
            ;;

        update)
            local target_agents=("${available_agents[@]}")
            [ -n "$agent" ] && target_agents=("$agent")

            for a in "${target_agents[@]}"; do
                echo -e "\033[1;34m=== Updating Target: $a ===\033[0m"
                unset -f agent_build_containerfile agent_generate_config agent_pre_update 2>/dev/null || true
                unset AGENT_SKILLS_SUBPATH 2>/dev/null || true
                source "$config_dir_agents/${a}.sh"

                if declare -f agent_pre_update > /dev/null; then
                    echo -e "\033[36mRunning pre-update hooks for $a...\033[0m"
                    agent_pre_update
                fi
                
                local target_instances=()
                if [ -n "$instance" ] && [ "$agent" = "$a" ]; then
                    target_instances=("$instance")
                else
                    if [ -d "$WORKSPACES_ROOT/${a}-pods" ]; then
                        for d in "$WORKSPACES_ROOT/${a}-pods"/*/; do
                            [ -d "$d" ] || continue
                            target_instances+=("$(basename "$d")")
                        done
                    fi
                fi

                [ ${#target_instances[@]} -eq 0 ] && continue

                for inst in "${target_instances[@]}"; do
                    local current_image
                    current_image=$(podman inspect --format='{{.Config.Image}}' "${a}-${inst}" 2>/dev/null || echo "")
                    local detected_flavor="all"
                    local detected_base="$BASE_IMAGE_TAG"
                    # Image name pattern: localhost/<agent>-agent-<flavor>-<base>:latest
                    if [[ "$current_image" =~ -agent-([^-]+)-([^:]+):[^:]+$ ]]; then
                        detected_flavor="${BASH_REMATCH[1]}"
                        detected_base="${BASH_REMATCH[2]}"
                    fi

                    echo -e "\033[33mRemoving old image cache for ${a}-${detected_flavor}-${detected_base} to force rebuild...\033[0m"
                    podman image rm -f "localhost/${a}-agent-${detected_flavor}-${detected_base}:latest" 2>/dev/null || true
                    rm -rf "${IMAGE_CACHE_ROOT}/${a}-${detected_flavor}-${detected_base}" 2>/dev/null || true

                    echo -e "\033[32mApplying update and restarting instance: ${a}-${inst}...\033[0m"
                    _pod_agents_main restart "$a" "$inst" "$detected_flavor" "all" "$detected_base"
                done
            done
            echo -e "\033[1;32m🎉 Fleet update complete.\033[0m"
            ;;
            
        prebuild)
            local target_agents=("${available_agents[@]}")
            [ -n "$agent" ] && target_agents=("$agent")
            for a in "${target_agents[@]}"; do
                unset -f agent_build_containerfile agent_generate_config agent_pre_update 2>/dev/null || true
                unset AGENT_SKILLS_SUBPATH 2>/dev/null || true
                source "$config_dir_agents/${a}.sh"
                local pre_image="localhost/${a}-agent-${flavor}-${BASE_IMAGE_TAG}:latest"
                if podman image exists "$pre_image"; then
                    echo -e "\033[33m✓ ${a} (${flavor}/${BASE_IMAGE_TAG}) already built — skipping. Use 'update' to rebuild.\033[0m"
                    continue
                fi
                local pre_build_dir="${IMAGE_CACHE_ROOT}/${a}-${flavor}-${BASE_IMAGE_TAG}"
                echo -e "\033[36mPrebuilding ${a} (${flavor}/${BASE_IMAGE_TAG})...\033[0m"
                mkdir -p "$pre_build_dir"
                agent_build_containerfile "$pre_build_dir" "$flavor" "$BASE_IMAGE_FULL"
                podman build --layers -t "$pre_image" -f "$pre_build_dir/Containerfile" "$pre_build_dir" || continue
                echo -e "\033[32m✓ Prebuilt $pre_image\033[0m"
            done
            echo -e "\033[1;32m🎉 Prebuild complete. Subsequent 'pod start' is now near-instant.\033[0m"
            ;;

        start|restart)
            local build_dir="${IMAGE_CACHE_ROOT}/${agent}-${flavor}-${BASE_IMAGE_TAG}"
            if ! podman image exists "$image_name"; then
                echo -e "\033[36mLocal image not found. Building ${agent} (${flavor}/${BASE_IMAGE_TAG})...\033[0m"
                mkdir -p "$build_dir"
                agent_build_containerfile "$build_dir" "$flavor" "$BASE_IMAGE_FULL"
                podman build --layers -t "$image_name" -f "$build_dir/Containerfile" "$build_dir"
            fi

            # Render quadlet to a tmp file first; only daemon-reload if the file actually changed.
            # daemon-reload re-runs every user generator (incl. quadlet), which is the dominant cost in pod start.
            local quadlet_tmp="${quadlet_file}.new"
            local prev_quadlet_file="$quadlet_file"
            quadlet_file="$quadlet_tmp"
            ensure_quadlet_template || { quadlet_file="$prev_quadlet_file"; return 1; }
            quadlet_file="$prev_quadlet_file"
            local need_reload=0
            if ! cmp -s "$quadlet_tmp" "$quadlet_file" 2>/dev/null; then
                mv "$quadlet_tmp" "$quadlet_file"
                need_reload=1
            else
                rm -f "$quadlet_tmp"
            fi

            # Files are written as the host user (UID 1001 in rootless podman maps to container UID 0 = root inside pod), so
            # plain mkdir/rm/cp/ln are sufficient. `podman unshare` would only be needed to overwrite files that the
            # container itself wrote with non-root in-namespace UIDs — not the case for our config files.
            mkdir -p "$config_dir/agent" 2>/dev/null || true
            rm -f "$config_dir/config.toml" "$config_dir/crush.json" "$config_dir/settings.json" "$config_dir/agent/models.json" 2>/dev/null || true

            agent_generate_config "$config_dir" "$action"

            local skills_subpath="${AGENT_SKILLS_SUBPATH:-skills}"
            mkdir -p "$config_dir/$(dirname "$skills_subpath")" 2>/dev/null || true
            ln -sfn /srv/skills "$config_dir/$skills_subpath" 2>/dev/null || true

            [ "$need_reload" -eq 1 ] && systemctl --user daemon-reload
            [ "$action" != "update" ] && echo -e "\033[32mExecuting: systemctl --user $action $service_name\033[0m"
            systemctl --user "$action" "$service_name"
            ;;
            
        stop|status) systemctl --user "$action" "$service_name" ;;
        remove) systemctl --user disable --now "$service_name"; echo "Service disabled." ;;
        delete)
            echo -e "\033[33mStopping and disabling $service_name...\033[0m"
            systemctl --user disable --now "$service_name" 2>/dev/null || true
            if [ -n "$instance" ] && [[ "$instance" != *"/"* ]] && [[ "$instance" != *"."* ]]; then
                echo -e "\033[31mNuking workspace data at $workspace_root/${instance}...\033[0m"
                rm -rf "$workspace_root/${instance}"
            fi
            ;;
        join|enter)
            if [ -n "${MODEL_OVERRIDE:-}" ]; then
                echo -e "\033[36mApplying --model=\033[1m${MODEL_OVERRIDE}\033[0m\033[36m to ${container_name} (regenerating agent config)...\033[0m"
                rm -f "$config_dir/config.toml" "$config_dir/crush.json" "$config_dir/settings.json" "$config_dir/agent/models.json" 2>/dev/null || true
                agent_generate_config "$config_dir" "update"
                echo -e "\033[33m  → most agents pick this up on next prompt; if yours caches the model, run \`pod restart $agent $instance --model $MODEL_OVERRIDE\`.\033[0m"
            fi
            podman exec -it -e TERM=xterm-256color -e COLORTERM=truecolor -e POD_AGENT="$agent" -e DEFAULT_MODEL="$DEFAULT_MODEL" -e POD_DEFAULT_MODEL="$POD_DEFAULT_MODEL" "$container_name" bash -lc 'cmd="$POD_AGENT"; tmux has-session -t bot 2>/dev/null && exec tmux attach -t bot || exec tmux new-session -s bot "bash -lc \"$cmd || true; exec bash\""'
            ;;
        it)
            podman exec -it -e DEFAULT_MODEL="$DEFAULT_MODEL" -e POD_DEFAULT_MODEL="$POD_DEFAULT_MODEL" "$container_name" bash
            ;;
        *)
            echo "Unknown action: $action"
            _pod_print_help
            return 1
            ;;
    esac

    return 99  # sentinel: fell off end, continue to next lib

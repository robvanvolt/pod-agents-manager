    if [ "$#" -eq 0 ]; then
        local options=("start" "stop" "restart" "update" "self-update" "prebuild" "status" "stats" "remove" "delete" "remove-all" "delete-all" "join" "enter" "it" "tmux" "config" "batch" "server" "base" "cache-clean" "uninstall" "quit")
        local selected_action=""
        
        while true; do
            echo -e "\033[1;36m🛥️  Podman/Quadlet Fleet Manager\033[0m"
            for i in "${!options[@]}"; do
                printf "\033[36m%2d)\033[0m %-11s " "$((i+1))" "${options[$i]}"
                if [ $(( (i+1) % 4 )) -eq 0 ]; then echo ""; fi
            done
            if [ $(( ${#options[@]} % 4 )) -ne 0 ]; then echo ""; fi
            
            read -p "Select an action: " choice
            if ! [[ "$choice" =~ ^[0-9]+$ ]] || [ "$choice" -lt 1 ] || [ "$choice" -gt "${#options[@]}" ]; then
                echo -e "\033[31mInvalid option.\033[0m\n"
                continue
            fi
            selected_action="${options[$((choice-1))]}"
            break
        done

        case $selected_action in
            quit) echo "Exiting."; return 0 ;;
            cache-clean) pod cache-clean; return $? ;;
            remove-all) pod remove --all; return $? ;;
            delete-all) pod delete --all; return $? ;;
            self-update) pod self-update; return $? ;;
            config)
                pod config
                return $?
                ;;
            base)
                read -p "Set default base image (alpine, trixie-slim) [${BASE_IMAGE}]: " prompt_base
                prompt_base="${prompt_base:-$BASE_IMAGE}"
                pod base "$prompt_base"
                return $?
                ;;
            server)
                local server_options=("start" "stop" "restart" "status" "logs")
                echo -e "\033[36mServer action:\033[0m"
                local original_ps3="$PS3"
                PS3="Action: "
                select sa in "${server_options[@]}" "Cancel"; do
                    [ "$sa" = "Cancel" ] && { PS3="$original_ps3"; return 0; }
                    [ -n "$sa" ] && { PS3="$original_ps3"; pod server "$sa"; return $?; }
                done
                ;;
            batch)
                local batch_options=("run" "log" "tmux" "stats" "list" "stop")
                echo -e "\033[36mBatch action:\033[0m"
                local original_ps3="$PS3"
                PS3="Action: "
                select ba in "${batch_options[@]}" "Cancel"; do
                    [ "$ba" = "Cancel" ] && { PS3="$original_ps3"; return 0; }
                    [ -z "$ba" ] && continue
                    PS3="$original_ps3"
                    case "$ba" in
                        run)
                            local prompt_agent="" prompt_instance="" prompt_file=""
                            prompt_agent="$(pick_agent_or_all)" || return 0
                            [ -n "$prompt_agent" ] && read -p "Instance name (blank for all of agent): " prompt_instance
                            read -p "Path to prompts file: " prompt_file
                            read -p "Concurrent? [y/N]: " prompt_conc
                            local extra=""
                            [[ "$prompt_conc" =~ ^[yY] ]] && extra="--concurrent"
                            if [ -n "$prompt_instance" ]; then
                                pod batch "$prompt_agent" "$prompt_instance" "$prompt_file" $extra
                            elif [ -n "$prompt_agent" ]; then
                                pod batch "$prompt_agent" "$prompt_file" $extra
                            else
                                pod batch "$prompt_file" $extra
                            fi
                            return $?
                            ;;
                        log) pod batch log; return $? ;;
                        tmux|stats|list) pod batch "$ba"; return $? ;;
                        stop)
                            read -p "Batch id (blank to list): " bid
                            [ -z "$bid" ] && { pod batch list; return 0; }
                            pod batch stop "$bid"
                            return $?
                            ;;
                    esac
                done
                ;;
            tmux)
                echo -e "\033[36mEnter instance name for grid view (leave blank for the first pod of all agents):\033[0m"
                read -p "Instance name: " prompt_instance
                pod tmux "$prompt_instance"
                return $? 
                ;;
            update)
                echo -e "\033[36m(Leave blank to update ALL agents or ALL instances)\033[0m"
                local prompt_agent=""
                prompt_agent="$(pick_agent_or_all)" || { echo "Action canceled."; return 0; }
                local prompt_instance=""
                [ -n "$prompt_agent" ] && read -p "Instance name: " prompt_instance
                pod update "$prompt_agent" "$prompt_instance"
                return $?
                ;;
            prebuild)
                echo -e "\033[36m(Leave blank to prebuild ALL agents)\033[0m"
                local prompt_agent=""
                prompt_agent="$(pick_agent_or_all)" || { echo "Action canceled."; return 0; }
                local flavor_hints="${available_flavors[*]} all"
                read -p "Flavor (${flavor_hints// /, }) [all]: " prompt_flavor
                prompt_flavor="${prompt_flavor:-all}"
                local vol_hints="${available_volumes[*]} all none"
                read -p "Volumes (${vol_hints// /, }) [all]: " prompt_volumes
                prompt_volumes="${prompt_volumes:-all}"
                read -p "Base image (alpine, trixie-slim) [${BASE_IMAGE}]: " prompt_base
                prompt_base="${prompt_base:-$BASE_IMAGE}"
                pod prebuild "$prompt_agent" "" "$prompt_flavor" "$prompt_volumes" "$prompt_base"
                return $?
                ;;
            start)
                echo -e "\033[36mCreate or start a pod:\033[0m"
                local prompt_agent=""
                prompt_agent="$(pick_agent_start)" || { echo "Action canceled."; return 0; }

                read -p "Instance name (leave blank for auto): " prompt_instance

                local flavor_hints="${available_flavors[*]} all"
                read -p "Flavor (${flavor_hints// /, }) [all]: " prompt_flavor
                prompt_flavor="${prompt_flavor:-all}"
                local vol_hints="${available_volumes[*]} all none"
                read -p "Volumes (${vol_hints// /, }) [all]: " prompt_volumes
                prompt_volumes="${prompt_volumes:-all}"
                read -p "Base image (alpine, trixie-slim) [${BASE_IMAGE}]: " prompt_base
                prompt_base="${prompt_base:-$BASE_IMAGE}"

                if [ "$prompt_agent" = "All" ]; then
                    for a in "${available_agents[@]}"; do
                        echo -e "\033[1;32mStarting ${a}...\033[0m"
                        pod start "$a" "$prompt_instance" "$prompt_flavor" "$prompt_volumes" "$prompt_base"
                    done
                else
                    pod start "$prompt_agent" "$prompt_instance" "$prompt_flavor" "$prompt_volumes" "$prompt_base"
                fi
                return $?
                ;;
            stop|restart|status|stats|remove|delete|join|enter|it)
                local available_pods=()
                [ "$selected_action" = "stats" ] && available_pods+=("All")

                for agent_dir in "$WORKSPACES_ROOT/"*-pods; do
                    [ -d "$agent_dir" ] || continue
                    local current_agent=$(basename "$agent_dir" | sed 's/-pods//')
                    for inst_dir in "$agent_dir"/*/; do
                        [ -d "$inst_dir" ] || continue
                        available_pods+=("${current_agent} $(basename "$inst_dir")")
                    done
                done

                if [ ${#available_pods[@]} -eq 0 ] && [ "$selected_action" != "stats" ]; then
                    echo -e "\033[33mNo existing pods found.\033[0m"; return 0
                fi

                echo -e "\033[36mSelect target pod for '$selected_action':\033[0m"
                local original_ps3="$PS3"
                PS3="Pod number: "
                select selected_pod in "${available_pods[@]}" "Cancel"; do
                    if [ "$selected_pod" == "Cancel" ]; then echo "Action canceled."; break
                    elif [ "$selected_pod" == "All" ]; then pod stats; return $?
                    elif [ -n "$selected_pod" ]; then
                        pod "$selected_action" "${selected_pod%% *}" "${selected_pod#* }"
                        return $?
                    else echo -e "\033[31mInvalid selection.\033[0m"; fi
                done
                PS3="$original_ps3"
                ;;
        esac
        return 0
    fi

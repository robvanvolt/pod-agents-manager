    # Resolve a base shorthand or explicit tag to (full_image, short_tag).
    # Used for both the Containerfile FROM line and image-name tagging.
    _resolve_base_image() {
        local raw="${1:-$BASE_IMAGE}"
        case "$raw" in
            ""|alpine|node:current-alpine) BASE_IMAGE_FULL="docker.io/library/node:current-alpine"; BASE_IMAGE_TAG="alpine" ;;
            trixie|trixie-slim|debian|slim|node:current-trixie-slim) BASE_IMAGE_FULL="docker.io/library/node:current-trixie-slim"; BASE_IMAGE_TAG="trixie" ;;
            *) BASE_IMAGE_FULL="$raw"; BASE_IMAGE_TAG="$(echo "$raw" | tr '/:' '__')" ;;
        esac
    }

    # Dynamically scan for agents and flavors
    local available_agents=()
    for f in "$config_dir_agents"/*.sh; do
        [ -f "$f" ] || continue
        available_agents+=("$(basename "$f" .sh)")
    done
    
    local available_flavors=()
    for f in "$config_dir_flavors"/*.containerfile; do
        [ -f "$f" ] || continue
        available_flavors+=("$(basename "$f" .containerfile)")
    done

    local available_volumes=()
    for f in "$config_dir_volumes"/*.volumes; do
        [ -f "$f" ] || continue
        available_volumes+=("$(basename "$f" .volumes)")
    done

    pick_agent_required() {
        if [ ${#available_agents[@]} -eq 0 ]; then
            echo -e "\033[31mNo agent plugins found in $config_dir_agents.\033[0m" >&2
            return 1
        fi
        echo -e "\033[36mSelect an agent:\033[0m" >&2
        local original_ps3="$PS3"
        PS3="Agent number: "
        select selected_agent in "${available_agents[@]}" "Cancel"; do
            if [ "$selected_agent" = "Cancel" ]; then
                PS3="$original_ps3"
                return 1
            elif [ -n "$selected_agent" ]; then
                PS3="$original_ps3"
                printf '%s\n' "$selected_agent"
                return 0
            else
                echo -e "\033[31mInvalid selection.\033[0m" >&2
            fi
        done
    }

    pick_agent_start() {
        if [ ${#available_agents[@]} -eq 0 ]; then
            echo -e "\033[31mNo agent plugins found in $config_dir_agents.\033[0m" >&2
            return 1
        fi
        echo -e "\033[36mSelect an agent:\033[0m" >&2
        local original_ps3="$PS3"
        PS3="Agent number: "
        select selected_agent in "${available_agents[@]}" "All" "Cancel"; do
            if [ "$selected_agent" = "Cancel" ]; then
                PS3="$original_ps3"
                return 1
            elif [ -n "$selected_agent" ]; then
                PS3="$original_ps3"
                printf '%s\n' "$selected_agent"
                return 0
            else
                echo -e "\033[31mInvalid selection.\033[0m" >&2
            fi
        done
    }

    pick_agent_or_all() {
        if [ ${#available_agents[@]} -eq 0 ]; then
            echo ""
            return 0
        fi
        echo -e "\033[36mSelect an agent to update (or All):\033[0m" >&2
        local original_ps3="$PS3"
        PS3="Agent number: "
        select selected_agent in "All" "${available_agents[@]}" "Cancel"; do
            if [ "$selected_agent" = "Cancel" ]; then
                PS3="$original_ps3"
                return 1
            elif [ "$selected_agent" = "All" ]; then
                PS3="$original_ps3"
                echo ""
                return 0
            elif [ -n "$selected_agent" ]; then
                PS3="$original_ps3"
                printf '%s\n' "$selected_agent"
                return 0
            else
                echo -e "\033[31mInvalid selection.\033[0m" >&2
            fi
        done
    }

    # Helper: Shared Node + Dynamic Flavors Base Builder
    write_base_node_containerfile() {
        local build_dir="$1"
        local flavor="$2"
        local base_in="${3:-$BASE_IMAGE}"

        _resolve_base_image "$base_in"

        if [[ "$BASE_IMAGE_FULL" == *alpine* ]]; then
            cat <<EOF > "$build_dir/Containerfile"
FROM ${BASE_IMAGE_FULL}
RUN apk add --no-cache bash tmux curl unzip git ripgrep fd
ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8
WORKDIR /workspace
EOF
        else
            cat <<EOF > "$build_dir/Containerfile"
FROM ${BASE_IMAGE_FULL}
RUN DEBIAN_FRONTEND=noninteractive apt-get update && \\
    apt-get install -y tmux curl unzip git bash locales ripgrep fd-find && \\
    ln -sf "\$(command -v fdfind)" /usr/local/bin/fd && \\
    apt-get clean && rm -rf /var/lib/apt/lists/* && \\
    sed -i -e 's/# en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen && \\
    dpkg-reconfigure --frontend=noninteractive locales
ENV LANG=en_US.UTF-8
ENV LC_ALL=en_US.UTF-8
WORKDIR /workspace
EOF
        fi

        # Dynamically inject flavor snippets
        if [ "$flavor" = "all" ] || [ "$flavor" = "combo" ]; then
            for f in "$config_dir_flavors"/*.containerfile; do
                [ -f "$f" ] && { cat "$f"; echo ""; } >> "$build_dir/Containerfile"
            done
        elif [ -n "$flavor" ] && [ -f "$config_dir_flavors/${flavor}.containerfile" ]; then
            cat "$config_dir_flavors/${flavor}.containerfile" >> "$build_dir/Containerfile"
            echo "" >> "$build_dir/Containerfile"
        fi

        # TrueColor tmux config without "set -g extended-keys-format csi-u"
        cat <<'EOF' > "$build_dir/tmux.conf"
set -g default-terminal "tmux-256color"
set -ag terminal-overrides ",xterm-256color:RGB"
set -g extended-keys on
set -g extended-keys-format csi-u
set -g focus-events on
EOF
        echo "COPY tmux.conf /etc/tmux.conf" >> "$build_dir/Containerfile"
    }

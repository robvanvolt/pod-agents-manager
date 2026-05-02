    _pod_write_env_file() {
        local target_file="$1"
        cat <<EOF > "$target_file"
POD_OPENAI_BASE_URL="${POD_OPENAI_BASE_URL}"
POD_OPENAI_API_KEY="${POD_OPENAI_API_KEY}"
POD_DEFAULT_MODEL="${POD_DEFAULT_MODEL}"
POD_IMAGE_CACHE_ROOT="${POD_IMAGE_CACHE_ROOT}"
POD_WORKSPACES_ROOT="${POD_WORKSPACES_ROOT}"
# Base image used for all builds. Pick one of: alpine, trixie-slim
POD_BASE_IMAGE="${POD_BASE_IMAGE}"
EOF
    }

    _pod_env_value_needs_setup() {
        local value="$1"
        case "$value" in
            ""|"<"*|*">"|CHANGE_ME|change-me|changeme|__SET_ME__)
                return 0
                ;;
        esac
        return 1
    }

    _pod_prompt_yes_no() {
        local prompt="$1"
        local default_answer="${2:-N}"
        local reply=""

        [ -r /dev/tty ] && [ -w /dev/tty ] || return 1

        while true; do
            printf '%s [%s]: ' "$prompt" "$default_answer" > /dev/tty
            IFS= read -r reply < /dev/tty || return 1
            reply="${reply:-$default_answer}"
            case "$reply" in
                y|Y|yes|YES) return 0 ;;
                n|N|no|NO) return 1 ;;
            esac
        done
    }

    _pod_prompt_value() {
        local prompt="$1"
        local current_value="$2"
        local secret="${3:-0}"
        local reply=""

        [ -r /dev/tty ] && [ -w /dev/tty ] || return 1

        if [ "$secret" = "1" ]; then
            if [ -n "$current_value" ]; then
                printf '%s [hidden]: ' "$prompt" > /dev/tty
            else
                printf '%s: ' "$prompt" > /dev/tty
            fi
            IFS= read -r -s reply < /dev/tty || return 1
            printf '\n' > /dev/tty
        else
            if [ -n "$current_value" ]; then
                printf '%s [%s]: ' "$prompt" "$current_value" > /dev/tty
            else
                printf '%s: ' "$prompt" > /dev/tty
            fi
            IFS= read -r reply < /dev/tty || return 1
        fi

        printf '%s' "${reply:-$current_value}"
    }

    _pod_configure_env() {
        local mode="${1:-interactive}"
        local changed=0
        local var_name prompt fallback secret current display_value missing should_prompt new_value

        if [ ! -r /dev/tty ] || [ ! -w /dev/tty ]; then
            echo -e "\033[31mInteractive configuration requires a terminal. Edit $pod_env_file directly or run 'pod config' from a shell.\033[0m" >&2
            return 1
        fi

        printf '\033[1;36mConfiguring pod-agents-manager env at %s\033[0m\n' "$pod_env_file" > /dev/tty

        for var_name in POD_OPENAI_BASE_URL POD_OPENAI_API_KEY POD_DEFAULT_MODEL POD_IMAGE_CACHE_ROOT POD_WORKSPACES_ROOT POD_BASE_IMAGE; do
            case "$var_name" in
                POD_OPENAI_BASE_URL)
                    prompt="OpenAI-compatible base URL"
                    fallback="http://192.168.178.67:8008/v1"
                    secret=0
                    ;;
                POD_OPENAI_API_KEY)
                    prompt="OpenAI API key"
                    fallback="rms-omlx"
                    secret=1
                    ;;
                POD_DEFAULT_MODEL)
                    prompt="Default model"
                    fallback="Qwen3.6-35B-A3B-8bit"
                    secret=0
                    ;;
                POD_IMAGE_CACHE_ROOT)
                    prompt="Image cache root"
                    fallback="$HOME/.cache/podman-containers"
                    secret=0
                    ;;
                POD_WORKSPACES_ROOT)
                    prompt="Agent workspaces root"
                    fallback="$HOME/Developer"
                    secret=0
                    ;;
                POD_BASE_IMAGE)
                    prompt="Base image (alpine, trixie-slim, or explicit image)"
                    fallback="alpine"
                    secret=0
                    ;;
            esac

            current="${!var_name:-}"
            [ -n "$current" ] || current="$fallback"
            if _pod_env_value_needs_setup "${!var_name:-}"; then
                missing=1
            else
                missing=0
            fi

            should_prompt=0
            case "$mode" in
                missing)
                    [ "$missing" -eq 1 ] && should_prompt=1
                    ;;
                interactive)
                    if [ "$missing" -eq 1 ]; then
                        should_prompt=1
                    else
                        display_value="$current"
                        [ "$secret" = "1" ] && display_value="<set>"
                        if _pod_prompt_yes_no "$prompt is currently ${display_value}. Change it?" "N"; then
                            should_prompt=1
                        fi
                    fi
                    ;;
                *)
                    echo -e "\033[31mUnknown config mode: $mode\033[0m" >&2
                    return 1
                    ;;
            esac

            [ "$should_prompt" -eq 1 ] || continue

            while true; do
                new_value="$(_pod_prompt_value "$prompt" "$current" "$secret")" || return 1
                if [ -n "$new_value" ]; then
                    break
                fi
                printf 'A value is required for %s.\n' "$prompt" > /dev/tty
            done
            printf -v "$var_name" '%s' "$new_value"
            changed=1
        done

        if [ "$changed" -eq 1 ]; then
            _pod_write_env_file "$pod_env_file"
            # shellcheck disable=SC1090
            source "$pod_env_file"
            OPENAI_BASE_URL="$POD_OPENAI_BASE_URL"
            OPENAI_API_KEY="$POD_OPENAI_API_KEY"
            DEFAULT_MODEL="$POD_DEFAULT_MODEL"
            IMAGE_CACHE_ROOT="$POD_IMAGE_CACHE_ROOT"
            WORKSPACES_ROOT="$POD_WORKSPACES_ROOT"
            BASE_IMAGE="$POD_BASE_IMAGE"
            printf '\033[32mSaved configuration to %s\033[0m\n' "$pod_env_file" > /dev/tty
        elif [ "$mode" = "missing" ]; then
            printf '\033[36mNo missing POD_* values detected in %s\033[0m\n' "$pod_env_file" > /dev/tty
        else
            printf '\033[36mNo changes made.\033[0m\n' > /dev/tty
        fi

        return 0
    }

    _pod_has_missing_env() {
        local var_name
        for var_name in POD_OPENAI_BASE_URL POD_OPENAI_API_KEY POD_DEFAULT_MODEL POD_IMAGE_CACHE_ROOT POD_BASE_IMAGE; do
            if _pod_env_value_needs_setup "${!var_name:-}"; then
                return 0
            fi
        done
        return 1
    }

    if [ -f "$pod_env_file" ]; then
        source "$pod_env_file"
    else
        : "${POD_OPENAI_BASE_URL:=http://192.168.178.67:8008/v1}"
        : "${POD_OPENAI_API_KEY:=rms-omlx}"
        : "${POD_DEFAULT_MODEL:=Qwen3.6-35B-A3B-8bit}"
        : "${POD_IMAGE_CACHE_ROOT:=$HOME/.cache/podman-containers}"
        : "${POD_WORKSPACES_ROOT:=$HOME/Developer}"
        : "${POD_BASE_IMAGE:=alpine}"
        _pod_write_env_file "$pod_env_file"
    fi

    [ -f "$pod_env_example_file" ] || _pod_write_env_file "$pod_env_example_file"

    : "${POD_OPENAI_BASE_URL:=${OPENAI_BASE_URL:-http://192.168.178.67:8008/v1}}"
    : "${POD_OPENAI_API_KEY:=${OPENAI_API_KEY:-rms-omlx}}"
    : "${POD_DEFAULT_MODEL:=${DEFAULT_MODEL:-Qwen3.6-35B-A3B-8bit}}"
    : "${POD_IMAGE_CACHE_ROOT:=${IMAGE_CACHE_ROOT:-$HOME/.cache/podman-containers}}"
    : "${POD_WORKSPACES_ROOT:=${WORKSPACES_ROOT:-$HOME/Developer}}"
    : "${POD_BASE_IMAGE:=${BASE_IMAGE:-alpine}}"

    OPENAI_BASE_URL="$POD_OPENAI_BASE_URL"
    OPENAI_API_KEY="$POD_OPENAI_API_KEY"
    DEFAULT_MODEL="$POD_DEFAULT_MODEL"
    IMAGE_CACHE_ROOT="$POD_IMAGE_CACHE_ROOT"
    WORKSPACES_ROOT="$POD_WORKSPACES_ROOT"
    BASE_IMAGE="$POD_BASE_IMAGE"

    local initial_action="${1:-}"
    local should_auto_config=1
    case "$initial_action" in
        config|-h|--help|help|-v|--version|version)
            should_auto_config=0
            ;;
    esac
    if [ "$should_auto_config" -eq 1 ] && [ -r /dev/tty ] && [ -w /dev/tty ] && _pod_has_missing_env; then
        _pod_configure_env "missing" || return 1
    fi

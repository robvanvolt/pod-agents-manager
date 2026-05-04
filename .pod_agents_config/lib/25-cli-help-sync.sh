    # Version string, help text, and self-update file sync helpers
    if [ -f "$pod_version_file" ]; then
        # shellcheck disable=SC1090
        source "$pod_version_file"
    fi
    : "${POD_AGENTS_VERSION:=$pod_version_default}"

    _pod_print_version() {
        printf 'pod-agents-manager %s\n' "$POD_AGENTS_VERSION"
    }

    _pod_print_help() {
        local _cmd="${user_cmd_name:-pod}"
        cat <<EOF
pod-agents-manager ${POD_AGENTS_VERSION}

Usage:
  ${_cmd} [--help|-h]
  ${_cmd} [--version|-v]
  ${_cmd} <action> [agent] [instance] [flavor] [volumes] [base]
            [--model NAME] [--endpoint URL] [--api-key KEY]
            [--ports HOST:CONTAINER[,...]]

Actions:
  lifecycle    start stop restart status stats remove delete remove-all delete-all
  interaction  join enter it tmux config
  images       prebuild update self-update cache-clean base
  batch        batch [log [id]|tmux|stats|list|stop <id>|...]
  server       server {start|stop|restart|status|logs|build}
  diagnostics  doctor
  uninstall    uninstall

Examples:
  ${_cmd} config
  ${_cmd} doctor
  ${_cmd} self-update
  ${_cmd} start pi dev all all alpine
  ${_cmd} start pi dev --model my-other-model
  ${_cmd} enter pi dev --endpoint http://192.168.1.10:8000/v1 --api-key sk-...
  ${_cmd} start pi dev --endpoint http://127.0.0.1:8000/v1 --api_key sk-local
  ${_cmd} join pi dev
  ${_cmd} restart opencode dev --ports 3000:3000
  ${_cmd} batch prompts.txt --concurrent
  ${_cmd} batch log
  ${_cmd} server start
  ${_cmd} uninstall
  ${_cmd} --version
EOF
    }

    _pod_require_cmd() {
        command -v "$1" >/dev/null 2>&1 || {
            echo -e "\033[31mMissing required command: $1\033[0m" >&2
            return 1
        }
    }

    _pod_merge_tree() {
        local src="$1"
        local dest="$2"
        [ -d "$src" ] || return 0
        mkdir -p "$dest"
        cp -R "$src"/. "$dest"/
    }

    _pod_fetch_repo_snapshot() {
        local tmp_dir="$1"
        local repo="${POD_AGENTS_REPO:-robvanvolt/pod-agents-manager}"
        local ref="${POD_AGENTS_REF:-main}"
        local archive_url="${POD_AGENTS_ARCHIVE_URL:-https://codeload.github.com/${repo}/tar.gz/refs/heads/${ref}}"

        _pod_require_cmd curl || return 1
        _pod_require_cmd tar || return 1

        curl -fsSL "$archive_url" | tar -xzf - -C "$tmp_dir" || return 1
        find "$tmp_dir" -mindepth 1 -maxdepth 1 -type d | head -n 1
    }

    _pod_sync_managed_files() {
        local src_root="$1"

        [ -f "$src_root/.pod_agents" ] || {
            echo -e "\033[31mDownloaded archive is missing .pod_agents.\033[0m" >&2
            return 1
        }

        mkdir -p "$config_dir_root"
        cp "$src_root/.pod_agents" "$HOME/.pod_agents"
        [ -f "$config_dir_root/.env" ] || cp "$src_root/.pod_agents_config/.env.example" "$config_dir_root/.env"
        cp "$src_root/.pod_agents_config/.env.example" "$config_dir_root/.env.example"
        cp "$src_root/.pod_agents_config/version.conf" "$config_dir_root/version.conf"
        _pod_merge_tree "$src_root/.pod_agents_config/agents" "$config_dir_agents"
        _pod_merge_tree "$src_root/.pod_agents_config/flavors" "$config_dir_flavors"
        _pod_merge_tree "$src_root/.pod_agents_config/volumes" "$config_dir_volumes"
        _pod_merge_tree "$src_root/.pod_agents_config/skills" "$config_dir_skills"
        _pod_merge_tree "$src_root/.pod_agents_config/lib" "$config_dir_root/lib"
        _pod_merge_tree "$src_root/.pod_agents_config/server" "$config_dir_root/server"
        rm -f "$config_dir_root/server/static/favicon.ico"
    }

    return 99  # sentinel: fell off end, continue to next lib

    if [ "$action" = "server" ]; then
        local sub="${2:-status}"
        local server_dir="$config_dir_root/server"
        local server_port="${POD_SERVER_PORT:-1337}"
        local server_bin="$server_dir/server"
        local server_pid_file="$server_dir/server.pid"
        local server_log_file="$server_dir/server.log"
        local builder_image="docker.io/library/golang:1.23-alpine"

        # Returns one IPv4 per line. IPv6 is intentionally skipped here — most LAN
        # consumers want a v4 address, and v6 link-local addresses (fe80::) aren't
        # reachable from peers anyway.
        _pod_server_ips() {
            local ips=()
            if command -v ip &>/dev/null; then
                while IFS= read -r ip; do
                    [ -n "$ip" ] && ips+=("$ip")
                done < <(ip -4 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1)
            fi
            if [ ${#ips[@]} -eq 0 ] && command -v hostname &>/dev/null; then
                local h_ips
                h_ips=$(hostname -I 2>/dev/null) || h_ips=""
                for ip in $h_ips; do
                    case "$ip" in *:*) ;; *) ips+=("$ip") ;; esac  # skip v6
                done
            fi
            if [ ${#ips[@]} -eq 0 ] && command -v ifconfig &>/dev/null; then
                while IFS= read -r ip; do
                    [ -n "$ip" ] && [ "$ip" != "127.0.0.1" ] && ips+=("$ip")
                done < <(ifconfig 2>/dev/null | awk '/inet /{print $2}')
            fi
            printf '%s\n' "${ips[@]}"
        }

        _pod_server_pid_alive() {
            [ -f "$server_pid_file" ] || return 1
            local p; p=$(cat "$server_pid_file" 2>/dev/null)
            [ -n "$p" ] && kill -0 "$p" 2>/dev/null
        }

        # Find any process running our server binary, regardless of pid-file state.
        # Returns one PID per line on stdout.
        _pod_server_orphan_pids() {
            # pgrep -f matches the full command line; -x would require exact match which we don't want.
            pgrep -f "^${server_bin}( |$)" 2>/dev/null
        }

        _pod_server_port_holder() {
            # Returns the PID currently bound to $server_port, if any.
            if command -v ss >/dev/null 2>&1; then
                ss -ltnp "sport = :${server_port}" 2>/dev/null \
                    | awk -F 'pid=' 'NF>1 {print $2}' \
                    | awk -F ',' '{print $1}' \
                    | head -n 1
            elif command -v lsof >/dev/null 2>&1; then
                lsof -nP -iTCP:"${server_port}" -sTCP:LISTEN 2>/dev/null \
                    | awk 'NR>1 {print $2; exit}'
            fi
        }

        _pod_server_kill_pid() {
            local p="$1"
            [ -z "$p" ] && return 0
            kill "$p" 2>/dev/null || true
            for _ in 1 2 3 4 5 6 7 8; do
                kill -0 "$p" 2>/dev/null || return 0
                sleep 0.2
            done
            kill -9 "$p" 2>/dev/null || true
        }

        _pod_server_build() {
            local force="${1:-0}"
            if [ "$force" != "1" ] && [ -x "$server_bin" ] && [ "$server_bin" -nt "$server_dir/main.go" ]; then
                return 0
            fi
            echo -e "\033[36mBuilding dashboard binary via $builder_image (host has no Go toolchain assumed)...\033[0m"
            # Build with a throwaway builder container; output the static binary to the host fs.
            # `podman unshare chown` is unnecessary because we mount the dir as the user namespace already maps it.
            if command -v go >/dev/null 2>&1; then
                ( cd "$server_dir" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o server main.go ) || return 1
            else
                podman run --rm \
                    -v "$server_dir":/src:Z \
                    -w /src \
                    -e CGO_ENABLED=0 \
                    "$builder_image" \
                    sh -c 'go build -trimpath -ldflags="-s -w" -o server main.go' || return 1
            fi
            chmod +x "$server_bin"
        }

        case "$sub" in
            start)
                [ -d "$server_dir" ] || { echo -e "\033[31mServer directory not found at $server_dir.\033[0m"; return 1; }
                [ -f "$server_dir/main.go" ] || { echo -e "\033[31mmain.go not found in $server_dir.\033[0m"; return 1; }

                if _pod_server_pid_alive; then
                    echo -e "\033[33mDashboard already running (pid $(cat "$server_pid_file")). Use 'pod server restart'.\033[0m"
                    return 0
                fi

                # Migration: clean up the legacy container-in-container dashboard if it's still there.
                if podman container exists pod-dashboard 2>/dev/null; then
                    echo -e "\033[33mRemoving legacy 'pod-dashboard' container (the dashboard now runs natively on the host)...\033[0m"
                    podman rm -f pod-dashboard >/dev/null 2>&1 || true
                    podman image rm -f localhost/pod-dashboard:latest >/dev/null 2>&1 || true
                fi

                # Reap orphans from earlier launches (older versions used setsid which didn't
                # preserve $! correctly, so the pid file pointed at a dead pid while the real
                # server process kept holding the port).
                local orphan
                while IFS= read -r orphan; do
                    [ -z "$orphan" ] && continue
                    echo -e "\033[33mKilling orphan dashboard process $orphan...\033[0m"
                    _pod_server_kill_pid "$orphan"
                done < <(_pod_server_orphan_pids)

                # If the port is still held by something else, refuse to start with a clear pointer.
                local holder; holder=$(_pod_server_port_holder)
                if [ -n "$holder" ]; then
                    echo -e "\033[31mPort $server_port is held by pid $holder. Resolve manually (e.g. \`kill $holder\`) and retry.\033[0m"
                    return 1
                fi

                _pod_server_build || return 1

                # Visual separator in the log so each run is easy to spot when tailing.
                printf '\n=== %s — pod server start ===\n' "$(date -Iseconds 2>/dev/null || date)" >> "$server_log_file"

                # Run natively on the host so it uses host podman directly. No nesting, no socket mount.
                # `nohup` execs the binary in-place (preserving $!) and ignores SIGHUP; `disown` removes
                # it from the shell's job table so the parent shell exiting won't tear it down.
                # Monitor mode is disabled around the launch so bash doesn't print "[1] <pid>".
                local _prev_monitor=0
                case "$-" in *m*) _prev_monitor=1 ;; esac
                set +m
                cd "$server_dir" || { [ "$_prev_monitor" = "1" ] && set -m; return 1; }
                POD_SERVER_PORT="$server_port" nohup "$server_bin" >>"$server_log_file" 2>&1 </dev/null &
                local server_pid=$!
                disown 2>/dev/null || true
                cd - >/dev/null 2>&1 || true
                [ "$_prev_monitor" = "1" ] && set -m
                echo "$server_pid" > "$server_pid_file"

                # Brief wait so the log line / port-bind error surfaces if it dies on launch.
                sleep 0.4
                if ! _pod_server_pid_alive; then
                    echo -e "\033[31mDashboard failed to start. Last log lines:\033[0m"
                    tail -n 20 "$server_log_file" 2>/dev/null
                    rm -f "$server_pid_file"
                    return 1
                fi

                echo -e "\033[1;32mDashboard running (pid $(cat "$server_pid_file")):\033[0m"
                local printed=0
                while IFS= read -r ip; do
                    [ -z "$ip" ] && continue
                    printf "  \033[36mhttp://%s:%s\033[0m\n" "$ip" "$server_port"
                    printed=1
                done < <(_pod_server_ips)
                [ "$printed" -eq 0 ] && printf "  \033[36mhttp://localhost:%s\033[0m  (no LAN IPs detected)\n" "$server_port"
                echo -e "  logs: \033[36mpod server logs\033[0m"
                return 0
                ;;
            stop)
                local killed_any=0
                if _pod_server_pid_alive; then
                    _pod_server_kill_pid "$(cat "$server_pid_file")"
                    killed_any=1
                fi
                rm -f "$server_pid_file"

                # Always sweep for orphan processes too (handles pid-file drift from previous versions).
                local orphan
                while IFS= read -r orphan; do
                    [ -z "$orphan" ] && continue
                    _pod_server_kill_pid "$orphan"
                    killed_any=1
                done < <(_pod_server_orphan_pids)

                # Last-resort: anything else holding our port (e.g. a manually-started binary).
                local holder; holder=$(_pod_server_port_holder)
                if [ -n "$holder" ]; then
                    echo -e "\033[33mPort $server_port still held by pid $holder; killing.\033[0m"
                    _pod_server_kill_pid "$holder"
                    killed_any=1
                fi

                if [ "$killed_any" = "1" ]; then
                    echo -e "\033[32mDashboard stopped.\033[0m"
                else
                    echo -e "\033[33mDashboard not running.\033[0m"
                fi
                return 0
                ;;
            restart)
                pod server stop
                pod server start
                return $?
                ;;
            status)
                if _pod_server_pid_alive; then
                    echo -e "\033[32mRunning (pid $(cat "$server_pid_file"))\033[0m"
                    while IFS= read -r ip; do
                        [ -n "$ip" ] && printf "  http://%s:%s\n" "$ip" "$server_port"
                    done < <(_pod_server_ips)
                else
                    echo -e "\033[33mNot running\033[0m"
                    echo ""
                    echo -e "  \033[36mpod server start\033[0m    — build & start the dashboard on :${server_port}"
                    echo -e "  \033[36mpod server build\033[0m    — (re)build the Go binary without starting"
                    echo -e "  \033[36mpod server logs\033[0m     — follow the server log file"
                fi
                return 0
                ;;
            logs)
                [ -f "$server_log_file" ] || { echo -e "\033[33mNo log file yet.\033[0m"; return 0; }
                tail -F "$server_log_file"
                return $?
                ;;
            build)
                _pod_server_build 1
                return $?
                ;;
            *)
                echo "Usage: pod server {start|stop|restart|status|logs|build}"
                return 1
                ;;
        esac
    fi

    return 99  # sentinel: fell off end, continue to next lib

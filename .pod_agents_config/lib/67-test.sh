    # On-side tests: smoke-test every configured agent against the dashboard's
    # built-in OpenAI/Anthropic sham endpoints. Each agent runs against a
    # dedicated `<agent>-shamtest` pod whose config points at the sham server,
    # so a passing test means the agent CLI actually completes a round-trip
    # with whatever local inference server config it would use in production.
    #
    # Output is parsed for the literal "This request succeeded" reply that the
    # sham server always returns.

    if [ "$action" = "test" ]; then
        local _all=0 _setup=1 _teardown=0 _endpoint=""
        local _positional=()
        local _i=0 _argv=("${@:2}")
        while [ "$_i" -lt "${#_argv[@]}" ]; do
            case "${_argv[$_i]}" in
                --all) _all=1 ;;
                --no-setup) _setup=0 ;;
                --setup) _setup=1 ;;
                --teardown) _teardown=1 ;;
                --keep) _teardown=0 ;;
                --endpoint=*) _endpoint="${_argv[$_i]#--endpoint=}" ;;
                --endpoint)
                    _i=$((_i + 1))
                    _endpoint="${_argv[$_i]:-}"
                    ;;
                -*) ;;
                *) _positional+=("${_argv[$_i]}") ;;
            esac
            _i=$((_i + 1))
        done

        local _agents=()
        if [ "$_all" = "1" ]; then
            local _f
            for _f in "$config_dir_agents"/*.sh; do
                [ -f "$_f" ] || continue
                _agents+=("$(basename "$_f" .sh)")
            done
        elif [ "${#_positional[@]}" -ge 1 ]; then
            _agents=("${_positional[0]}")
        else
            cat <<USAGE
Usage:
  ${user_cmd_name:-pod} test --all                         test every configured agent
  ${user_cmd_name:-pod} test <agent>                       test one agent
  Flags:
    --no-setup     don't auto-start <agent>-shamtest pods
    --teardown     delete <agent>-shamtest pods after testing
    --endpoint URL override the sham endpoint (default: dashboard at host.containers.internal)
USAGE
            return 1
        fi

        local _server_port="${POD_SERVER_PORT:-1337}"
        if [ -z "$_endpoint" ]; then
            _endpoint="http://host.containers.internal:${_server_port}/v1"
        fi
        local _anthropic_endpoint="${_endpoint%/v1}"

        if ! command -v curl >/dev/null 2>&1; then
            echo -e "\033[31mcurl is required for pod test.\033[0m" >&2
            return 1
        fi

        if ! curl -fsS "http://127.0.0.1:${_server_port}/v1/models" >/dev/null 2>&1; then
            echo -e "\033[31mSham endpoint not reachable at http://127.0.0.1:${_server_port}/v1/models\033[0m" >&2
            echo -e "  Start the dashboard first: \033[36m${user_cmd_name:-pod} server start\033[0m" >&2
            return 1
        fi

        if [ "${#_agents[@]}" -eq 0 ]; then
            echo -e "\033[33mNo agent configs found in $config_dir_agents.\033[0m"
            return 0
        fi

        echo -e "\033[1mPod sham-endpoint test\033[0m"
        echo -e "  endpoint: \033[36m${_endpoint}\033[0m"
        echo -e "  model:    \033[36mlocal-sham-endpoint\033[0m"
        echo ""

        local _passed=0 _failed=0 _skipped=0
        local _agent _container _invoke _result _t0 _t1 _dur _start_rc

        for _agent in "${_agents[@]}"; do
            _container="${_agent}-shamtest"
            printf "  \033[36m%-14s\033[0m  " "$_agent"

            if ! podman ps --format '{{.Names}}' 2>/dev/null | grep -qFx "$_container"; then
                if [ "$_setup" = "1" ]; then
                    _pod_agents_main start "$_agent" shamtest \
                        --endpoint "$_endpoint" \
                        --api-key "sham" \
                        --model "local-sham-endpoint" >/dev/null 2>&1
                    _start_rc=$?
                    # podman pod readiness can lag; give it a moment.
                    local _w=0
                    while [ "$_w" -lt 10 ] && ! podman ps --format '{{.Names}}' 2>/dev/null | grep -qFx "$_container"; do
                        sleep 1
                        _w=$((_w + 1))
                    done
                    if ! podman ps --format '{{.Names}}' 2>/dev/null | grep -qFx "$_container"; then
                        echo -e "\033[33mSKIP\033[0m  (failed to start $_container, rc=$_start_rc)"
                        _skipped=$((_skipped + 1))
                        continue
                    fi
                else
                    echo -e "\033[33mSKIP\033[0m  ($_container not running; pass --setup to auto-start)"
                    _skipped=$((_skipped + 1))
                    continue
                fi
            fi

            _invoke=$(
                if [ -f "$config_dir_agents/${_agent}.sh" ]; then
                    unset AGENT_BATCH_INVOKE
                    # shellcheck disable=SC1090
                    source "$config_dir_agents/${_agent}.sh" >/dev/null 2>&1
                    printf '%s\n' "${AGENT_BATCH_INVOKE:-${_agent} \"\$PROMPT\"}"
                else
                    printf '%s\n' "${_agent} \"\$PROMPT\""
                fi
            )

            _t0=$(date +%s)
            _result=$(podman exec \
                -e PROMPT="ping" \
                -e OPENAI_BASE_URL="$_endpoint" \
                -e OPENAI_API_BASE="$_endpoint" \
                -e OPENAI_API_KEY="sham" \
                -e ANTHROPIC_BASE_URL="$_anthropic_endpoint" \
                -e ANTHROPIC_API_KEY="sham" \
                -e LLM="local-sham-endpoint" \
                -e DEFAULT_MODEL="local-sham-endpoint" \
                -e POD_DEFAULT_MODEL="local-sham-endpoint" \
                "$_container" bash -lc "timeout 60 bash -lc $(printf %q "$_invoke")" 2>&1)
            _t1=$(date +%s)
            _dur=$((_t1 - _t0))

            if printf '%s' "$_result" | grep -qF "This request succeeded"; then
                echo -e "\033[1;32mPASS\033[0m  (${_dur}s)"
                _passed=$((_passed + 1))
            else
                echo -e "\033[1;31mFAIL\033[0m  (${_dur}s)"
                printf '%s\n' "$_result" | head -10 | sed 's/^/      /'
                _failed=$((_failed + 1))
            fi

            if [ "$_teardown" = "1" ]; then
                _pod_agents_main delete "$_agent" shamtest >/dev/null 2>&1
            fi
        done

        echo ""
        echo -e "\033[1mSummary:\033[0m \033[32m${_passed} passed\033[0m, \033[31m${_failed} failed\033[0m, \033[33m${_skipped} skipped\033[0m"
        [ "$_failed" -gt 0 ] && return 1
        return 0
    fi

    return 99  # sentinel: fell off end, continue to next lib

    if [ "$#" -gt 0 ]; then
        case "$1" in
            -h|--help|help)
                _pod_print_help
                return 0
                ;;
            -v|--version|version)
                _pod_print_version
                return 0
                ;;
        esac
    fi

    # --- Strip supported overrides from "$@" so the positional contract in
    #     50-arg-parse stays clean. Captured values live in function-scope
    #     locals and are applied after positional parsing in 50-arg-parse.
    local MODEL_OVERRIDE=""
    local ENDPOINT_OVERRIDE=""
    local API_KEY_OVERRIDE=""
    local WORKSPACE_DIR_OVERRIDE=""
    local _mf_args=() _mf_skip=0 _mf_expect="" _mf_arg
    for _mf_arg in "$@"; do
        if [ "$_mf_skip" = "1" ]; then
            case "$_mf_expect" in
                model) MODEL_OVERRIDE="$_mf_arg" ;;
                endpoint) ENDPOINT_OVERRIDE="$_mf_arg" ;;
                api_key) API_KEY_OVERRIDE="$_mf_arg" ;;
                workspace) WORKSPACE_DIR_OVERRIDE="$_mf_arg" ;;
            esac
            _mf_skip=0
            _mf_expect=""
            continue
        fi
        case "$_mf_arg" in
            --model=*) MODEL_OVERRIDE="${_mf_arg#--model=}" ;;
            --endpoint=*) ENDPOINT_OVERRIDE="${_mf_arg#--endpoint=}" ;;
            --workspace=*) WORKSPACE_DIR_OVERRIDE="${_mf_arg#--workspace=}" ;;
            # All three spellings are accepted; --api-key is the canonical one
            # shown in help text. Underscore and run-together forms are kept
            # because users naturally type them.
            --api-key=*|--api_key=*|--apikey=*) API_KEY_OVERRIDE="${_mf_arg#*=}" ;;
            --model)   _mf_skip=1; _mf_expect="model" ;;
            --endpoint) _mf_skip=1; _mf_expect="endpoint" ;;
            --workspace) _mf_skip=1; _mf_expect="workspace" ;;
            --api-key|--api_key|--apikey) _mf_skip=1; _mf_expect="api_key" ;;
            *)         _mf_args+=("$_mf_arg") ;;
        esac
    done
    if [ "$_mf_skip" = "1" ]; then
        case "$_mf_expect" in
            model)
                echo -e "\033[31m--model requires a value (e.g. --model my-model or --model=my-model).\033[0m" >&2
                ;;
            endpoint)
                echo -e "\033[31m--endpoint requires a value (e.g. --endpoint http://127.0.0.1:8000/v1 or --endpoint=http://127.0.0.1:8000/v1).\033[0m" >&2
                ;;
            workspace)
                echo -e "\033[31m--workspace requires a value (e.g. --workspace agents-dimensions or --workspace=/workspace/agents-dimensions).\033[0m" >&2
                ;;
            api_key)
                echo -e "\033[31m--api-key requires a value (e.g. --api-key sk-... or --api-key=sk-...).\033[0m" >&2
                ;;
        esac
        return 1
    fi
    set -- "${_mf_args[@]}"

    return 99  # sentinel: fell off end, continue to next lib

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

    # --- Strip --model VAL / --model=VAL out of "$@" so the positional
    #     contract in 50-arg-parse stays clean. The captured value lives in
    #     MODEL_OVERRIDE (declared at function scope) and is applied to
    #     DEFAULT_MODEL after positional parsing in 50-arg-parse.
    local MODEL_OVERRIDE=""
    local _mf_args=() _mf_skip=0 _mf_arg
    for _mf_arg in "$@"; do
        if [ "$_mf_skip" = "1" ]; then
            MODEL_OVERRIDE="$_mf_arg"; _mf_skip=0; continue
        fi
        case "$_mf_arg" in
            --model=*) MODEL_OVERRIDE="${_mf_arg#--model=}" ;;
            --model)   _mf_skip=1 ;;
            *)         _mf_args+=("$_mf_arg") ;;
        esac
    done
    if [ "$_mf_skip" = "1" ]; then
        echo -e "\033[31m--model requires a value (e.g. --model my-model or --model=my-model).\033[0m" >&2
        return 1
    fi
    set -- "${_mf_args[@]}"

    return 99  # sentinel: fell off end, continue to next lib

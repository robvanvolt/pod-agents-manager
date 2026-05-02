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

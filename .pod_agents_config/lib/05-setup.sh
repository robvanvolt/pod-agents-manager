    # 1. Host one-time setup (directories are created by ~/.pod_agents before sourcing lib/)

    # Mask quadlet's auto-injected network-wait dependency on podman < 5.8.
    # podman 5.0–5.7 quadlet adds Wants=/After=podman-user-wait-network-online.service to every container unit;
    # if that probe can't reach its target it sits at its default 90s timeout, blocking every pod start.
    # podman >= 5.8 fixed this, so we skip the mask there.
    # Marker file makes this a one-time op per host.
    if [ ! -e "$config_dir_root/.network-wait-checked" ]; then
        local _podman_ver _podman_major _podman_minor _podman_rest
        _podman_ver=$(podman --version 2>/dev/null | awk '{print $3}')
        _podman_major=${_podman_ver%%.*}
        _podman_rest=${_podman_ver#*.}
        _podman_minor=${_podman_rest%%.*}
        if [[ "$_podman_major" =~ ^[0-9]+$ ]] && [[ "$_podman_minor" =~ ^[0-9]+$ ]]; then
            if [ "$_podman_major" -lt 5 ] || { [ "$_podman_major" -eq 5 ] && [ "$_podman_minor" -lt 8 ]; }; then
                if systemctl --user list-unit-files podman-user-wait-network-online.service &>/dev/null; then
                    echo -e "\033[36mPodman $_podman_ver detected — masking podman-user-wait-network-online.service to avoid 90s startup delay (fixed upstream in 5.8).\033[0m"
                    systemctl --user mask podman-user-wait-network-online.service &>/dev/null || true
                fi
            fi
            touch "$config_dir_root/.network-wait-checked"
        fi
    fi

    # Keep user units alive after SSH logout. Without linger, systemd --user
    # can stop all pod services when the last session exits, which looks like
    # "containers randomly die after a few minutes" even with tmux inside.
    # One-time check + best-effort auto-fix.
    if [ ! -e "$config_dir_root/.linger-checked" ] && command -v loginctl >/dev/null 2>&1; then
        local _pod_user _linger_state
        _pod_user="$(id -un 2>/dev/null || true)"
        _linger_state=""
        if [ -n "$_pod_user" ]; then
            _linger_state=$(loginctl show-user "$_pod_user" -p Linger --value 2>/dev/null || true)
        fi
        if [ "$_linger_state" = "yes" ]; then
            touch "$config_dir_root/.linger-checked"
        elif [ -n "$_pod_user" ]; then
            echo -e "\033[36mEnabling linger for user '$_pod_user' to keep pods running after logout...\033[0m"
            loginctl enable-linger "$_pod_user" >/dev/null 2>&1 || true
            _linger_state=$(loginctl show-user "$_pod_user" -p Linger --value 2>/dev/null || true)
            if [ "$_linger_state" = "yes" ]; then
                echo -e "\033[32mLinger enabled: user services remain active without an SSH session.\033[0m"
                touch "$config_dir_root/.linger-checked"
            else
                echo -e "\033[33mCould not enable linger automatically.\033[0m"
                echo -e "\033[33mRun manually on the host: sudo loginctl enable-linger $_pod_user\033[0m"
            fi
        fi
    fi

    # Auto-scaffold 'none' if the flavors directory is empty
    if [ -z "$(ls -A "$config_dir_flavors" 2>/dev/null)" ]; then
        echo "# Base node image only; no extra flavors added." > "$config_dir_flavors/none.containerfile"
    fi

    return 99  # sentinel: fell off end, continue to next lib

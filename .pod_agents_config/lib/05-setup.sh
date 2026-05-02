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

    # Auto-scaffold 'none' if the flavors directory is empty
    if [ -z "$(ls -A "$config_dir_flavors" 2>/dev/null)" ]; then
        echo "# Base node image only; no extra flavors added." > "$config_dir_flavors/none.containerfile"
    fi

    return 99  # sentinel: fell off end, continue to next lib

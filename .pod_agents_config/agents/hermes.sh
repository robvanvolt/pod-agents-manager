AGENT_VOLUME_CONFIG_PATH="/opt/data"

agent_pre_update() {
    podman pull docker.io/nousresearch/hermes-agent:latest
}

agent_build_containerfile() {
    local build_dir="$1"
    cat <<EOF > "$build_dir/Containerfile"
FROM docker.io/nousresearch/hermes-agent:latest
USER root
RUN DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y tmux curl unzip git bash && apt-get clean
RUN HERMES_BIN=\$(find /opt /app /home /usr /root -path "*/bin/hermes" -type f 2>/dev/null | head -n 1); \\
    if [ -n "\$HERMES_BIN" ]; then ln -sf "\$HERMES_BIN" /usr/bin/hermes; fi
RUN mkdir -p /root/.config/hermes /root/.hermes && \\
    ln -sf /opt/data/config.yaml /root/.config/hermes/config.yaml && \\
    ln -sf /opt/data/config.yaml /root/.hermes/config.yaml && \\
    ln -sf /opt/data/config.yaml /root/.hermes.yaml
CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"
    local action="$2"
    [ "$action" = "update" ] && return 0

    echo -e "\033[36mGenerating hermes config...\033[0m"
    local first_model=""
    IFS=',' read -ra ADDR <<< "$DEFAULT_MODEL"
    for i in "${!ADDR[@]}"; do
        local m=$(echo "${ADDR[$i]}" | xargs)
        if [ -z "$first_model" ]; then first_model="$m"; fi
    done

    local temp_cfg="/tmp/hermes_cfg_$$.yaml"
    cat <<EOF > "$temp_cfg"
model:
  provider: custom
  default: ${first_model}
  base_url: ${OPENAI_BASE_URL}
  api_key: ${OPENAI_API_KEY}

memory:
  enabled: true
  skill_generation: true
EOF
    mv -f "$temp_cfg" "$config_dir/config.yaml"
}

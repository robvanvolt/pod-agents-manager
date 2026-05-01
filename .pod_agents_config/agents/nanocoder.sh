AGENT_VOLUME_CONFIG_PATH="/root/.config/nanocoder"

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"
    write_base_node_containerfile "$build_dir" "$flavor"
    cat <<'EOF' >> "$build_dir/Containerfile"
RUN npm install -g @nanocollective/nanocoder && npm cache clean --force
CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"
    local action="$2"
    [ "$action" = "update" ] && return 0

    echo -e "\033[36mGenerating nanocoder config...\033[0m"
    local models_json=""
    local remaining_models="$DEFAULT_MODEL"
    while [ -n "$remaining_models" ]; do
        local m="${remaining_models%%,*}"
        if [ "$remaining_models" = "$m" ]; then
            remaining_models=""
        else
            remaining_models="${remaining_models#*,}"
        fi
        m=$(echo "$m" | xargs)
        [ -z "$m" ] && continue
        [ -n "$models_json" ] && models_json+=","
        models_json+="\"$m\""
    done

    local temp_cfg="/tmp/nanocoder_$$.json"
    cat <<EOF > "$temp_cfg"
{
  "nanocoder": {
    "providers": [
      {
        "name": "rms",
        "models": [${models_json}],
        "baseUrl": "${OPENAI_BASE_URL}",
        "apiKey": "${OPENAI_API_KEY}",
        "timeout": 300000
      }
    ]
  }
}
EOF

    mv -f "$temp_cfg" "$config_dir/agents.config.json"
}
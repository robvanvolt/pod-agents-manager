AGENT_VOLUME_CONFIG_PATH="/root/.config/crush"

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"
    write_base_node_containerfile "$build_dir" "$flavor"
    cat <<'EOF' >> "$build_dir/Containerfile"
RUN npm install -g @charmland/crush && npm cache clean --force
CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"
    local action="$2"
    [ "$action" = "update" ] && return 0
    
    echo -e "\033[36mGenerating crush config...\033[0m"
    local crush_models_json=""
    IFS=',' read -ra ADDR <<< "$DEFAULT_MODEL"
    for i in "${!ADDR[@]}"; do
        local m=$(echo "${ADDR[$i]}" | xargs)
        crush_models_json+="{\"id\":\"$m\",\"name\":\"$m\",\"context_window\":262144,\"default_max_tokens\":32000}"
        if [ $i -lt $((${#ADDR[@]}-1)) ]; then crush_models_json+=","; fi
    done

    local temp_cfg="/tmp/crush_$$.json"
    cat <<EOF > "$temp_cfg"
{
  "\$schema": "https://charm.land/crush.json",
  "providers": { "custom_env": { "type": "openai-compat", "base_url": "${OPENAI_BASE_URL}", "api_key": "${OPENAI_API_KEY}", "models": [${crush_models_json}] } },
  "options": { "disable_metrics": true, "disable_provider_auto_update": true, "disable_default_providers": true }
}
EOF
    mv -f "$temp_cfg" "$config_dir/crush.json"
}
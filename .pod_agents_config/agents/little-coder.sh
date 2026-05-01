AGENT_VOLUME_CONFIG_PATH="/root/.pi"

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"

    write_base_node_containerfile "$build_dir" "$flavor"
    cat <<EOF >> "$build_dir/Containerfile"
ARG LITTLE_CODER_CACHE_BUST=$(date +%s)
RUN echo "cache-bust: \$LITTLE_CODER_CACHE_BUST" && \\
    git clone https://github.com/itayinbarr/little-coder.git /opt/little-coder && \\
    cd /opt/little-coder && \\
    npm install && \\
    ln -s /opt/little-coder/node_modules/.bin/pi /usr/local/bin/little-coder && \\
    npm cache clean --force
CMD ["tail", "-f", "/dev/null"]
EOF
}

# Mirrors pi's layout
AGENT_SKILLS_SUBPATH="agent/skills"

agent_generate_config() {
    local config_dir="$1"
    local action="$2"
    [ "$action" = "update" ] && return 0

    echo -e "\033[36mGenerating little-coder config...\033[0m"
    local models_json=""
    local first_model=""
    IFS=',' read -ra ADDR <<< "$DEFAULT_MODEL"
    for i in "${!ADDR[@]}"; do
        local m=$(echo "${ADDR[$i]}" | xargs)
        [ -z "$first_model" ] && first_model="$m"
        models_json+="{\"id\":\"$m\",\"name\":\"$m\",\"contextWindow\":262144,\"maxTokens\":32000}"
        if [ $i -lt $((${#ADDR[@]}-1)) ]; then models_json+=","; fi
    done

    local temp_models="/tmp/LLM_$$.json"
    local temp_settings="/tmp/pi_settings_$$.json"
    cat <<EOF > "$temp_models"
{
  "defaultProvider": "custom_env",
  "defaultModel": "${first_model}",
  "providers": {
    "custom_env": {
      "baseUrl": "${OPENAI_BASE_URL}",
      "api": "openai-completions",
      "apiKey": "${OPENAI_API_KEY}",
      "models": [${models_json}]
    }
  }
}
EOF
    cat <<EOF > "$temp_settings"
{
  "enableInstallTelemetry": false,
  "defaultProvider": "custom_env",
  "defaultModel": "${first_model}",
  "models": [${models_json}]
}
EOF
    mkdir -p "$config_dir/agent" 2>/dev/null || true
    mv -f "$temp_models" "$config_dir/agent/models.json"
    mv -f "$temp_settings" "$config_dir/settings.json"
}

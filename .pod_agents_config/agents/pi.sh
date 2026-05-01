AGENT_VOLUME_CONFIG_PATH="/root/.pi"

# Non-interactive prompt mode for `pod batch`. $PROMPT is set per-line by the runner.
AGENT_BATCH_INVOKE='pi -p "$PROMPT"'

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"
    write_base_node_containerfile "$build_dir" "$flavor"
    cat <<'EOF' >> "$build_dir/Containerfile"
RUN npm install -g @mariozechner/pi-coding-agent && npm cache clean --force
CMD ["tail", "-f", "/dev/null"]
EOF
}

# Pi keeps user-facing config under ~/.pi/agent/, so skills live there too
AGENT_SKILLS_SUBPATH="agent/skills"

agent_generate_config() {
    local config_dir="$1"
    local action="$2"
    [ "$action" = "update" ] && return 0

    echo -e "\033[36mGenerating pi config...\033[0m"
    local models_json=""
    local first_model=""
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
        [ -z "$first_model" ] && first_model="$m"
        [ -n "$models_json" ] && models_json+=","
        models_json+="{\"id\":\"$m\"}"
    done

    local temp_models="/tmp/pi_models_$$.json"
    cat <<EOF > "$temp_models"
{
  "defaultProvider": "rms",
  "defaultModel": "${first_model}",
  "providers": {
    "rms": {
      "baseUrl": "${OPENAI_BASE_URL}",
      "api": "openai-completions",
      "apiKey": "${OPENAI_API_KEY}",
      "authHeader": true,
      "models": [${models_json}]
    }
  }
}
EOF

    local temp_settings="/tmp/pi_settings_$$.json"
    cat <<EOF > "$temp_settings"
{
  "enableInstallTelemetry": false,
  "defaultProvider": "rms",
  "defaultModel": "${first_model}"
}
EOF

    mkdir -p "$config_dir/agent" 2>/dev/null || true
    mv -f "$temp_models" "$config_dir/agent/models.json"
    mv -f "$temp_settings" "$config_dir/agent/settings.json"
}
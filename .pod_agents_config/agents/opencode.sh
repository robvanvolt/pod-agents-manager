AGENT_VOLUME_CONFIG_PATH="/root/.config/opencode"

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"
    
    # opencode binary uses glibc, force Debian (trixie-slim) instead of Alpine
    write_base_node_containerfile "$build_dir" "$flavor" "trixie-slim"
    cat <<'EOF' >> "$build_dir/Containerfile"
# Install OpenCode (package name requires the -ai suffix)
RUN npm install -g opencode-ai && npm cache clean --force

CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"
    local action="$2"
    
    # Do not overwrite on simple updates
    [ "$action" = "update" ] && return 0
    
    echo -e "\033[36mGenerating opencode config...\033[0m"
    local first_model=""
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
        if [ -z "$first_model" ]; then first_model="$m"; fi
        [ -n "$models_json" ] && models_json+=","
        models_json+="\"$m\": { \"name\": \"$m\" }"
    done
    
    rm -f "$config_dir/config.json" 2>/dev/null || true
    cat <<EOF > "$config_dir/opencode.jsonc"
{
  "\$schema": "https://opencode.ai/config.json",
  "model": "rms/${first_model}",
  "share": "disabled",
  "autoupdate": false,
  "experimental": {
    "openTelemetry": false
  },
  "provider": {
    "rms": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "RMS",
      "options": {
        "baseURL": "${OPENAI_BASE_URL}",
        "apiKey": "${OPENAI_API_KEY}"
      },
      "models": {
        ${models_json}
      }
    }
  }
}
EOF
}
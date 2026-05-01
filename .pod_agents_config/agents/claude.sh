AGENT_VOLUME_CONFIG_PATH="/root/.claude"

# Non-interactive prompt mode for `pod batch`. $PROMPT is set per-line by the runner.
AGENT_BATCH_INVOKE='claude -p "$PROMPT"'

agent_build_containerfile() {
    local build_dir="$1"
    local flavor="$2"
    
    write_base_node_containerfile "$build_dir" "$flavor"
    cat <<'EOF' >> "$build_dir/Containerfile"
# Install Claude Code via NPM
RUN npm install -g @anthropic-ai/claude-code && npm cache clean --force

# Create a bulletproof wrapper script instead of relying on bash profiles
RUN mv /usr/local/bin/claude /usr/local/bin/claude-original && \
    echo '#!/bin/bash' > /usr/local/bin/claude && \
    echo 'RAW_KEY=${ANTHROPIC_API_KEY#sk-ant-api03-}' >> /usr/local/bin/claude && \
    echo 'RAW_KEY=${RAW_KEY#sk-ant-}' >> /usr/local/bin/claude && \
    echo 'export FORMATTED_KEY="sk-ant-api03-${RAW_KEY}"' >> /usr/local/bin/claude && \
    echo 'echo "{\"hasCompletedOnboarding\": true, \"theme\": \"auto\", \"primaryApiKey\": \"$FORMATTED_KEY\"}" > /root/.claude.json' >> /usr/local/bin/claude && \
    echo 'unset ANTHROPIC_API_KEY' >> /usr/local/bin/claude && \
    echo 'exec claude-original --model "${LLM:-Qwen3.6-35B-A3B-8bit}" --dangerously-skip-permissions "$@"' >> /usr/local/bin/claude && \
    chmod +x /usr/local/bin/claude

CMD ["tail", "-f", "/dev/null"]
EOF
}

agent_generate_config() {
    local config_dir="$1"
    local action="$2"
    
    # Do not overwrite on simple updates
    [ "$action" = "update" ] && return 0
    
    echo -e "\033[36mGenerating claude settings (KV Cache & Telemetry fixes)...\033[0m"
    
    local temp_cfg="/tmp/claude_$$.json"
    cat <<EOF > "$temp_cfg"
{
  "promptSuggestionEnabled": false,
  "env": {
    "CLAUDE_CODE_ENABLE_TELEMETRY": "0",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "CLAUDE_CODE_ATTRIBUTION_HEADER": "0",
    "IS_SANDBOX": "1"
  },
  "attribution": {
    "commit": "",
    "pr": ""
  },
  "plansDirectory": "./plans",
  "prefersReducedMotion": true,
  "terminalProgressBarEnabled": false,
  "effortLevel": "high"
}
EOF
    
    mv -f "$temp_cfg" "$config_dir/settings.json"
}
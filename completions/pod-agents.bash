# Bash completion for pod-agents-manager (Homebrew install).
# The curl|bash installer registers completion inline from ~/.pod_agents; this
# standalone file is for package-manager installs that put `pod-agents` on PATH.
# Registered for both the default brew bin name and the classic `pod` alias.
complete -W "start stop restart update self-update prebuild status stats remove delete remove-all delete-all join enter it tmux config batch inbox ask instruct server token base cache-clean doctor test bench uninstall quit help version --help --version --model --endpoint --api-key --api_key --apikey --ports --workspace --from-template --no-cache --cached --all -h -v" pod-agents pod 2>/dev/null || true

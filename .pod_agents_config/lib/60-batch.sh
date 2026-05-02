    if [ "$action" = "batch" ]; then
        local batch_root="$config_dir_root/batch"
        mkdir -p "$batch_root"

        _pod_batch_progress_line() {
            local prog_file="$1"
            [ -f "$prog_file" ] || return 1
            local raw cur total pct
            raw=$(cat "$prog_file" 2>/dev/null)
            cur="${raw%%/*}"
            total="${raw##*/}"
            [[ "$cur" =~ ^[0-9]+$ ]] || cur=0
            [[ "$total" =~ ^[0-9]+$ ]] || total=0
            pct=0
            [ "$total" -gt 0 ] && pct=$(( cur * 100 / total ))
            local tgt bid pod_a pod_i
            tgt=$(basename "$prog_file" .prog)
            bid=$(basename "$(dirname "$(dirname "$prog_file")")")
            pod_a="${tgt%%-*}"
            pod_i="${tgt#*-}"
            local status="running"
            [ -f "$(dirname "$(dirname "$prog_file")")/done.${tgt}" ] && status="done"
            printf "  \033[36m%-12s %-12s\033[0m [%s/%s] %3d%%  %s  batch=%s\n" \
                "$pod_a" "$pod_i" "$cur" "$total" "$pct" "$status" "$bid"
        }

        local sub="${2:-}"
        case "$sub" in
            tmux)
                local panes=()
                for d in "$batch_root"/*/; do
                    [ -d "$d" ] || continue
                    local bid; bid=$(basename "$d")
                    for prog_file in "$d/progress/"*.prog; do
                        [ -f "$prog_file" ] || continue
                        local tgt; tgt=$(basename "$prog_file" .prog)
                        # Skip finished
                        [ -f "$d/done.${tgt}" ] && continue
                        panes+=("$bid|$tgt|$d/logs/${tgt}.log")
                    done
                done
                if [ ${#panes[@]} -eq 0 ]; then
                    echo -e "\033[33mNo active batch runners.\033[0m"
                    return 0
                fi
                if ! command -v tmux >/dev/null 2>&1; then
                    echo -e "\033[31mtmux is not installed on this host.\033[0m"
                    echo -e "  Install:  \033[36msudo apt install tmux\033[0m  (or your distro equivalent)"
                    echo ""
                    echo -e "\033[33mFollowing active batch logs instead (Ctrl+C to stop):\033[0m"
                    local _fb_files=()
                    for _fb_entry in "${panes[@]}"; do
                        local _fb_lf="${_fb_entry##*|}"
                        echo -e "  \033[36m${_fb_entry%%|*}\033[0m  →  $_fb_lf"
                        _fb_files+=("$_fb_lf")
                    done
                    echo ""
                    tail -F "${_fb_files[@]}" 2>/dev/null
                    return 0
                fi
                local session="batches"
                tmux kill-session -t "$session" 2>/dev/null || true
                local i
                for (( i=0; i<${#panes[@]}; i++ )); do
                    local entry="${panes[$i]}"
                    local bid="${entry%%|*}"
                    local rest="${entry#*|}"
                    local tgt="${rest%%|*}"
                    local logf="${rest#*|}"
                    local title="${tgt} (${bid})"
                    if [ "$i" -eq 0 ]; then
                        tmux new-session -d -s "$session" -n "batch" "bash -lc 'echo === $title ===; tail -F \"$logf\" 2>/dev/null; exec bash -il'"
                    else
                        tmux split-window -t "${session}:batch" "bash -lc 'echo === $title ===; tail -F \"$logf\" 2>/dev/null; exec bash -il'"
                        tmux select-layout -t "${session}:batch" tiled
                    fi
                done
                tmux select-layout -t "${session}:batch" tiled
                if [ -n "$TMUX" ]; then
                    tmux switch-client -t "$session"
                else
                    tmux attach-session -t "$session"
                fi
                return 0
                ;;
            log)
                local _lid="${3:-}"
                if [ -z "$_lid" ]; then
                    _lid=$(ls -1t "$batch_root" 2>/dev/null | head -n 1)
                fi
                if [ -z "$_lid" ]; then
                    echo -e "\033[33mNo batches found.\033[0m"
                    return 0
                fi
                local _ld="$batch_root/$_lid"
                [ -d "$_ld" ] || { echo -e "\033[31mBatch not found: $_lid\033[0m"; return 1; }
                echo -e "\033[1;36mBatch $_lid\033[0m"
                local _batch_log_files=()
                for _lf in "$_ld/logs/"*.log; do
                    [ -f "$_lf" ] || continue
                    [[ "$(basename "$_lf")" == *.runner.log ]] && continue
                    _batch_log_files+=("$_lf")
                done
                if [ ${#_batch_log_files[@]} -eq 0 ]; then
                    echo -e "\033[33mNo log files found.\033[0m"
                    return 0
                fi
                local _all_done=1
                for _lf in "${_batch_log_files[@]}"; do
                    local _tgt_base; _tgt_base=$(basename "$_lf" .log)
                    [ -f "$_ld/done.${_tgt_base}" ] || _all_done=0
                done
                if [ "$_all_done" = "0" ]; then
                    echo -e "\033[33mBatch still running — following log (Ctrl+C to stop):\033[0m"
                    echo ""
                    tail -F "${_batch_log_files[@]}" 2>/dev/null
                else
                    cat "${_batch_log_files[@]}"
                fi
                return 0
                ;;
            stats)
                local found=0
                for d in "$batch_root"/*/; do
                    [ -d "$d" ] || continue
                    local bid; bid=$(basename "$d")
                    [ -d "$d/progress" ] || continue
                    local first=1
                    for prog_file in "$d/progress/"*.prog; do
                        [ -f "$prog_file" ] || continue
                        if [ "$first" -eq 1 ]; then
                            echo -e "\033[1;36mBatch $bid\033[0m  ($(stat -c %y "$d/meta.conf" 2>/dev/null || stat -f %Sm "$d/meta.conf" 2>/dev/null))"
                            first=0
                        fi
                        _pod_batch_progress_line "$prog_file"
                        found=1
                    done
                done
                [ "$found" = "0" ] && echo -e "\033[33mNo batches found.\033[0m"
                return 0
                ;;
            list)
                local any=0
                for d in "$batch_root"/*/; do
                    [ -d "$d" ] || continue
                    any=1
                    basename "$d"
                done
                [ "$any" = "0" ] && echo -e "\033[33mNo batches found.\033[0m"
                return 0
                ;;
            stop)
                local bid="${3:-}"
                _pod_batch_stop_dir() {
                    local d="$1"
                    [ -d "$d" ] || return
                    # Write stop sentinel so the runner exits cleanly between prompts
                    touch "$d/.stop"
                    for pidfile in "$d"/runner-*.pid; do
                        [ -f "$pidfile" ] || continue
                        local p; p=$(cat "$pidfile" 2>/dev/null)
                        [ -z "$p" ] && continue
                        # Kill the runner and its direct children (podman exec calls)
                        pkill -P "$p" 2>/dev/null || true
                        kill "$p" 2>/dev/null || true
                    done
                    # Also kill any podman exec processes belonging to containers in this batch
                    if [ -f "$d/meta.conf" ]; then
                        local tgts; tgts=$(grep '^targets=' "$d/meta.conf" | cut -d= -f2-)
                        for t in $tgts; do
                            pkill -f "podman exec.*${t}" 2>/dev/null || true
                        done
                    fi
                }
                if [ -z "$bid" ] || [ "$bid" = "all" ]; then
                    for d in "$batch_root"/*/; do
                        _pod_batch_stop_dir "$d"
                    done
                    echo -e "\033[32mAll batch runners signaled to stop.\033[0m"
                    return 0
                fi
                local d="$batch_root/$bid"
                [ -d "$d" ] || { echo "Batch not found: $bid"; return 1; }
                _pod_batch_stop_dir "$d"
                echo -e "\033[32mStopped batch $bid.\033[0m"
                return 0
                ;;
        esac

        # ---- Run a new batch ----
        local concurrent=0
        local positional=()
        local arg
        for arg in "${@:2}"; do
            case "$arg" in
                --concurrent) concurrent=1 ;;
                --*) echo "Unknown flag: $arg"; return 1 ;;
                *) positional+=("$arg") ;;
            esac
        done

        local target_agent="" target_instance="" prompts_file=""
        case "${#positional[@]}" in
            1) prompts_file="${positional[0]}" ;;
            2) target_agent="${positional[0]}"; prompts_file="${positional[1]}" ;;
            3) target_agent="${positional[0]}"; target_instance="${positional[1]}"; prompts_file="${positional[2]}" ;;
            *)
                cat <<USAGE
Usage:
  pod batch <prompts.txt>                       # all running pods (.txt or .json)
  pod batch <agent> <prompts.txt>               # all running pods of one agent
  pod batch <agent> <instance> <prompts.txt>    # one specific pod
  pod batch log [id]   | tmux | stats | list | stop [id]
  Optional flag: --concurrent  (per-pod parallelism instead of sequential)
  Input formats:
    .txt / plain  — one prompt per line, # comments ignored
    .json         — [{"prompt":"..."}, ...] or ["...", ...]
USAGE
                return 1
                ;;
        esac

        [ -f "$prompts_file" ] || { echo -e "\033[31mPrompts file not found: $prompts_file\033[0m"; return 1; }
        prompts_file="$(cd "$(dirname "$prompts_file")" && pwd)/$(basename "$prompts_file")"

        # Resolve targets among currently running pods
        local targets=()
        local running_names
        running_names=$(podman ps --format '{{.Names}}' 2>/dev/null)
        local n
        while IFS= read -r n; do
            [ -z "$n" ] && continue
            local pod_a="${n%%-*}"
            local pod_i="${n#*-}"
            [ "$pod_a" = "$n" ] && continue   # name had no dash
            [ -d "$WORKSPACES_ROOT/${pod_a}-pods/${pod_i}" ] || continue
            [ -n "$target_agent" ] && [ "$pod_a" != "$target_agent" ] && continue
            [ -n "$target_instance" ] && [ "$pod_i" != "$target_instance" ] && continue
            targets+=("${pod_a}-${pod_i}")
        done <<< "$running_names"

        if [ ${#targets[@]} -eq 0 ]; then
            echo -e "\033[31mNo matching running pods found.\033[0m"
            return 1
        fi

        local batch_id; batch_id="$(date +%Y%m%d-%H%M%S)-$$"
        local batch_dir="$batch_root/$batch_id"
        mkdir -p "$batch_dir/progress" "$batch_dir/logs"
        rm -f "$batch_dir/.stop"

        # Normalise input: JSON array → one prompt per line plain text
        local is_json=0
        case "$prompts_file" in *.json) is_json=1 ;; esac
        if [ "$is_json" = "0" ]; then
            local _head; _head=$(head -c 1 "$prompts_file" 2>/dev/null)
            [ "$_head" = "[" ] && is_json=1
        fi
        if [ "$is_json" = "1" ]; then
            python3 - "$prompts_file" "$batch_dir/prompts.txt" <<'PYJSON'
import sys, json
src, dst = sys.argv[1], sys.argv[2]
with open(src) as f:
    data = json.load(f)
with open(dst, 'w') as out:
    for item in data:
        if isinstance(item, dict):
            line = item.get('prompt') or item.get('text') or item.get('q') or item.get('question') or ''
        else:
            line = str(item)
        line = line.strip()
        if line:
            out.write(line + '\n')
PYJSON
            if [ $? -ne 0 ]; then
                echo -e "\033[31mFailed to parse JSON prompts file.\033[0m"
                return 1
            fi
        else
            cp "$prompts_file" "$batch_dir/prompts.txt"
        fi

        local total
        total=$(grep -cve '^[[:space:]]*$' "$batch_dir/prompts.txt")
        [ "$total" -eq 0 ] && { echo -e "\033[31mPrompts file is empty.\033[0m"; return 1; }
        {
            echo "batch_id=$batch_id"
            echo "started=$(date -Iseconds 2>/dev/null || date)"
            echo "concurrent=$concurrent"
            echo "total=$total"
            echo "targets=${targets[*]}"
            echo "source=$prompts_file"
            echo "pod_manager_version=${POD_AGENTS_VERSION:-unknown}"
        } > "$batch_dir/meta.conf"

        local _prompts_stem; _prompts_stem="$(basename "${prompts_file%.*}")"
        local _launched_tgts=()

        local tgt
        for tgt in "${targets[@]}"; do
            local pod_a="${tgt%%-*}"
            local pod_i="${tgt#*-}"
            local _out_dir_for_tgt="$PWD/output/${pod_a}/${_prompts_stem}"
            local skip_count=0

            if [ -f "$_out_dir_for_tgt/.done" ]; then
                echo -e "\033[1;33mBatch '$tgt' ($_prompts_stem) is already complete.\033[0m"
                printf "  Run again from scratch? [y/N] "
                local _rerun_ans; read -r _rerun_ans
                case "$_rerun_ans" in
                    [yY]*) rm -f "$_out_dir_for_tgt/.done"; rm -rf "$_out_dir_for_tgt/sessions"; skip_count=0 ;;
                    *) echo -e "  Skipping $tgt."; continue ;;
                esac
            elif [ -d "$_out_dir_for_tgt/sessions" ]; then
                skip_count=$(find "$_out_dir_for_tgt/sessions" -name "*.jsonl" 2>/dev/null | wc -l | tr -d ' ')
                if [ "$skip_count" -gt 0 ]; then
                    echo -e "\033[1;33mResuming $tgt: $skip_count/$total already done, continuing...\033[0m"
                fi
            fi
            mkdir -p "$_out_dir_for_tgt/sessions"
            printf '%s\n' "$prompts_file" > "$_out_dir_for_tgt/.source"

            # Pull AGENT_BATCH_INVOKE from agent.sh in a subshell (so we don't pollute pod()'s scope).
            local invoke
            invoke=$(
                if [ -f "$config_dir_agents/${pod_a}.sh" ]; then
                    unset AGENT_BATCH_INVOKE
                    source "$config_dir_agents/${pod_a}.sh" >/dev/null 2>&1
                    printf '%s\n' "${AGENT_BATCH_INVOKE:-${pod_a} \"\$PROMPT\"}"
                else
                    printf '%s\n' "${pod_a} \"\$PROMPT\""
                fi
            )
            printf '%s\n' "$invoke" > "$batch_dir/invoke.${tgt}"

            local runner="$batch_dir/runner-${tgt}.sh"
            local prog_file="$batch_dir/progress/${tgt}.prog"
            local log_file="$batch_dir/logs/${tgt}.log"
            echo "0/$total" > "$prog_file"

            cat > "$runner" <<RUNNER
#!/bin/bash
batch_dir=$(printf '%q' "$batch_dir")
container=$(printf '%q' "$tgt")
total=$total
concurrent=$concurrent
pod_manager_version=$(printf '%q' "${POD_AGENTS_VERSION:-unknown}")
workspaces_root=$(printf '%q' "$WORKSPACES_ROOT")
out_dir=$(printf '%q' "$_out_dir_for_tgt")
skip_count=$skip_count
prog_file="\$batch_dir/progress/${tgt}.prog"
log_file="\$batch_dir/logs/${tgt}.log"
prompts="\$batch_dir/prompts.txt"
invoke=\$(cat "\$batch_dir/invoke.${tgt}")

write_progress() {
    local v="\$1"
    printf '%s\n' "\$v" > "\$prog_file.tmp" && mv -f "\$prog_file.tmp" "\$prog_file"
}

_je() {
    printf '%s' "\$1" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read().rstrip("\n")))' 2>/dev/null \\
        || printf '"%s"' "\$(printf '%s' "\$1" | sed 's/\\\\/\\\\\\\\/g; s/"/\\\\"/g')"
}

# Write session header to log
{
    echo "==================================================================="
    echo "  Batch:   \$( basename "\$batch_dir" )"
    echo "  Pod:     \$container"
    echo "  Started: \$(date -Iseconds 2>/dev/null || date)"
    echo "  Image:   \$(podman inspect --format '{{.Config.Image}}' "\$container" 2>/dev/null || echo 'unknown')"
    echo "  Model:   \$(podman exec "\$container" bash -lc 'echo "\${LLM:-\${DEFAULT_MODEL:-unknown}}"' 2>/dev/null | tail -n1 || echo 'unknown')"
    echo "  Manager: \$pod_manager_version"
    echo "  Prompts: \$total  (concurrent=\$concurrent)"
    echo "==================================================================="
    echo ""
} >> "\$log_file"

_pod_name="\${container%%-*}"
_pod_inst="\${container#*-}"
_sessions_src="\${workspaces_root}/\${_pod_name}-pods/\${_pod_inst}/config/agent/sessions/--workspace--"
_config_src="\${workspaces_root}/\${_pod_name}-pods/\${_pod_inst}/config/agent"
mkdir -p "\$out_dir/sessions"
touch "\$out_dir/.copy_marker"

write_progress "\$skip_count/\$total"

i=0
declare -a pids
while IFS= read -r line || [ -n "\$line" ]; do
    case "\$line" in ''|\#*) continue ;; esac
    i=\$((i+1))
    [ "\$i" -le "\$skip_count" ] && continue
    [ -f "\$batch_dir/.stop" ] && { echo -e "\\033[33m  Stopped at prompt \$i (sentinel).\\033[0m" >> "\$log_file"; break; }
    out_log="\$batch_dir/logs/${tgt}.\$i.out"
    if [ "\$concurrent" = "1" ]; then
        ( podman exec -e PROMPT="\$line" "\$container" bash -lc "\$invoke" >"\$out_log" 2>&1 ) &
        pids+=(\$!)
    else
        printf '\n=== [%d/%d] %s ===\n' "\$i" "\$total" "\$line" >> "\$log_file"
        _t0=\$(date +%s)
        _t0_iso=\$(date -Iseconds 2>/dev/null || date '+%Y-%m-%dT%H:%M:%S%z')
        podman exec -e PROMPT="\$line" "\$container" bash -lc "\$invoke" >>"\$log_file" 2>&1
        _ec=\$?
        _t1=\$(date +%s)
        _t1_iso=\$(date -Iseconds 2>/dev/null || date '+%Y-%m-%dT%H:%M:%S%z')
        _dur=\$(( \$_t1 - \$_t0 ))
        printf '{"i":%d,"total":%d,"pod":"%s","prompt":%s,"exit_code":%d,"started_at":"%s","finished_at":"%s","duration_s":%d}\n' "\$i" "\$total" "\$container" "\$(_je "\$line")" "\$_ec" "\$_t0_iso" "\$_t1_iso" "\$_dur" >> "\$batch_dir/logs/${tgt}.results.jsonl"
        find "\$_sessions_src" -maxdepth 1 -name "*.jsonl" -newer "\$out_dir/.copy_marker" \\
            -exec cp {} "\$out_dir/sessions/" \; 2>/dev/null || true
        touch "\$out_dir/.copy_marker"
        write_progress "\$i/\$total"
    fi
done < "\$prompts"

if [ "\$concurrent" = "1" ]; then
    completed=0
    for p in "\${pids[@]}"; do
        wait "\$p" 2>/dev/null || true
        completed=\$((completed+1))
        write_progress "\$completed/\$total"
    done
fi

date -Iseconds > "\$batch_dir/done.${tgt}" 2>/dev/null || date > "\$batch_dir/done.${tgt}"

# Write session footer to log
{
    echo ""
    echo "==================================================================="
    echo "  Batch complete: \$( basename "\$batch_dir" )"
    echo "  Finished: \$(date -Iseconds 2>/dev/null || date)"
    echo "==================================================================="
} >> "\$log_file"

# --- Finalise batch outputs ---
[ -f "\$_config_src/settings.json" ] && cp "\$_config_src/settings.json" "\$out_dir/settings.json"
[ -d "\$_config_src/skills" ] && cp -r "\$_config_src/skills" "\$out_dir/" 2>/dev/null || true
touch "\$out_dir/.done"

_n_sessions=\$(find "\$out_dir/sessions" -name "*.jsonl" 2>/dev/null | wc -l | tr -d ' ')

cat > "\$batch_dir/_stats_writer_${tgt}.py" <<'PYEOF'
import json, os

container    = os.environ.get('_CT', '')
batch_dir    = os.environ.get('_BD', '')
results_file = os.environ.get('_RF', '')
total        = int(os.environ.get('_TOT', '0'))
n_sessions   = int(os.environ.get('_NS', '0') or '0')

results = []
if os.path.exists(results_file):
    with open(results_file) as f:
        for ln in f:
            ln = ln.strip()
            if ln:
                try: results.append(json.loads(ln))
                except: pass

n         = len(results)
total_dur = sum(r.get('duration_s', 0) for r in results)
avg       = round(total_dur / n, 2) if n else 0

started = source_file = finished = ''
meta_path = os.path.join(batch_dir, 'meta.conf')
if os.path.exists(meta_path):
    with open(meta_path) as f:
        for line in f:
            if line.startswith('started='):
                started = line.split('=', 1)[1].strip()
            elif line.startswith('source='):
                source_file = line.split('=', 1)[1].strip()

done_path = os.path.join(batch_dir, f'done.{container}')
if os.path.exists(done_path):
    with open(done_path) as f:
        finished = f.read().strip()

print(json.dumps({
    'batch_id':          os.path.basename(batch_dir),
    'pod':               container,
    'input_file':        source_file,
    'started_at':        started,
    'finished_at':       finished,
    'total_prompts':     total,
    'processed':         n,
    'sessions_collected': n_sessions,
    'total_duration_s':  total_dur,
    'avg_duration_s':    avg,
}, indent=2))
PYEOF
_BD="\$batch_dir" _CT="\$container" \\
    _RF="\$batch_dir/logs/${tgt}.results.jsonl" \\
    _TOT="\$total" _NS="\$_n_sessions" \\
    python3 "\$batch_dir/_stats_writer_${tgt}.py" > "\$out_dir/stats.json" 2>/dev/null || true
echo -e "\033[1;32m  Output saved to: \$out_dir\033[0m"
RUNNER
            chmod +x "$runner"
            nohup "$runner" >"$batch_dir/logs/${tgt}.runner.log" 2>&1 < /dev/null &
            local rpid=$!
            echo "$rpid" > "$batch_dir/runner-${tgt}.pid"
            disown 2>/dev/null || true
            _launched_tgts+=("$tgt")
        done

        if [ ${#_launched_tgts[@]} -eq 0 ]; then
            echo -e "\033[33mNothing to run.\033[0m"
            return 0
        fi

        echo -e "\033[1;32mBatch $batch_id started across ${#_launched_tgts[@]} pod(s):\033[0m"
        for tgt in "${_launched_tgts[@]}"; do
            local pod_a="${tgt%%-*}"
            local pod_i="${tgt#*-}"
            printf "  \033[36m%-12s %-12s\033[0m  log: %s\n" "$pod_a" "$pod_i" "$batch_dir/logs/${tgt}.log"
        done
        echo
        echo -e "  Watch:    \033[36mpod batch tmux\033[0m"
        echo -e "  Progress: \033[36mpod batch stats\033[0m"
        echo -e "  Stop:     \033[36mpod batch stop $batch_id\033[0m"
        return 0
    fi

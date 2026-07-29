# shellcheck shell=bash disable=SC2154,SC2168,SC2034
# Sourced as a fragment inside the pod() function in ~/.pod_agents; the
# variables and `local`s it uses come from that enclosing scope.
    # pod bench — reproducible agent comparison on pluggable tasks.
    #
    # Tasks live under ~/.pod_agents_config/benchmarks/<name>/ (same drop-in
    # convention as agents/flavors):
    #   task.conf    KEY="value": BENCH_DESCRIPTION, BENCH_ARTIFACT, BENCH_TIMEOUT
    #   prompt.md    the prompt sent to each agent verbatim ($PROMPT)
    #   criteria.md  the rubric handed to the judge LLM
    #
    # Runs are stored under benchmarks/<task>/runs/<run-id>/<agent>-<inst>/
    # (meta.json, judge.json, output.log, plus a copy of the artifact) so
    # results are diffable and easy to share.
    #
    # Judge LLM: POD_BENCH_JUDGE_MODEL / POD_BENCH_JUDGE_BASE_URL /
    # POD_BENCH_JUDGE_API_KEY — each falls back to the main .env values, so a
    # zero-config setup judges with the same local model that did the work.
    #
    # Agents run SEQUENTIALLY by design: on a single local inference server,
    # parallel runs would contend for the GPU and skew every wall-clock
    # number. Sequential is the only fair comparison on shared hardware.

    if [ "$action" = "bench" ]; then
        local bench_root="$config_dir_root/benchmarks"
        mkdir -p "$bench_root"
        local sub="${2:-}"

        _pod_bench_usage() {
            local _cmd="${user_cmd_name:-pod}"
            cat <<USAGE
Usage:
  ${_cmd} bench list                              available tasks
  ${_cmd} bench run <task> [agent [instance]]     run on running pods (sequential)
  ${_cmd} bench results <task> [run-id]           comparison table
  ${_cmd} bench export <task> [run-id] [--csv]    machine-readable results (JSON default)

Env:
  POD_BENCH_JUDGE_MODEL/_BASE_URL/_API_KEY   judge LLM (default: main .env values)
  POD_BENCH_TIMEOUT                          per-pod seconds (default: task.conf)
USAGE
        }

        # Extract the judge's {"score": N, "rationale": "..."} from raw LLM
        # output on stdin. Thinking models prepend reasoning and some servers
        # inline <think> blocks into content, so: flatten newlines, drop think
        # blocks, then take the LAST brace-object containing "score" (the
        # rubric forbids braces inside the rationale). Prints JSON on success.
        _pod_bench_parse_judge() {
            local flat cleaned obj
            flat=$(tr '\n' ' ')
            cleaned=$(printf '%s' "$flat" | sed 's/<think>.*<\/think>//g')
            obj=$(printf '%s' "$cleaned" | grep -o '{[^{}]*"score"[^{}]*}' | tail -1)
            [ -n "$obj" ] || return 1
            printf '%s' "$obj" | jq -e '{score: (.score | tonumber), rationale: (.rationale // "")}' 2>/dev/null
        }

        # Judge one artifact against the rubric. Writes judge.json (score,
        # rationale, judge_model) next to the raw response; returns nonzero
        # if the endpoint failed or the reply was unparseable.
        _pod_bench_judge() {
            local artifact="$1" criteria_file="$2" out_dir="$3"
            local jm="${POD_BENCH_JUDGE_MODEL:-$DEFAULT_MODEL}"
            local jb="${POD_BENCH_JUDGE_BASE_URL:-$OPENAI_BASE_URL}"
            local jk="${POD_BENCH_JUDGE_API_KEY:-$OPENAI_API_KEY}"
            command -v jq >/dev/null 2>&1 || { echo -e "\033[31mbench judge requires jq on the host.\033[0m" >&2; return 1; }

            local sys usr
            sys="You are a strict, impartial code judge. Score the submission against the rubric. Reply with EXACTLY one line of JSON: {\"score\": <number 0-10>, \"rationale\": \"<one sentence, no curly braces>\"} and nothing else."
            usr="RUBRIC:
$(cat "$criteria_file")

SUBMISSION (index file contents, possibly truncated):
$(head -c 24000 "$artifact")"

            jq -n --arg m "$jm" --arg sys "$sys" --arg usr "$usr" \
                '{model:$m, temperature:0, max_tokens:700,
                  messages:[{role:"system",content:$sys},{role:"user",content:$usr}]}' \
                > "$out_dir/judge_request.json" || return 1

            if ! curl -fsS --max-time 300 \
                    -H "Authorization: Bearer $jk" -H 'Content-Type: application/json' \
                    -d @"$out_dir/judge_request.json" \
                    "$jb/chat/completions" > "$out_dir/judge_response.json" 2>"$out_dir/judge_error.log"; then
                echo -e "\033[33m  judge endpoint failed (see judge_error.log)\033[0m" >&2
                return 1
            fi
            rm -f "$out_dir/judge_error.log"

            local raw parsed
            raw=$(jq -r '.choices[0].message.content // empty' "$out_dir/judge_response.json")
            parsed=$(printf '%s' "$raw" | _pod_bench_parse_judge) || {
                echo -e "\033[33m  judge reply unparseable (raw kept in judge_response.json)\033[0m" >&2
                return 1
            }
            printf '%s' "$parsed" | jq --arg jm "$jm" '. + {judge_model:$jm}' > "$out_dir/judge.json"
        }

        # Best-effort token usage from the agent's own trace (see TRACES.md).
        # Only codex exposes cached-vs-new input tokens in an easily parseable
        # host-visible file today (date-partitioned rollout JSONL under the
        # bind-mounted config dir). Others: silently no-op — meta.json simply
        # lacks token fields. Prints a JSON fragment or nothing.
        _pod_bench_tokens() {
            local agent_name="$1" inst="$2" since_epoch="$3"
            command -v jq >/dev/null 2>&1 || return 0
            case "$agent_name" in
                codex)
                    local sess_dir="$WORKSPACES_ROOT/${agent_name}-pods/${inst}/config/sessions"
                    [ -d "$sess_dir" ] || return 0
                    local f
                    f=$(find "$sess_dir" -type f -name '*.jsonl' -newermt "@$since_epoch" 2>/dev/null | sort | tail -1)
                    [ -n "$f" ] || return 0
                    jq -s '[ .[] | select(.type=="event_msg" and .payload.type=="token_count") ] | last
                           | .payload.info.total_token_usage
                           | {tokens_input: .input_tokens, tokens_cached: .cached_input_tokens,
                              tokens_output: .output_tokens}' "$f" 2>/dev/null | grep -v null || true
                    ;;
            esac
        }

        case "$sub" in
            list)
                local found=0 d name desc
                for d in "$bench_root"/*/; do
                    [ -f "$d/task.conf" ] || continue
                    found=1
                    name=$(basename "$d")
                    desc=$(grep -E '^BENCH_DESCRIPTION=' "$d/task.conf" | head -1 | cut -d= -f2- | tr -d '"')
                    local nruns=0
                    [ -d "$d/runs" ] && nruns=$(find "$d/runs" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l | tr -d ' ')
                    printf '  \033[36m%-28s\033[0m %s  \033[2m(%s run(s))\033[0m\n' "$name" "$desc" "$nruns"
                done
                [ "$found" = "0" ] && echo -e "\033[33mNo benchmark tasks in $bench_root.\033[0m"
                return 0
                ;;

            run)
                local task="${3:-}"
                [ -n "$task" ] || { _pod_bench_usage; return 1; }
                local task_dir="$bench_root/$task"
                if [ ! -f "$task_dir/task.conf" ] || [ ! -f "$task_dir/prompt.md" ] || [ ! -f "$task_dir/criteria.md" ]; then
                    echo -e "\033[31mUnknown or incomplete bench task: $task (need task.conf, prompt.md, criteria.md in $task_dir)\033[0m" >&2
                    return 1
                fi
                # shellcheck disable=SC1091
                source "$task_dir/task.conf"
                local artifact="${BENCH_ARTIFACT:-index.html}"
                local budget="${POD_BENCH_TIMEOUT:-${BENCH_TIMEOUT:-900}}"
                local prompt_text; prompt_text=$(cat "$task_dir/prompt.md")

                local agent_filter="${4:-}" inst_filter="${5:-}"

                # Targets: running pods whose agent defines AGENT_BATCH_INVOKE.
                local targets=() a name inst
                for a in "${available_agents[@]}"; do
                    [ -n "$agent_filter" ] && [ "$a" != "$agent_filter" ] && continue
                    while IFS= read -r name; do
                        [ -n "$name" ] || continue
                        inst="${name#"${a}"-}"
                        [ -n "$inst_filter" ] && [ "$inst" != "$inst_filter" ] && continue
                        targets+=("$a $inst")
                    done < <(podman ps --format '{{.Names}}' 2>/dev/null | grep "^${a}-" || true)
                done
                if [ ${#targets[@]} -eq 0 ]; then
                    echo -e "\033[33mNo running pods matched. Start one first, e.g.: ${user_cmd_name:-pod} start codex bench\033[0m"
                    return 1
                fi

                local run_id; run_id=$(date +%Y%m%d-%H%M%S)
                local run_root="$task_dir/runs/$run_id"
                mkdir -p "$run_root"
                echo -e "\033[1;36m==> bench '$task' — run $run_id, ${#targets[@]} pod(s), budget ${budget}s each (sequential for fair wall-clock)\033[0m"

                local entry cn workdir host_ws host_art out_dir started ended wall rc size tokjson
                for entry in "${targets[@]}"; do
                    a="${entry%% *}"; inst="${entry#* }"
                    cn="${a}-${inst}"
                    # Load the agent's invoke contract.
                    unset AGENT_BATCH_INVOKE
                    # shellcheck disable=SC1090
                    source "$config_dir_agents/${a}.sh" 2>/dev/null || true
                    if [ -z "${AGENT_BATCH_INVOKE:-}" ]; then
                        echo -e "  \033[33mskip $cn — agent '$a' has no headless mode (no AGENT_BATCH_INVOKE)\033[0m"
                        continue
                    fi

                    workdir="/workspace/bench-$run_id"
                    host_ws="$WORKSPACES_ROOT/${a}-pods/${inst}/workspace/bench-$run_id"
                    host_art="$host_ws/$artifact"
                    out_dir="$run_root/$cn"
                    mkdir -p "$out_dir"

                    echo -e "  \033[36m$cn\033[0m running..."
                    podman exec "$cn" mkdir -p "$workdir" 2>/dev/null || {
                        echo -e "  \033[31m$cn — cannot create workdir; skipping\033[0m"; continue; }

                    started=$(date +%s)
                    if command -v timeout >/dev/null 2>&1; then
                        timeout "$budget" podman exec -w "$workdir" -e PROMPT="$prompt_text" "$cn" sh -lc "$AGENT_BATCH_INVOKE" >"$out_dir/output.log" 2>&1
                    else
                        podman exec -w "$workdir" -e PROMPT="$prompt_text" "$cn" sh -lc "$AGENT_BATCH_INVOKE" >"$out_dir/output.log" 2>&1
                    fi
                    rc=$?
                    ended=$(date +%s); wall=$((ended - started))

                    size=0
                    if [ -f "$host_art" ]; then
                        cp "$host_art" "$out_dir/$artifact"
                        size=$(wc -c < "$out_dir/$artifact" | tr -d ' ')
                    fi

                    tokjson=$(_pod_bench_tokens "$a" "$inst" "$started")
                    jq -n --arg task "$task" --arg run "$run_id" --arg agent "$a" --arg inst "$inst" \
                          --arg art "$artifact" \
                          --argjson wall "$wall" --argjson rc "$rc" --argjson size "$size" \
                          --argjson tok "${tokjson:-null}" \
                        '{task:$task, run_id:$run, agent:$agent, instance:$inst,
                          wall_seconds:$wall, exit_code:$rc,
                          artifact:$art, artifact_bytes:$size}
                         + (if $tok == null then {} else $tok end)' > "$out_dir/meta.json"

                    if [ "$size" -gt 0 ]; then
                        echo -e "    ${wall}s, artifact ${size} bytes — judging..."
                        if _pod_bench_judge "$out_dir/$artifact" "$task_dir/criteria.md" "$out_dir"; then
                            echo -e "    \033[32mscore $(jq -r .score "$out_dir/judge.json")/10\033[0m — $(jq -r .rationale "$out_dir/judge.json")"
                        fi
                    else
                        echo -e "    \033[31m${wall}s, NO artifact (exit $rc) — score 0\033[0m"
                        printf '{"score": 0, "rationale": "no artifact produced", "judge_model": "n/a"}\n' > "$out_dir/judge.json"
                    fi
                done

                echo
                _pod_agents_main bench results "$task" "$run_id"
                return 0
                ;;

            results)
                local task="${3:-}" only_run="${4:-}"
                [ -n "$task" ] || { _pod_bench_usage; return 1; }
                local runs_dir="$bench_root/$task/runs"
                if [ ! -d "$runs_dir" ] || [ -z "$(ls -A "$runs_dir" 2>/dev/null)" ]; then
                    echo -e "\033[33mNo runs yet for '$task'. Start one: ${user_cmd_name:-pod} bench run $task\033[0m"
                    return 0
                fi
                command -v jq >/dev/null 2>&1 || { echo -e "\033[31mbench results requires jq.\033[0m" >&2; return 1; }
                printf '\033[1m%-17s %-22s %7s %9s %6s\033[0m  %s\n' "RUN" "POD" "WALL(s)" "BYTES" "SCORE" "RATIONALE"
                local rd pd score rat
                for rd in "$runs_dir"/*/; do
                    [ -d "$rd" ] || continue
                    [ -n "$only_run" ] && [ "$(basename "$rd")" != "$only_run" ] && continue
                    for pd in "$rd"*/; do
                        [ -f "$pd/meta.json" ] || continue
                        score="-"; rat=""
                        if [ -f "$pd/judge.json" ]; then
                            score=$(jq -r '.score' "$pd/judge.json")
                            rat=$(jq -r '.rationale' "$pd/judge.json" | head -c 60)
                        fi
                        printf '%-17s %-22s %7s %9s %6s  %s\n' \
                            "$(basename "$rd")" "$(basename "$pd")" \
                            "$(jq -r '.wall_seconds' "$pd/meta.json")" \
                            "$(jq -r '.artifact_bytes' "$pd/meta.json")" \
                            "$score" "$rat"
                    done
                done
                return 0
                ;;

            export)
                local task="${3:-}" only_run="" fmt="json" argx
                [ -n "$task" ] || { _pod_bench_usage; return 1; }
                for argx in "${@:4}"; do
                    case "$argx" in
                        --csv) fmt="csv" ;;
                        --json) fmt="json" ;;
                        *) only_run="$argx" ;;
                    esac
                done
                local runs_dir="$bench_root/$task/runs"
                [ -d "$runs_dir" ] || { echo -e "\033[33mNo runs yet for '$task'.\033[0m" >&2; return 1; }
                command -v jq >/dev/null 2>&1 || { echo -e "\033[31mbench export requires jq.\033[0m" >&2; return 1; }
                local rows=() rd pd merged
                for rd in "$runs_dir"/*/; do
                    [ -d "$rd" ] || continue
                    [ -n "$only_run" ] && [ "$(basename "$rd")" != "$only_run" ] && continue
                    for pd in "$rd"*/; do
                        [ -f "$pd/meta.json" ] || continue
                        if [ -f "$pd/judge.json" ]; then
                            merged=$(jq -s '.[0] + {score: .[1].score, rationale: .[1].rationale, judge_model: .[1].judge_model}' "$pd/meta.json" "$pd/judge.json")
                        else
                            merged=$(cat "$pd/meta.json")
                        fi
                        rows+=("$merged")
                    done
                done
                if [ "$fmt" = "csv" ]; then
                    printf '%s\n' "${rows[@]}" | jq -r -s '
                        (["run_id","agent","instance","wall_seconds","artifact_bytes","score","tokens_input","tokens_cached","tokens_output","rationale"]),
                        (.[] | [.run_id,.agent,.instance,.wall_seconds,.artifact_bytes,(.score//""),(.tokens_input//""),(.tokens_cached//""),(.tokens_output//""),(.rationale//"")])
                        | @csv'
                else
                    printf '%s\n' "${rows[@]}" | jq -s '.'
                fi
                return 0
                ;;

            *)
                _pod_bench_usage
                [ -z "$sub" ] && return 1 || return 1
                ;;
        esac
    fi

    return 99  # sentinel: fell off end, continue to next lib

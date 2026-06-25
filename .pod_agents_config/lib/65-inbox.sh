# shellcheck shell=bash disable=SC2154,SC2168,SC2034
# Sourced as a fragment inside the pod() function in ~/.pod_agents; the
# variables and `local`s it uses come from that enclosing scope.
    # Human-in-the-loop inbox commands. Entries are append-only JSONL files so
    # the CLI, dashboard, and future notification workers can all share the
    # same lightweight queue without needing a daemon or database.

    _pod_inbox_root() {
        printf '%s\n' "$config_dir_root/inbox"
    }

    _pod_inbox_target_path() {
        local _agent="$1"
        local _instance="$2"
        printf '%s/%s-%s.jsonl\n' "$(_pod_inbox_root)" "$_agent" "$_instance"
    }

    _pod_inbox_json_escape() {
        if command -v python3 >/dev/null 2>&1; then
            python3 -c 'import json,sys; sys.stdout.write(json.dumps(sys.stdin.read().rstrip("\n")))' 2>/dev/null
        else
            sed 's/\\/\\\\/g; s/"/\\"/g; s/^/"/; s/$/"/'
        fi
    }

    _pod_inbox_json_array() {
        local _first=1 _item
        printf '['
        for _item in "$@"; do
            [ "$_first" -eq 1 ] || printf ','
            printf '%s' "$_item" | _pod_inbox_json_escape
            _first=0
        done
        printf ']'
    }

    _pod_inbox_validate_target() {
        local _agent="$1"
        local _instance="$2"
        if [ -z "$_agent" ] || [ -z "$_instance" ]; then
            echo "Usage: ${user_cmd_name:-pod} $action <agent> <instance> ..." >&2
            return 1
        fi
        if [ ! -f "$config_dir_agents/${_agent}.sh" ]; then
            echo -e "\033[31mUnknown agent: $_agent\033[0m" >&2
            return 1
        fi
        case "$_agent" in *[!a-zA-Z0-9_.-]*|"") echo -e "\033[31mInvalid agent: $_agent\033[0m" >&2; return 1 ;; esac
        case "$_instance" in *[!a-zA-Z0-9_.-]*|"") echo -e "\033[31mInvalid instance: $_instance\033[0m" >&2; return 1 ;; esac
        return 0
    }

    _pod_inbox_append() {
        local _type="$1"
        local _agent="$2"
        local _instance="$3"
        local _body="$4"
        local _source="${5:-cli}"
        shift 5 || true
        local _options=("$@")

        _pod_inbox_validate_target "$_agent" "$_instance" || return 1
        [ -n "$_body" ] || { echo -e "\033[31mMessage body is required.\033[0m" >&2; return 1; }

        local _root _path _id _created _escaped_body _escaped_source _options_json
        _root="$(_pod_inbox_root)"
        _path="$(_pod_inbox_target_path "$_agent" "$_instance")"
        mkdir -p "$_root"
        _id="$(date +%Y%m%d-%H%M%S)-$$-${RANDOM:-0}"
        _created="$(date -Iseconds 2>/dev/null || date '+%Y-%m-%dT%H:%M:%S%z')"
        _escaped_body="$(printf '%s' "$_body" | _pod_inbox_json_escape)"
        _escaped_source="$(printf '%s' "$_source" | _pod_inbox_json_escape)"
        _options_json="$(_pod_inbox_json_array "${_options[@]}")"

        printf '{"id":"%s","created_at":"%s","type":"%s","agent":"%s","instance":"%s","source":%s,"status":"pending","body":%s,"options":%s}\n' \
            "$_id" "$_created" "$_type" "$_agent" "$_instance" "$_escaped_source" "$_escaped_body" "$_options_json" >> "$_path"
        chmod 0600 "$_path" 2>/dev/null || true
        echo -e "\033[32mQueued $_type for ${_agent}-${_instance}: $_id\033[0m"
    }

    _pod_inbox_print_file() {
        local _path="$1"
        local _json="${2:-0}"
        [ -f "$_path" ] || return 0
        if [ "$_json" = "1" ]; then
            cat "$_path"
            return 0
        fi
        echo -e "\033[1;36m$(basename "$_path" .jsonl)\033[0m"
        if command -v python3 >/dev/null 2>&1; then
            python3 - "$_path" <<'PYINBOX'
import json, sys
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    try:
        item = json.loads(line)
    except Exception:
        print("  " + line)
        continue
    body = item.get("body", "")
    if len(body) > 120:
        body = body[:117] + "..."
    opts = item.get("options") or []
    opt_text = (" options=" + ", ".join(map(str, opts))) if opts else ""
    print(f"  [{item.get('status','pending')}] {item.get('type','entry')} {item.get('id','')} {item.get('created_at','')}")
    print(f"      {body}{opt_text}")
PYINBOX
        else
            sed 's/^/  /' "$_path"
        fi
    }

    if [ "$action" = "instruct" ]; then
        local _msg
        _msg="${*:4}"
        if [ -z "$_msg" ]; then
            echo "Usage: ${user_cmd_name:-pod} instruct <agent> <instance> <instruction...>"
            return 1
        fi
        _pod_inbox_append "instruction" "$agent" "$instance" "$_msg" "cli"
        return $?
    fi

    if [ "$action" = "ask" ]; then
        local _question="${4:-}"
        if [ -z "$_question" ]; then
            echo "Usage: ${user_cmd_name:-pod} ask <agent> <instance> \"Question?\" --option A --option B"
            return 1
        fi
        local _ask_args=("${@:5}")
        local _options=()
        local _i=0
        while [ "$_i" -lt "${#_ask_args[@]}" ]; do
            case "${_ask_args[$_i]}" in
                --option|-o)
                    _i=$((_i + 1))
                    [ "$_i" -lt "${#_ask_args[@]}" ] || { echo -e "\033[31m--option requires a value.\033[0m" >&2; return 1; }
                    _options+=("${_ask_args[$_i]}")
                    ;;
                *)
                    _options+=("${_ask_args[$_i]}")
                    ;;
            esac
            _i=$((_i + 1))
        done
        [ "${#_options[@]}" -gt 0 ] || { echo -e "\033[31mAt least one option is required.\033[0m" >&2; return 1; }
        _pod_inbox_append "question" "$agent" "$instance" "$_question" "cli" "${_options[@]}"
        return $?
    fi

    if [ "$action" = "inbox" ]; then
        local _json=0 _clear=0 _all=0 _arg
        for _arg in "${@:2}"; do
            case "$_arg" in
                --json) _json=1 ;;
                --clear) _clear=1 ;;
                --all) _all=1 ;;
            esac
        done

        local _root _path _archive _ts _found=0
        _root="$(_pod_inbox_root)"
        mkdir -p "$_root"

        if [ -n "$agent" ] && [ "$agent" != "--all" ] && [ "$agent" != "--json" ] && [ "$agent" != "--clear" ]; then
            _pod_inbox_validate_target "$agent" "$instance" || return 1
            _path="$(_pod_inbox_target_path "$agent" "$instance")"
            if [ "$_clear" = "1" ]; then
                [ -f "$_path" ] || { echo -e "\033[33mNo inbox entries for ${agent}-${instance}.\033[0m"; return 0; }
                _archive="$_root/archive"
                _ts="$(date +%Y%m%d-%H%M%S)"
                mkdir -p "$_archive"
                mv "$_path" "$_archive/${agent}-${instance}.${_ts}.jsonl"
                echo -e "\033[32mArchived inbox to $_archive/${agent}-${instance}.${_ts}.jsonl\033[0m"
                return 0
            fi
            [ -f "$_path" ] || { echo -e "\033[33mNo inbox entries for ${agent}-${instance}.\033[0m"; return 0; }
            _pod_inbox_print_file "$_path" "$_json"
            return 0
        fi

        for _path in "$_root"/*.jsonl; do
            [ -f "$_path" ] || continue
            _found=1
            _pod_inbox_print_file "$_path" "$_json"
        done
        if [ "$_found" = "0" ]; then
            [ "$_all" = "1" ] && echo -e "\033[33mNo inbox files found.\033[0m" || echo -e "\033[33mNo inbox entries found.\033[0m"
        fi
        return 0
    fi

    return 99  # sentinel: fell off end, continue to next lib

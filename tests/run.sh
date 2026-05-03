#!/usr/bin/env bash
# Lint + sham tests for pod-agents-manager.
# No podman, no systemctl, no network — these just verify that the bash
# entrypoint and the modular lib/ files parse, load, and behave on a
# sandboxed $HOME. Designed to run on macOS (bash 3.2+).
#
# Usage:  bash tests/run.sh
# Exit:   0 on success, 1 on any failure.

set -u

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"

passes=0
fails=0
failed_names=()

note() { printf '\033[36m[..]\033[0m %s\n' "$*"; }
pass() { printf '\033[32m[OK]\033[0m %s\n' "$*"; passes=$((passes + 1)); }
fail() { printf '\033[31m[FAIL]\033[0m %s\n' "$*"; fails=$((fails + 1)); failed_names+=("$*"); }

run_test() {
    local name="$1"; shift
    note "$name"
    if "$@"; then
        pass "$name"
    else
        fail "$name"
    fi
}

# ----- 1. Syntax checks -----------------------------------------------------

t_syntax_entrypoint() { bash -n .pod_agents; }

t_syntax_libs() {
    local f rc=0
    for f in .pod_agents_config/lib/*.sh; do
        if ! bash -n "$f"; then
            echo "  syntax error in $f" >&2
            rc=1
        fi
    done
    return "$rc"
}

t_syntax_install() { bash -n install.sh; }

# ----- 2. Shellcheck --------------------------------------------------------
# SC2168 ('local' is only valid in functions) is a known false-positive for
# the lib files: they are sourced INSIDE the pod() function, which shellcheck
# cannot infer from a standalone file. We filter only that code from libs.

t_shellcheck_available() { command -v shellcheck >/dev/null 2>&1; }

t_shellcheck_entrypoint() {
    # SC2034: locals consumed by sourced libs (false-positive when sourcing).
    shellcheck -s bash --severity=error --exclude=SC2034 .pod_agents
}

t_shellcheck_libs() {
    # Suppress SC2168 because lib files are sourced inside pod().
    # Suppress SC1090/SC1091 — dynamic source paths.
    local f rc=0
    for f in .pod_agents_config/lib/*.sh; do
        if ! shellcheck -s bash --severity=error \
                --exclude=SC2168,SC1090,SC1091 "$f"; then
            rc=1
        fi
    done
    return "$rc"
}

t_shellcheck_install() {
    shellcheck -s bash --severity=error install.sh
}

# ----- 3. Lib-loader contract ----------------------------------------------
# All lib files must be sourced in numeric order. Verify each has the
# NN-name.sh prefix the entrypoint glob relies on.

t_libs_numeric_prefix() {
    local f base rc=0
    for f in .pod_agents_config/lib/*.sh; do
        base=$(basename "$f")
        if ! [[ "$base" =~ ^[0-9]+- ]]; then
            echo "  lib file lacks NN- prefix: $base" >&2
            rc=1
        fi
    done
    return "$rc"
}

t_libs_sort_matches_intent() {
    # The entrypoint relies on shell glob ordering (lexical) to load files.
    # With zero-padded numeric prefixes lexical order == numeric order.
    local listed expected
    listed=$(cd .pod_agents_config/lib && ls -1 ./*.sh | sort)
    expected=$(cd .pod_agents_config/lib && ls -1 ./*.sh)
    [ "$listed" = "$expected" ]
}

# ----- 4. Static contract checks -------------------------------------------
# These are cheap regression guards for past bugs.

t_install_copies_lib() {
    grep -qE 'merge_tree[[:space:]]+"\$src_root/\.pod_agents_config/lib"' install.sh
}

t_self_update_copies_lib() {
    grep -qE '_pod_merge_tree[[:space:]]+"\$src_root/\.pod_agents_config/lib"' \
        .pod_agents_config/lib/25-cli-help-sync.sh
}

t_self_update_seeds_env_from_example() {
    # The tarball ships .env.example (not .env, which is gitignored), so the
    # sync code must seed user .env from the example.
    ! grep -qE 'cp[[:space:]]+"\$src_root/\.pod_agents_config/\.env"[[:space:]]' \
        .pod_agents_config/lib/25-cli-help-sync.sh
}

t_entrypoint_sources_lib_glob() {
    grep -qE 'for[[:space:]]+_pod_lib[[:space:]]+in[[:space:]]+"\$config_dir_root"/lib/\*\.sh' .pod_agents
}

# ----- 5. Sandboxed runtime smoke tests ------------------------------------
# Mirror the repo into a fake $HOME, source the entrypoint, and exercise the
# pod function for the no-side-effect actions (--help, --version).

setup_sandbox() {
    local tmp
    tmp=$(mktemp -d -t pod-agents-test.XXXXXX)
    mkdir -p "$tmp/.pod_agents_config"
    cp .pod_agents "$tmp/.pod_agents"
    # Mirror everything except .git artifacts
    cp -R .pod_agents_config/. "$tmp/.pod_agents_config/"
    # Pre-seed .env from .env.example so the entrypoint doesn't try to
    # auto-configure (which would also no-op without a tty, but be explicit).
    cp .pod_agents_config/.env.example "$tmp/.pod_agents_config/.env"
    printf '%s' "$tmp"
}

# Run a pod-function invocation in a clean child shell. We deliberately
# avoid `complete -W` (it would error in a non-interactive shell) by
# stripping that line before sourcing.
run_pod_in_sandbox() {
    local sandbox_home="$1"
    shift
    HOME="$sandbox_home" bash --noprofile --norc -c '
        set -u
        # Strip the "complete -W ..." line (interactive-only) for the test shell.
        tmp_entry=$(mktemp)
        grep -v "^complete " "$HOME/.pod_agents" > "$tmp_entry"
        # shellcheck disable=SC1090
        source "$tmp_entry"
        rm -f "$tmp_entry"
        pod "$@"
    ' bash "$@"
}

# Regression: `return N` from a sourced lib only exits the source command,
# not the loop in pod(). The entrypoint uses a `return 99` sentinel at the
# end of every lib so it can distinguish "fell off the end → continue" from
# "explicit early return → propagate". This test guards both halves.

t_lib_sentinel_in_every_file() {
    local f rc=0
    for f in .pod_agents_config/lib/*.sh; do
        if ! tail -1 "$f" | grep -qE '^[[:space:]]*return 99'; then
            echo "  missing 'return 99' sentinel at end of $f" >&2
            rc=1
        fi
    done
    return "$rc"
}

t_entrypoint_handles_sentinel() {
    grep -qE '_pod_lib_rc[[:space:]]*=\$\?' .pod_agents \
        && grep -qE '\[[[:space:]]+"\$_pod_lib_rc"[[:space:]]+-ne[[:space:]]+99[[:space:]]+\]' .pod_agents
}

# `pod --help` previously printed help twice and fell through to lifecycle's
# "Unknown action" message (because `return 0` from the sourced lib didn't
# exit pod()). After the sentinel fix it should print the help exactly once.
t_pod_help_no_double_print() {
    local sandbox out unknown_count usage_count
    sandbox=$(setup_sandbox)
    out=$(run_pod_in_sandbox "$sandbox" --help 2>&1)
    rm -rf "$sandbox"
    unknown_count=$(printf '%s\n' "$out" | grep -c 'Unknown action' || true)
    usage_count=$(printf '%s\n' "$out" | grep -c '^Usage:$' || true)
    if [ "$unknown_count" -ne 0 ]; then
        echo "  pod --help fell through to 'Unknown action' ($unknown_count occurrences)" >&2
        return 1
    fi
    if [ "$usage_count" -ne 1 ]; then
        echo "  pod --help printed Usage line $usage_count times (expected 1)" >&2
        return 1
    fi
}

t_pod_help_runs() {
    local sandbox out rc
    sandbox=$(setup_sandbox)
    out=$(run_pod_in_sandbox "$sandbox" --help 2>&1)
    rc=$?
    rm -rf "$sandbox"
    if [ "$rc" -ne 0 ]; then
        echo "  pod --help exited $rc" >&2
        echo "$out" | sed 's/^/    /' >&2
        return 1
    fi
    if ! printf '%s' "$out" | grep -q 'Usage:'; then
        echo "  pod --help output missing 'Usage:'" >&2
        echo "$out" | sed 's/^/    /' >&2
        return 1
    fi
    if ! printf '%s' "$out" | grep -q 'pod-agents-manager'; then
        echo "  pod --help output missing 'pod-agents-manager'" >&2
        return 1
    fi
}

t_pod_version_runs() {
    local sandbox out rc
    sandbox=$(setup_sandbox)
    out=$(run_pod_in_sandbox "$sandbox" --version 2>&1)
    rc=$?
    rm -rf "$sandbox"
    if [ "$rc" -ne 0 ]; then
        echo "  pod --version exited $rc" >&2
        echo "$out" | sed 's/^/    /' >&2
        return 1
    fi
    printf '%s' "$out" | grep -qE '^pod-agents-manager [0-9]+\.[0-9]+\.[0-9]+'
}

# `pod --version` must reflect the value in version.conf — guards against a
# future refactor accidentally returning the `pod_version_default` fallback
# baked into .pod_agents (which exists only for the case where version.conf
# is missing or unreadable).
t_pod_version_matches_conf() {
    local sandbox out conf_version reported_version
    conf_version=$(grep '^POD_AGENTS_VERSION=' .pod_agents_config/version.conf \
        | head -n1 | cut -d'"' -f2)
    if [ -z "$conf_version" ]; then
        echo "  could not parse POD_AGENTS_VERSION from version.conf" >&2
        return 1
    fi
    sandbox=$(setup_sandbox)
    out=$(run_pod_in_sandbox "$sandbox" --version 2>&1)
    rm -rf "$sandbox"
    reported_version=$(printf '%s\n' "$out" | awk '/^pod-agents-manager/ {print $2; exit}')
    if [ "$reported_version" != "$conf_version" ]; then
        echo "  version.conf says \"$conf_version\" but pod --version reported \"$reported_version\"" >&2
        return 1
    fi
}

# `pod doctor` should run end-to-end and print the standard output structure,
# even when most checks FAIL on a developer machine (e.g. macOS without
# podman/systemd). We assert structure, not pass/fail counts.
t_pod_doctor_runs() {
    local sandbox out rc
    sandbox=$(setup_sandbox)
    out=$(run_pod_in_sandbox "$sandbox" doctor 2>&1)
    rc=$?
    rm -rf "$sandbox"
    # rc may be 1 if podman/systemd are absent — that's expected.
    # We only require that doctor produced its banner and a Summary line,
    # which proves it ran every section without crashing.
    if ! printf '%s' "$out" | grep -q 'pod-agents-manager doctor'; then
        echo "  doctor banner missing" >&2
        echo "$out" | sed 's/^/    /' >&2
        return 1
    fi
    if ! printf '%s' "$out" | grep -q 'Summary:'; then
        echo "  doctor summary line missing — check probably crashed mid-run" >&2
        echo "$out" | sed 's/^/    /' >&2
        return 1
    fi
    return 0
}

# Doctor's contract: exit 0 iff there are zero [FAIL] lines in its output,
# non-zero otherwise. This is environment-independent — works on macOS (where
# podman/systemctl are missing → FAILs) and on CI runners (where they may be
# present → no FAILs). Either way the contract must hold.
# `--model VAL` and `--model=VAL` should be stripped from the positional args
# in 30-early-flags so 50-arg-parse sees a clean contract. We can't run a real
# `pod start` (no podman on dev machines), so we exercise parsing via doctor:
# `pod doctor --model my-model` should still print the doctor banner instead
# of failing with "--model requires a value" or being treated as an action.
t_model_flag_parses() {
    local sandbox out rc
    sandbox=$(setup_sandbox)
    out=$(run_pod_in_sandbox "$sandbox" doctor --model my-test-model 2>&1)
    rc=$?
    rm -rf "$sandbox"
    if ! printf '%s' "$out" | grep -q 'pod-agents-manager doctor'; then
        echo "  --model VAL was not stripped before doctor ran (rc=$rc)" >&2
        echo "$out" | head -5 | sed 's/^/    /' >&2
        return 1
    fi
}

t_model_flag_eq_form_parses() {
    local sandbox out
    sandbox=$(setup_sandbox)
    out=$(run_pod_in_sandbox "$sandbox" doctor --model=my-test-model 2>&1)
    rm -rf "$sandbox"
    printf '%s' "$out" | grep -q 'pod-agents-manager doctor'
}

t_model_flag_missing_value_errors() {
    local sandbox out rc
    sandbox=$(setup_sandbox)
    out=$(run_pod_in_sandbox "$sandbox" doctor --model 2>&1)
    rc=$?
    rm -rf "$sandbox"
    [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q '\-\-model requires a value'
}

# When ~/.pod_agents_config/.cmd_name contains a custom name (e.g. "pods" for
# hosts that already have CocoaPods at /usr/bin/pod), the entrypoint must
# define the user-facing function under THAT name and the help text should
# reflect it. Internal lib re-entries always use the canonical
# `_pod_agents_main`, so they keep working regardless.
t_alias_custom_name() {
    local sandbox out
    sandbox=$(setup_sandbox)
    printf 'pods\n' > "$sandbox/.pod_agents_config/.cmd_name"
    out=$(HOME="$sandbox" bash --noprofile --norc -c '
        set -u
        # Strip the "complete -W ..." line(s) — interactive-only and would error
        # under non-interactive bash.
        tmp_entry=$(mktemp)
        grep -v "^complete " "$HOME/.pod_agents" > "$tmp_entry"
        # shellcheck disable=SC1090
        source "$tmp_entry"
        rm -f "$tmp_entry"

        # The default name MUST NOT exist as a function.
        if declare -F pod >/dev/null; then echo "ERROR: pod still defined"; exit 2; fi
        # The chosen name MUST exist and be callable.
        if ! declare -F pods >/dev/null; then echo "ERROR: pods not defined"; exit 3; fi
        pods --help
    ' 2>&1)
    rm -rf "$sandbox"
    if ! printf '%s' "$out" | grep -q '^Usage:$'; then
        echo "  alias-named function failed to print help" >&2
        echo "$out" | head -8 | sed 's/^/    /' >&2
        return 1
    fi
    # Help text should show "pods" (the alias) in usage examples, not literal "pod".
    if ! printf '%s' "$out" | grep -qE '^[[:space:]]+pods (config|doctor|start)'; then
        echo "  help text does not reflect the chosen alias 'pods'" >&2
        echo "$out" | head -20 | sed 's/^/    /' >&2
        return 1
    fi
}

t_pod_doctor_exit_matches_fail_count() {
    local sandbox out rc fail_count
    sandbox=$(setup_sandbox)
    out=$(run_pod_in_sandbox "$sandbox" doctor 2>&1)
    rc=$?
    rm -rf "$sandbox"
    fail_count=$(printf '%s\n' "$out" | grep -c '\[FAIL\]' || true)
    if [ "$fail_count" -eq 0 ] && [ "$rc" -ne 0 ]; then
        echo "  doctor reported 0 fails but exited $rc" >&2
        return 1
    fi
    if [ "$fail_count" -gt 0 ] && [ "$rc" -eq 0 ]; then
        echo "  doctor reported $fail_count fail(s) but exited 0" >&2
        return 1
    fi
}

# Missing lib dir should produce a clear diagnostic and non-zero return.
t_pod_errors_when_lib_missing() {
    local sandbox out rc
    sandbox=$(setup_sandbox)
    rm -rf "$sandbox/.pod_agents_config/lib"
    out=$(run_pod_in_sandbox "$sandbox" --help 2>&1)
    rc=$?
    rm -rf "$sandbox"
    if [ "$rc" -eq 0 ]; then
        echo "  pod returned 0 even though lib/ was missing" >&2
        return 1
    fi
    printf '%s' "$out" | grep -q 'No library modules found'
}

# ----- 6. Helper-function unit tests ---------------------------------------
# Source pod once to define the inner helpers globally, then exercise them.

t_helpers_unit() {
    local sandbox
    sandbox=$(setup_sandbox)
    HOME="$sandbox" bash --noprofile --norc -c '
        set -u
        tmp_entry=$(mktemp)
        grep -v "^complete " "$HOME/.pod_agents" > "$tmp_entry"
        # shellcheck disable=SC1090
        source "$tmp_entry"
        rm -f "$tmp_entry"

        # Run pod once with --version so all libs are sourced and the inner
        # helper functions become globally available in this shell.
        pod --version >/dev/null

        rc=0

        # _resolve_base_image: alpine alias
        _resolve_base_image alpine
        if [ "$BASE_IMAGE_TAG" != "alpine" ] || [[ "$BASE_IMAGE_FULL" != *alpine* ]]; then
            echo "  alpine alias resolved wrong: tag=$BASE_IMAGE_TAG full=$BASE_IMAGE_FULL" >&2
            rc=1
        fi

        # _resolve_base_image: trixie aliases
        _resolve_base_image trixie-slim
        if [ "$BASE_IMAGE_TAG" != "trixie" ] || [[ "$BASE_IMAGE_FULL" != *trixie-slim* ]]; then
            echo "  trixie-slim resolved wrong: tag=$BASE_IMAGE_TAG full=$BASE_IMAGE_FULL" >&2
            rc=1
        fi

        # _resolve_base_image: explicit image — full kept as-is, tag sanitised
        _resolve_base_image "registry.example.com/foo/bar:1.2"
        if [ "$BASE_IMAGE_FULL" != "registry.example.com/foo/bar:1.2" ]; then
            echo "  explicit image FULL mangled: $BASE_IMAGE_FULL" >&2
            rc=1
        fi
        if [[ "$BASE_IMAGE_TAG" == *[/:]* ]]; then
            echo "  explicit image TAG still contains / or :: $BASE_IMAGE_TAG" >&2
            rc=1
        fi

        # _pod_env_value_needs_setup: empty/placeholder cases
        for v in "" "<my-key>" "CHANGE_ME" "change-me" "changeme" "__SET_ME__"; do
            if ! _pod_env_value_needs_setup "$v"; then
                echo "  expected needs-setup for value: \"$v\"" >&2
                rc=1
            fi
        done

        # _pod_env_value_needs_setup: real values must NOT match
        for v in "real-value" "sk-abc123" "http://localhost:8000"; do
            if _pod_env_value_needs_setup "$v"; then
                echo "  real value flagged as needs-setup: \"$v\"" >&2
                rc=1
            fi
        done

        exit "$rc"
    '
    local rc=$?
    rm -rf "$sandbox"
    return "$rc"
}

# ---------------------------------------------------------------------------

echo "==> pod-agents-manager test suite"
echo "    repo: $repo_root"
echo

run_test "syntax: .pod_agents"                 t_syntax_entrypoint
run_test "syntax: lib/*.sh"                    t_syntax_libs
run_test "syntax: install.sh"                  t_syntax_install

if t_shellcheck_available; then
    run_test "shellcheck: .pod_agents"         t_shellcheck_entrypoint
    run_test "shellcheck: lib/*.sh"            t_shellcheck_libs
    run_test "shellcheck: install.sh"          t_shellcheck_install
else
    printf '\033[33m[SKIP]\033[0m shellcheck not installed; skipping lint tests\n'
fi

run_test "lib files: numeric NN- prefix"       t_libs_numeric_prefix
run_test "lib files: lex order = numeric"      t_libs_sort_matches_intent

run_test "install.sh copies lib/"              t_install_copies_lib
run_test "self-update syncs lib/"              t_self_update_copies_lib
run_test "self-update seeds .env from example" t_self_update_seeds_env_from_example
run_test "entrypoint sources lib glob"         t_entrypoint_sources_lib_glob

run_test "lib: 'return 99' sentinel present"   t_lib_sentinel_in_every_file
run_test "entrypoint: handles 99 sentinel"     t_entrypoint_handles_sentinel
run_test "smoke: pod --help"                   t_pod_help_runs
run_test "smoke: pod --help prints once"       t_pod_help_no_double_print
run_test "smoke: pod --version"                t_pod_version_runs
run_test "version: matches version.conf"       t_pod_version_matches_conf
run_test "smoke: pod errors w/o lib/"          t_pod_errors_when_lib_missing
run_test "smoke: pod doctor runs"              t_pod_doctor_runs
run_test "smoke: pod doctor exit ↔ fail count" t_pod_doctor_exit_matches_fail_count
run_test "model flag: --model VAL parses"      t_model_flag_parses
run_test "model flag: --model=VAL parses"      t_model_flag_eq_form_parses
run_test "model flag: missing value errors"    t_model_flag_missing_value_errors
run_test "alias: custom .cmd_name binds func"  t_alias_custom_name
run_test "unit: inner helper functions"        t_helpers_unit

echo
echo "==> Summary: $passes passed, $fails failed"
if [ "$fails" -gt 0 ]; then
    printf 'Failed:\n'
    for n in "${failed_names[@]}"; do
        printf '  - %s\n' "$n"
    done
    exit 1
fi
exit 0

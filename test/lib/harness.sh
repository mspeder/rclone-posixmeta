#!/usr/bin/env bash
# shellcheck shell=bash
#
# Shared test harness for rclone-posixmeta.
#
# This file is sourced by test/run.sh (parent discovery mode) and by the
# per-test child process. It must not enable `set -e` itself: the runner
# enables strict mode only inside the per-test child shell.
#
# Conventions:
#   * Every test is a function named `test_<something>`.
#   * Assertion helpers call `pm_fail`, which returns non-zero; the child
#     shell runs with `set -Eeuo pipefail`, so a failed assertion aborts the
#     test and the ERR trap reports the failing line.
#   * Tests run in throwaway directories under $PM_RUN_ROOT and are isolated
#     from the user's ~/.config/rclone and ~/.cache/rclone via --config,
#     --cache-dir and HOME/XDG_* overrides.

# ---------------------------------------------------------------------------
# Globals
# ---------------------------------------------------------------------------

: "${PM_BINARY:?PM_BINARY must be set by test/run.sh}"
: "${PM_RUN_ROOT:?PM_RUN_ROOT must be set by test/run.sh}"

PM_SUITE_SKIP_REASON=""
declare -A PM_CFG_OPTS=() # posixmeta config overrides for helpers
PM_MOUNTS=()
PM_MOUNT_PIDS=()
PM_MOUNT_LOGS=()

# ANSI colours are only emitted on a TTY.
if [[ -t 2 ]]; then
	PM_C_RED=$'\033[31m'
	PM_C_GREEN=$'\033[32m'
	PM_C_YELLOW=$'\033[33m'
	PM_C_BLUE=$'\033[34m'
	PM_C_BOLD=$'\033[1m'
	PM_C_RESET=$'\033[0m'
else
	PM_C_RED=""
	PM_C_GREEN=""
	PM_C_YELLOW=""
	PM_C_BLUE=""
	PM_C_BOLD=""
	PM_C_RESET=""
fi

# ---------------------------------------------------------------------------
# Logging / failure primitives
# ---------------------------------------------------------------------------

pm_fail() {
	printf '%bASSERT FAILED%b: %s\n' "$PM_C_RED" "$PM_C_RESET" "$*" >&2
	return 1
}

pm_note() {
	printf '# %s\n' "$*" >&2
}

skip() {
	printf '%s\n' "${*:-unspecified}" >"$PM_MARKER_DIR/skip"
	exit 77
}

xfail() {
	printf '%s\n' "${*:-expected failure}" >"$PM_MARKER_DIR/xfail"
}

pm_on_error() {
	local rc=$1 line=$2
	printf '%bFAILED%b: test %s exited rc=%s at line %s\n' \
		"$PM_C_RED" "$PM_C_RESET" "${PM_TEST_NAME:-?}" "$rc" "$line" >&2
}

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

pm_assert_eq() {
	local want=$1 got=$2 msg=${3:-}
	if [[ $want != "$got" ]]; then
		pm_fail "${msg:+$msg: }expected [$want], got [$got]"
	fi
}

pm_assert_ne() {
	local unexpected=$1 got=$2 msg=${3:-}
	if [[ $unexpected == "$got" ]]; then
		pm_fail "${msg:+$msg: }did not expect [$got]"
	fi
}

pm_assert_contains() {
	local haystack=$1 needle=$2 msg=${3:-}
	if [[ $haystack != *"$needle"* ]]; then
		pm_fail "${msg:+$msg: }[$haystack] does not contain [$needle]"
	fi
}

pm_assert_not_contains() {
	local haystack=$1 needle=$2 msg=${3:-}
	if [[ $haystack == *"$needle"* ]]; then
		pm_fail "${msg:+$msg: }[$haystack] unexpectedly contains [$needle]"
	fi
}

pm_assert_file() {
	[[ -f $1 ]] || pm_fail "expected file to exist: $1"
}

pm_assert_no_file() {
	[[ ! -e $1 ]] || pm_fail "expected path not to exist: $1"
}

pm_assert_dir() {
	[[ -d $1 ]] || pm_fail "expected directory to exist: $1"
}

pm_assert_success() {
	local desc=$1
	shift
	if ! "$@"; then
		pm_fail "expected success: $desc: $*"
	fi
}

pm_assert_failure() {
	local desc=$1
	shift
	local rc=0
	"$@" >/dev/null 2>&1 || rc=$?
	((rc != 0)) || pm_fail "expected failure but command succeeded: $desc: $*"
}

# pm_assert_json <json> <jq-filter> <expected> [message]
pm_assert_json() {
	local json=$1 filter=$2 want=$3 msg=${4:-jq "$filter"}
	local got
	if ! got=$(jq -r "$filter" <<<"$json" 2>/dev/null); then
		pm_fail "$msg: invalid JSON or filter"
	fi
	pm_assert_eq "$want" "$got" "$msg"
}

# pm_assert_json_true <json> <jq-filter> [message]
pm_assert_json_true() {
	local json=$1 filter=$2 msg=${3:-jq "$filter"}
	local got
	got=$(jq -r "$filter" <<<"$json" 2>/dev/null) || pm_fail "$msg: invalid JSON"
	pm_assert_eq "true" "$got" "$msg"
}


# ---------------------------------------------------------------------------
# Per-test environment
# ---------------------------------------------------------------------------

pm_init_test_env() {
	# Preserve the real Go caches before HOME is redirected into the scratch
	# tree, otherwise `go test` would rebuild/re-download the dependency graph.
	if [[ -z ${GOCACHE:-} ]]; then
		export GOCACHE="$HOME/.cache/go-build"
	fi
	if [[ -z ${GOPATH:-} ]]; then
		export GOPATH="$HOME/go"
	fi

	PM_SCRATCH="$PM_TEST_SCRATCH"
	PM_SRC="$PM_SCRATCH/src"
	PM_IN="$PM_SCRATCH/in"
	PM_MNT="$PM_SCRATCH/mnt"
	PM_VFS_CACHE="$PM_SCRATCH/vfs-cache"
	PM_CONFIG="$PM_SCRATCH/rclone.conf"
	PM_HOME="$PM_SCRATCH/home"
	declare -gA PM_CFG_OPTS=()
	PM_MOUNTS=()
	PM_MOUNT_PIDS=()
	PM_MOUNT_LOGS=()

	mkdir -p "$PM_SRC" "$PM_IN" "$PM_MNT" "$PM_VFS_CACHE" \
		"$PM_HOME/.config" "$PM_HOME/.cache"

	# Keep every rclone code path away from the developer's real state.
	export HOME="$PM_HOME"
	export XDG_CONFIG_HOME="$PM_HOME/.config"
	export XDG_CACHE_HOME="$PM_HOME/.cache"
	export RCLONE_CONFIG="$PM_CONFIG"
	export RCLONE_CACHE_DIR="$PM_VFS_CACHE"
}

# ---------------------------------------------------------------------------
# rclone invocation and config generation
# ---------------------------------------------------------------------------

# pm <rclone args...> — runs the harness binary with isolated config/cache.
pm() {
	"$PM_BINARY" \
		--config "$PM_CONFIG" \
		--cache-dir "$PM_VFS_CACHE" \
		--stats 0 \
		"$@"
}

pm_obscure() {
	"$PM_BINARY" obscure "$1" 2>/dev/null
}

# _pm_config_pm_section <remote> — emits the [pm] posixmeta section.
_pm_config_pm_section() {
	local remote=$1
	local -A opts=(
		[default_file_mode]=0644
		[default_dir_mode]=0755
		[default_uid]=1000
		[default_gid]=1000
		[inherit_parent_owner]=true
		[sidecar_suffix]=.posixmeta
		[hide_sidecars]=true
		[cache_ttl]=5m
		[writeback_delay]=50ms
	)
	local -a order=(
		default_file_mode default_dir_mode default_uid default_gid
		inherit_parent_owner sidecar_suffix hide_sidecars cache_ttl
		writeback_delay
	)
	local key known k
	for key in "${!PM_CFG_OPTS[@]}"; do
		opts[$key]=${PM_CFG_OPTS[$key]}
	done

	printf '[pm]\n'
	printf 'type = posixmeta\n'
	printf 'remote = %s\n' "$remote"
	for key in "${order[@]}"; do
		printf '%s = %s\n' "$key" "${opts[$key]}"
	done

	# Emit any extra (non-default) options in a deterministic order.
	for key in "${!opts[@]}"; do
		known=0
		for k in "${order[@]}"; do
			[[ $k == "$key" ]] && known=1 && break
		done
		((known == 1)) || printf '%s = %s\n' "$key" "${opts[$key]}"
	done | sort
}

# pm_write_local_config <config> <raw-dir>
# Layout: pm: -> local:<raw-dir>
pm_write_local_config() {
	local conf=$1 raw=$2
	{
		printf '[raw]\n'
		printf 'type = local\n'
		printf '\n'
		_pm_config_pm_section "raw:$raw"
	} >"$conf"
}

# pm_write_crypt_config <config> <raw-dir>
# Layout: pm: -> crypt(local:<raw-dir>).  Passwords are obscured as required
# by rclone; plaintext crypt passwords are rejected at runtime.
pm_write_crypt_config() {
	local conf=$1 raw=$2
	local pass salt
	pass=$(pm_obscure "harness-crypt-password")
	salt=$(pm_obscure "harness-crypt-salt")
	{
		printf '[raw_store]\n'
		printf 'type = local\n'
		printf '\n'
		printf '[enc_store]\n'
		printf 'type = crypt\n'
		printf 'remote = raw_store:%s\n' "$raw"
		printf 'password = %s\n' "$pass"
		printf 'password2 = %s\n' "$salt"
		printf '\n'
		_pm_config_pm_section "enc_store:"
	} >"$conf"
}

# ---------------------------------------------------------------------------
# Sidecar / metadata helpers
# ---------------------------------------------------------------------------

# pm_sidecar_path <remote-relative-path> — echo expected on-disk sidecar path.
pm_sidecar_path() {
	printf '%s/%s.posixmeta\n' "$PM_SRC" "$1"
}

# pm_sidecar_json <remote-relative-path> — echo raw sidecar JSON.
pm_sidecar_json() {
	cat "$(pm_sidecar_path "$1")"
}

# pm_meta_of <parent-remote> <entry-name> <metadata-key>
# Lists recursively from the remote root so ancestor directories are in scope
# for inherit_parent_owner regardless of which subpath is queried.
pm_meta_of() {
	local parent=$1 name=$2 key=$3 rel
	rel="${parent#pm:}"
	rel="${rel#/}"
	if [[ -n $rel ]]; then
		rel="$rel/$name"
	else
		rel="$name"
	fi

	pm lsjson --metadata --recursive pm: 2>/dev/null |
		jq -r --arg p "$rel" --arg k "$key" \
			'.[] | select(.Path == $p) | .Metadata[$k] // empty' |
		head -n 1
}

# pm_meta_is <parent-remote> <entry-name> <key> <expected>
pm_meta_is() {
	[[ $(pm_meta_of "$1" "$2" "$3") == "$4" ]]
}

# pm_wait_meta <parent> <name> <key> <expected> [timeout-seconds]
pm_wait_meta() {
	local parent=$1 name=$2 key=$3 want=$4 secs=${5:-10}
	pm_wait_for "$secs" pm_meta_is "$parent" "$name" "$key" "$want"
}

# pm_entry_exists <parent-remote> <entry-name>
pm_entry_exists() {
	local parent=$1 name=$2
	pm lsjson "$parent" 2>/dev/null |
		jq -e --arg n "$name" 'any(.[]; .Name == $n)' >/dev/null
}

# pm_wait_entry <parent> <name> [timeout-seconds]
pm_wait_entry() {
	pm_wait_for "${3:-10}" pm_entry_exists "$1" "$2"
}

pm_entry_missing() {
	! pm_entry_exists "$1" "$2"
}

# pm_wait_entry_gone <parent> <name> [timeout-seconds]
pm_wait_entry_gone() {
	pm_wait_for "${3:-10}" pm_entry_missing "$1" "$2"
}

# pm_wait_empty <remote-dir> [timeout-seconds]
pm_wait_empty() {
	pm_wait_for "${2:-10}" pm_dir_is_empty "$1"
}

pm_dir_is_empty() {
	[[ $(pm lsjson "$1" 2>/dev/null | jq 'length') == "0" ]]
}

# pm_assert_raw_has_no_plaintext <marker>
pm_assert_raw_has_no_plaintext() {
	local marker=$1 hits
	hits=$(find "$PM_SRC" \( -name "*${marker}*" -o -name '*.posixmeta' \) \
		-print -quit 2>/dev/null || true)
	[[ -z $hits ]] || pm_fail "plaintext name or sidecar leaked into raw storage: $hits"
}

# pm_assert_mount_has_no_sidecars <mountpoint>
pm_assert_mount_has_no_sidecars() {
	local mnt=$1 hits
	hits=$(find "$mnt" -name '*.posixmeta' -print -quit 2>/dev/null || true)
	[[ -z $hits ]] || pm_fail "sidecar leaked into FUSE mount: $hits"
}

# ---------------------------------------------------------------------------
# Polling and process/mount management
# ---------------------------------------------------------------------------

# pm_wait_for <seconds> <command...> — retry until success or timeout.
pm_wait_for() {
	local seconds=$1
	shift
	local deadline=$((SECONDS + seconds))
	while ! "$@" >/dev/null 2>&1; do
		if ((SECONDS >= deadline)); then
			printf '%bTIMEOUT%b after %ss waiting for: %s\n' \
				"$PM_C_RED" "$PM_C_RESET" "$seconds" "$*" >&2
			return 1
		fi
		sleep 0.1
	done
}

# pm_mount_start <remote> <mountpoint> [extra rclone mount flags...]
# Mounts in the background (tracked for cleanup) and waits until the kernel
# reports the mountpoint as mounted.
pm_mount_start() {
	local remote=$1 mnt=$2
	shift 2
	mkdir -p "$mnt"
	local log="$PM_TEST_SCRATCH/mount.$(printf '%s' "$mnt" | tr '/' '_').log"

	"$PM_BINARY" \
		--config "$PM_CONFIG" \
		--cache-dir "$PM_VFS_CACHE" \
		mount "$remote" "$mnt" \
		--log-file "$log" \
		"$@" </dev/null &
	local pid=$!

	PM_MOUNTS+=("$mnt")
	PM_MOUNT_PIDS+=("$pid")
	PM_MOUNT_LOGS+=("$log")

	if ! pm_wait_for 20 mountpoint -q "$mnt"; then
		{
			printf '%s--- mount log: %s ---%s\n' "$PM_C_YELLOW" "$log" "$PM_C_RESET"
			cat "$log" 2>/dev/null || true
		} >&2
		return 1
	fi
}

# pm_mount_stop <mountpoint> — unmount, reap the mount process, and fall back
# to lazy unmount / SIGKILL if the FUSE filesystem refuses to go away.
pm_mount_stop() {
	local mnt=$1 i pid="" log=""
	for i in "${!PM_MOUNTS[@]}"; do
		if [[ ${PM_MOUNTS[i]} == "$mnt" ]]; then
			pid=${PM_MOUNT_PIDS[i]}
			log=${PM_MOUNT_LOGS[i]}
			break
		fi
	done

	local n
	for n in {1..50}; do
		mountpoint -q "$mnt" || break
		fusermount3 -u "$mnt" 2>/dev/null ||
			fusermount -u "$mnt" 2>/dev/null ||
			umount "$mnt" 2>/dev/null || true
		sleep 0.1
	done
	if mountpoint -q "$mnt"; then
		umount -l "$mnt" 2>/dev/null || true
		sleep 0.2
	fi

	if [[ -n $pid ]]; then
		for n in {1..50}; do
			kill -0 "$pid" 2>/dev/null || break
			sleep 0.1
		done
		if kill -0 "$pid" 2>/dev/null; then
			kill -TERM "$pid" 2>/dev/null || true
			sleep 0.2
			kill -KILL "$pid" 2>/dev/null || true
		fi
		wait "$pid" 2>/dev/null || true
	fi

	if mountpoint -q "$mnt"; then
		printf '%bWARNING%b: failed to unmount %s\n' \
			"$PM_C_YELLOW" "$PM_C_RESET" "$mnt" >&2
		if [[ -n $log ]]; then
			tail -n 50 "$log" >&2 || true
		fi
		return 1
	fi
}

pm_mount_stop_all() {
	local mnt
	# Iterate over a snapshot: pm_mount_stop does not mutate the arrays, but a
	# copy keeps this safe if that ever changes.
	for mnt in "${PM_MOUNTS[@]:-}"; do
		[[ -n $mnt ]] || continue
		mountpoint -q "$mnt" && pm_mount_stop "$mnt" || true
	done
	# Reap any remaining mount processes.
	local pid
	for pid in "${PM_MOUNT_PIDS[@]:-}"; do
		[[ -n $pid ]] || continue
		kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
}

pm_cleanup() {
	pm_mount_stop_all
}

# ---------------------------------------------------------------------------
# Suite capability requirements (called at suite top level)
# ---------------------------------------------------------------------------

suite_require_cmd() {
	local cmd
	for cmd in "$@"; do
		if ! command -v "$cmd" >/dev/null 2>&1; then
			PM_SUITE_SKIP_REASON="required command not found: $cmd"
			return 0
		fi
	done
}

suite_require_fuse() {
	if [[ ! -e /dev/fuse ]]; then
		PM_SUITE_SKIP_REASON="FUSE unavailable: /dev/fuse missing"
		return 0
	fi
	if ! command -v fusermount3 >/dev/null 2>&1 &&
		! command -v fusermount >/dev/null 2>&1; then
		PM_SUITE_SKIP_REASON="FUSE unavailable: fusermount not installed"
	fi
}

# ---------------------------------------------------------------------------
# Child-mode entry point
# ---------------------------------------------------------------------------

# pm_child_run <test-function> — invoked in a fresh process by test/run.sh.
pm_child_run() {
	local name=$1
	mkdir -p "$PM_TEST_SCRATCH" "$PM_MARKER_DIR"
	pm_init_test_env
	trap pm_cleanup EXIT
	set -Eeuo pipefail
	trap 'pm_on_error "$?" "$LINENO"' ERR
	"$name"
}

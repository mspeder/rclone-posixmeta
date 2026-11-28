#!/usr/bin/env bash
# shellcheck shell=bash
#
# test/run.sh — test harness entry point for rclone-posixmeta.
#
# Usage:
#   ./test/run.sh [options] [suite ...]
#
# Options:
#   --binary PATH        rclone binary to test (default: ./rclone-posixmeta)
#   --build              force a CGO-enabled rebuild before running
#   --no-build           never build; fail if the binary is missing
#   --filter REGEX       only run tests whose "suite/test" matches REGEX
#   --list               list discovered tests and exit
#   --timeout SECONDS    per-test timeout (default: 180)
#   --keep               keep scratch directories/logs for passing tests
#   --verbose            stream each test's output as it runs
#   --tap                emit TAP-13 output
#   --strict-xpass       treat an unexpected XPASS as a failure
#   -h, --help           show this help
#
# Suites are the files in test/suites/*.sh. A suite may be selected by name
# ("01-cli"), by file name ("01-cli.sh") or by path.
#
# Each test runs in its own process with an isolated HOME, rclone config,
# cache directory and scratch tree under $PM_RUN_ROOT. FUSE mounts are torn
# down by an EXIT trap in the child and swept again by the parent.

set -uo pipefail

# Pin the umask: rclone mount derives --dir-perms/--file-perms and the FUSE
# create modes (which posixmeta now records) from it, so a different developer
# umask would silently change the expected modes.
umask 022

TEST_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$TEST_DIR/.." && pwd)
HARNESS_LIB="$TEST_DIR/lib/harness.sh"
SUITES_DIR="$TEST_DIR/suites"

DEFAULT_TIMEOUT=180
TAP_VERSION=13

# ---------------------------------------------------------------------------
# Child mode: run exactly one test in a fresh process.
# ---------------------------------------------------------------------------
if [[ ${1:-} == "--child" ]]; then
	shift
	if (($# != 2)); then
		echo "usage: run.sh --child <suite-file> <test-function>" >&2
		exit 2
	fi
	suite_file=$1
	test_name=$2
	# shellcheck source=lib/harness.sh
	source "$HARNESS_LIB"
	# shellcheck source=/dev/null
	source "$suite_file"
	pm_child_run "$test_name"
	exit $?
fi

# ---------------------------------------------------------------------------
# Parent mode
# ---------------------------------------------------------------------------

BUILD_MODE=auto
FILTER=""
DO_LIST=0
KEEP=0
VERBOSE=0
TAP=0
STRICT_XPASS=0
PM_TEST_TIMEOUT=$DEFAULT_TIMEOUT
PM_BINARY="$REPO_ROOT/rclone-posixmeta"
POSITIONAL=()

# Remove the scratch tree when we created it and nothing ran in it yet
# (e.g. --list, no matching filter, build failure).
discard_run_root() {
	[[ ${PM_RUN_ROOT_CREATED:-0} == 1 ]] && rm -rf "$PM_RUN_ROOT"
	return 0
}

die() {
	printf 'run.sh: %s\n' "$*" >&2
	discard_run_root
	exit 2
}

usage() {
	sed -n '2,27p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while (($# > 0)); do
	case $1 in
	--binary)
		[[ $# -ge 2 ]] || die "--binary requires a path"
		PM_BINARY=$2
		shift 2
		;;
	--build)
		BUILD_MODE=force
		shift
		;;
	--no-build)
		BUILD_MODE=never
		shift
		;;
	--filter)
		[[ $# -ge 2 ]] || die "--filter requires a regex"
		FILTER=$2
		shift 2
		;;
	--list)
		DO_LIST=1
		shift
		;;
	--timeout)
		[[ $# -ge 2 ]] || die "--timeout requires seconds"
		PM_TEST_TIMEOUT=$2
		shift 2
		;;
	--keep)
		KEEP=1
		shift
		;;
	--verbose)
		VERBOSE=1
		shift
		;;
	--tap)
		TAP=1
		shift
		;;
	--strict-xpass)
		STRICT_XPASS=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	--)
		shift
		POSITIONAL+=("$@")
		break
		;;
	-*)
		die "unknown option: $1 (try --help)"
		;;
	*)
		POSITIONAL+=("$1")
		shift
		;;
	esac
done

[[ $PM_TEST_TIMEOUT =~ ^[1-9][0-9]*$ ]] || die "--timeout must be a positive integer"

# A user-supplied --binary is assumed to be ready unless --build is explicit.
if [[ $BUILD_MODE == auto && $PM_BINARY != "$REPO_ROOT/rclone-posixmeta" ]]; then
	BUILD_MODE=never
fi
PM_BINARY=$(readlink -f -- "$PM_BINARY" 2>/dev/null || printf '%s' "$PM_BINARY")

if [[ -z ${PM_RUN_ROOT:-} ]]; then
	PM_RUN_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pm-harness.XXXXXX")
	PM_RUN_ROOT_CREATED=1
else
	PM_RUN_ROOT_CREATED=0
fi
mkdir -p "$PM_RUN_ROOT/work" "$PM_RUN_ROOT/logs" "$PM_RUN_ROOT/markers"
export PM_BINARY PM_RUN_ROOT PM_REPO_ROOT="$REPO_ROOT"

# The harness must be sourced in the parent for suite capability helpers such
# as suite_require_fuse().
# shellcheck source=lib/harness.sh
source "$HARNESS_LIB"

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

needs_build() {
	case $BUILD_MODE in
	never)
		[[ -x $PM_BINARY ]] || die "binary not found or not executable: $PM_BINARY (remove --no-build)"
		return 1
		;;
	force)
		return 0
		;;
	esac
	[[ -x $PM_BINARY ]] || return 0
	# Rebuild when a tracked Go source is newer than the binary.
	[[ -n $(find "$REPO_ROOT" -name '*.go' -newer "$PM_BINARY" -print -quit 2>/dev/null) ]]
}

build_binary() {
	needs_build || return 0
	command -v go >/dev/null 2>&1 || die "go toolchain not found; use --binary or --no-build"
	# The rclone fork is materialised from third_party/patches/*.patch
	# and is not versioned, so a fresh clone has to bootstrap it first.
	if [[ ! -f $REPO_ROOT/third_party/rclone/.posixmeta-fork ]]; then
		printf 'Materialising third_party/rclone from patch\n'
		if ! "$REPO_ROOT/scripts/setup-fork.sh" >&2; then
			die "could not materialise third_party/rclone (run scripts/setup-fork.sh)"
		fi
	fi
	printf 'Building %s\n' "$PM_BINARY"
	local build_log="$PM_RUN_ROOT/build.log"
	if ! (cd "$REPO_ROOT" && CGO_ENABLED=1 go build -o "$PM_BINARY" .) >"$build_log" 2>&1; then
		cat "$build_log" >&2
		die "go build failed (log: $build_log)"
	fi
}

# ---------------------------------------------------------------------------
# Suite discovery
# ---------------------------------------------------------------------------

SUITE_FILES=()
SUITE_NAMES=()
SUITE_SKIPS=()

resolve_suite() {
	local arg=$1
	if [[ -f $arg ]]; then
		readlink -f -- "$arg"
		return
	fi
	if [[ -f "$SUITES_DIR/$arg" ]]; then
		readlink -f -- "$SUITES_DIR/$arg"
		return
	fi
	if [[ -f "$SUITES_DIR/$arg.sh" ]]; then
		readlink -f -- "$SUITES_DIR/$arg.sh"
		return
	fi
	die "unknown suite: $arg"
}

if ((${#POSITIONAL[@]} > 0)); then
	for arg in "${POSITIONAL[@]}"; do
		SUITE_FILES+=("$(resolve_suite "$arg")")
	done
else
	while IFS= read -r file; do
		SUITE_FILES+=("$file")
	done < <(find "$SUITES_DIR" -maxdepth 1 -name '*.sh' -type f | sort)
fi

((${#SUITE_FILES[@]} > 0)) || die "no suites found in $SUITES_DIR"

for suite_file in "${SUITE_FILES[@]}"; do
	suite_name=$(basename "$suite_file" .sh)
	before=$(compgen -A function | sort)
	PM_SUITE_SKIP_REASON=""
	# shellcheck source=/dev/null
	source "$suite_file"
	after=$(compgen -A function | sort)

	# Drop functions the suite defined so the next suite cannot collide.
	mapfile -t new_funcs < <(comm -13 <(printf '%s\n' "$before") <(printf '%s\n' "$after") | sed '/^$/d')
	if ((${#new_funcs[@]} > 0)); then
		unset -f "${new_funcs[@]}" 2>/dev/null || true
	fi

	SUITE_NAMES+=("$suite_name")
	SUITE_SKIPS+=("$PM_SUITE_SKIP_REASON")
done

# ---------------------------------------------------------------------------
# Selection
# ---------------------------------------------------------------------------

RUN_SUITE=()
RUN_TEST=()
RUN_ID=()

for i in "${!SUITE_FILES[@]}"; do
	suite_file=${SUITE_FILES[i]}
	suite_name=${SUITE_NAMES[i]}
	mapfile -t tests < <(grep -oE '^test_[A-Za-z0-9_]+\(\)' "$suite_file" | sed 's/()$//')
	for test_name in "${tests[@]}"; do
		id="$suite_name/$test_name"
		if [[ -n $FILTER ]] && ! [[ $id =~ $FILTER ]]; then
			continue
		fi
		RUN_SUITE+=("$i")
		RUN_TEST+=("$test_name")
		RUN_ID+=("$id")
	done
done

if ((DO_LIST)); then
	for i in "${!RUN_ID[@]}"; do
		suite_idx=${RUN_SUITE[i]}
		skip=${SUITE_SKIPS[suite_idx]}
		if [[ -n $skip ]]; then
			printf '%s # SKIP %s\n' "${RUN_ID[i]}" "$skip"
		else
			printf '%s\n' "${RUN_ID[i]}"
		fi
	done
	discard_run_root
	exit 0
fi

if ((${#RUN_ID[@]} == 0)); then
	die "no tests selected (filter: ${FILTER:-none})"
fi


# ---------------------------------------------------------------------------
# Result bookkeeping
# ---------------------------------------------------------------------------

declare -A COUNTS=(
	[pass]=0
	[fail]=0
	[skip]=0
	[xfail]=0
	[xpass]=0
)
TAP_INDEX=0

tap_escape() {
	printf '%s' "$1" | tr '\n' ' '
}

record_result() {
	local status=$1 id=$2 reason=${3:-} log=${4:-}
	COUNTS[$status]=$((${COUNTS[$status]} + 1))

	if ((TAP)); then
		TAP_INDEX=$((TAP_INDEX + 1))
		case $status in
		pass) printf 'ok %d - %s\n' "$TAP_INDEX" "$id" ;;
		skip) printf 'ok %d - %s # SKIP %s\n' "$TAP_INDEX" "$id" "$(tap_escape "$reason")" ;;
		xfail) printf 'not ok %d - %s # TODO expected failure: %s\n' "$TAP_INDEX" "$id" "$(tap_escape "$reason")" ;;
		xpass) printf 'ok %d - %s # TODO unexpectedly passed: %s\n' "$TAP_INDEX" "$id" "$(tap_escape "$reason")" ;;
		fail) printf 'not ok %d - %s\n' "$TAP_INDEX" "$id" ;;
		esac
		if [[ $status == fail && -n $log ]]; then
			printf '# log: %s\n' "$log"
		fi
		return
	fi

	local colour=$PM_C_GREEN label=PASS
	case $status in
	skip)
		colour=$PM_C_YELLOW
		label=SKIP
		;;
	xfail)
		colour=$PM_C_YELLOW
		label=XFAIL
		;;
	xpass)
		colour=$PM_C_BLUE
		label=XPASS
		;;
	fail)
		colour=$PM_C_RED
		label=FAIL
		;;
	esac
	printf '%b[%s]%b %s' "$colour" "$label" "$PM_C_RESET" "$id"
	[[ -n $reason ]] && printf ' (%s)' "$reason"
	printf '\n'
	if [[ $status == fail && -n $log ]]; then
		printf '       log: %s\n' "$log"
	fi
}

# ---------------------------------------------------------------------------
# Leftover cleanup
# ---------------------------------------------------------------------------

sweep_mounts() {
	command -v findmnt >/dev/null 2>&1 || return 0
	local mnt
	while IFS= read -r mnt; do
		[[ $mnt == "$PM_RUN_ROOT"* ]] || continue
		fusermount3 -u "$mnt" 2>/dev/null ||
			fusermount -u "$mnt" 2>/dev/null ||
			umount -l "$mnt" 2>/dev/null || true
	done < <(findmnt -rno TARGET 2>/dev/null || true)
}

on_interrupt() {
	sweep_mounts
	printf '\nInterrupted; artifacts kept in %s\n' "$PM_RUN_ROOT" >&2
	exit 130
}
trap on_interrupt INT TERM

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

build_binary

if ((!TAP)); then
	printf '%b== rclone-posixmeta test harness ==%b\n' "$PM_C_BOLD" "$PM_C_RESET"
	printf 'binary:   %s\n' "$PM_BINARY"
	printf 'run root: %s\n' "$PM_RUN_ROOT"
	if [[ -e /dev/fuse ]]; then
		printf 'fuse:     available\n\n'
	else
		printf 'fuse:     unavailable\n\n'
	fi
else
	printf 'TAP version %s\n' "$TAP_VERSION"
	printf '1..%d\n' "${#RUN_ID[@]}"
fi

for idx in "${!RUN_ID[@]}"; do
	suite_idx=${RUN_SUITE[idx]}
	suite_file=${SUITE_FILES[suite_idx]}
	suite_name=${SUITE_NAMES[suite_idx]}
	test_name=${RUN_TEST[idx]}
	id=${RUN_ID[idx]}
	suite_skip=${SUITE_SKIPS[suite_idx]}

	if [[ -n $suite_skip ]]; then
		record_result skip "$id" "$suite_skip"
		continue
	fi

	scratch="$PM_RUN_ROOT/work/$suite_name/$test_name"
	marker="$PM_RUN_ROOT/markers/$suite_name/$test_name"
	log="$PM_RUN_ROOT/logs/$suite_name/$test_name.log"
	rm -rf "$scratch" "$marker"
	mkdir -p "$scratch" "$marker" "$(dirname "$log")"

	start=$SECONDS
	if ((VERBOSE)); then
		PM_TEST_NAME=$test_name PM_TEST_SCRATCH=$scratch PM_MARKER_DIR=$marker \
			timeout --foreground --kill-after=10 "${PM_TEST_TIMEOUT}s" \
			bash "$TEST_DIR/run.sh" --child "$suite_file" "$test_name" 2>&1 |
			tee "$log"
		rc=${PIPESTATUS[0]}
	else
		PM_TEST_NAME=$test_name PM_TEST_SCRATCH=$scratch PM_MARKER_DIR=$marker \
			timeout --foreground --kill-after=10 "${PM_TEST_TIMEOUT}s" \
			bash "$TEST_DIR/run.sh" --child "$suite_file" "$test_name" >"$log" 2>&1
		rc=$?
	fi
	elapsed=$((SECONDS - start))

	sweep_mounts

	status=fail
	reason=""
	if [[ -f $marker/skip ]]; then
		status=skip
		reason=$(cat "$marker/skip")
	elif [[ -f $marker/xfail ]]; then
		reason=$(cat "$marker/xfail")
		if ((rc == 0)); then
			status=xpass
		else
			status=xfail
		fi
	elif ((rc == 0)); then
		status=pass
	elif ((rc == 124 || rc == 137)); then
		reason="timed out after ${PM_TEST_TIMEOUT}s"
	else
		reason="exited rc=$rc"
	fi

	record_result "$status" "$id" "$reason" "$log"

	case $status in
	pass)
		((KEEP)) || rm -rf "$scratch"
		;;
	skip)
		rm -rf "$scratch"
		;;
	fail)
		printf '       scratch: %s\n' "$scratch"
		printf '       --- last 80 log lines ---\n'
		tail -n 80 "$log" 2>/dev/null | sed 's/^/       /' >&2
		;;
	esac

	if ((!TAP)) && [[ $status != skip ]]; then
		printf '       (%ss)\n' "$elapsed"
	fi
done

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

if ((TAP)); then
	printf '# pass=%d fail=%d skip=%d xfail=%d xpass=%d\n' \
		"${COUNTS[pass]}" "${COUNTS[fail]}" "${COUNTS[skip]}" \
		"${COUNTS[xfail]}" "${COUNTS[xpass]}"
else
	printf '\n%bSummary%b: %s%d passed%s, %s%d failed%s, %d skipped, %d xfail, %d xpass\n' \
		"$PM_C_BOLD" "$PM_C_RESET" \
		"$PM_C_GREEN" "${COUNTS[pass]}" "$PM_C_RESET" \
		"$PM_C_RED" "${COUNTS[fail]}" "$PM_C_RESET" \
		"${COUNTS[skip]}" "${COUNTS[xfail]}" "${COUNTS[xpass]}"
fi

failed=0
((COUNTS[fail] > 0)) && failed=1
if ((STRICT_XPASS && COUNTS[xpass] > 0)); then
	printf 'strict-xpass: %d unexpected pass(es)\n' "${COUNTS[xpass]}" >&2
	failed=1
fi

if ((failed)); then
	printf 'Artifacts kept in %s\n' "$PM_RUN_ROOT" >&2
	exit 1
fi

if ((!KEEP)); then
	rm -rf "$PM_RUN_ROOT"
fi
exit 0

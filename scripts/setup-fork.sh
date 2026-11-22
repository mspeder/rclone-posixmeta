#!/usr/bin/env bash
# shellcheck shell=bash
#
# setup-fork.sh — materialise third_party/rclone from the pinned upstream
# module and apply third_party/rclone-posixmeta.patch.
#
# The fork itself is not versioned (it is a 49 MB copy of a pristine rclone
# release); the patch is the source of truth. Run this script (or `make fork`)
# after cloning, and after bumping the rclone version in go.mod.
#
# Usage:
#   scripts/setup-fork.sh            # no-op when the fork is already in place
#   scripts/setup-fork.sh --force    # discard and rebuild the fork

set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
DEST="$REPO_ROOT/third_party/rclone"
PATCH="$REPO_ROOT/third_party/rclone-posixmeta.patch"
STAMP="$DEST/.posixmeta-fork"

FORCE=0
case ${1:-} in
--force) FORCE=1 ;;
"") ;;
-h | --help)
	sed -n '2,14p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit 0
	;;
*)
	echo "usage: $0 [--force]" >&2
	exit 2
	;;
esac

if ((FORCE == 0)) && [[ -f $STAMP ]]; then
	echo "fork already present: $DEST"
	exit 0
fi

[[ -f $PATCH ]] || {
	echo "missing patch: $PATCH" >&2
	exit 1
}
command -v patch >/dev/null 2>&1 || {
	echo "the 'patch' utility is required to apply $PATCH" >&2
	exit 1
}

# Version is pinned by the single-line require in go.mod.
VERSION=$(sed -nE 's#^require[[:space:]]+github\.com/rclone/rclone[[:space:]]+(v[^[:space:]]+).*#\1#p' "$REPO_ROOT/go.mod")
if [[ -z $VERSION ]]; then
	echo "could not derive the rclone version from $REPO_ROOT/go.mod" >&2
	exit 1
fi

echo "fetching github.com/rclone/rclone@$VERSION"
INFO=$(cd "$REPO_ROOT" && go mod download -json "github.com/rclone/rclone@$VERSION")
SRC=$(printf '%s\n' "$INFO" | sed -nE 's/^[[:space:]]*"Dir": "([^"]+)".*/\1/p')
if [[ -z $SRC || ! -d $SRC ]]; then
	echo "could not locate the downloaded module source for github.com/rclone/rclone@$VERSION" >&2
	exit 1
fi

echo "copying $SRC -> $DEST"
rm -rf "$DEST"
mkdir -p "$REPO_ROOT/third_party"
cp -a "$SRC" "$DEST"
chmod -R u+w "$DEST"
# Upstream agent instructions are not part of this repository.
rm -f "$DEST/AGENTS.md" "$DEST/CLAUDE.md"

echo "applying $(basename "$PATCH")"
patch -p1 -d "$DEST" <"$PATCH"

DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
{
	echo "materialised from github.com/rclone/rclone@$VERSION"
	echo "patch: $(basename "$PATCH")"
	echo "generated: $DATE"
} >"$STAMP"

echo "fork ready: $DEST (rclone $VERSION)"

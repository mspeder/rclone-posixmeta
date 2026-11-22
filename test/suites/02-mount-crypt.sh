#!/usr/bin/env bash
# shellcheck shell=bash
#
# Suite 02: end-to-end FUSE mount over crypt(local) -> posixmeta.
#
# Mirrors AGENTS.md Procedure B. Each test mounts pm: (posixmeta over crypt)
# in the background, drives it through the kernel, verifies metadata via
# `lsjson --metadata` against the backend, and verifies that the raw storage
# contains no plaintext names or sidecar suffixes.

suite_require_cmd jq
suite_require_cmd mountpoint
suite_require_fuse

_mount_setup() {
	pm_write_crypt_config "$PM_CONFIG" "$PM_SRC"
}

_mount_start() {
	pm_mount_start pm: "$PM_MNT" \
		--metadata-push \
		--vfs-cache-mode writes \
		--vfs-write-back 200ms \
		--dir-cache-time 500ms
}

test_mount_roundtrip_metadata_inheritance_and_encryption() {
	_mount_setup
	_mount_start

	mkdir -p "$PM_MNT/secret_dir"
	printf 'encrypted payload\n' >"$PM_MNT/secret_dir/notes.txt"
	pm_wait_entry pm:secret_dir notes.txt

	pm backend setmeta pm: secret_dir -o uid=1050 -o gid=1060 >/dev/null
	pm backend setmeta pm: secret_dir/notes.txt \
		-o mode=0600 -o btime=2026-11-22T08:00:00Z >/dev/null

	# The mount must expose plaintext data and hide the sidecar.
	pm_assert_file "$PM_MNT/secret_dir/notes.txt"
	pm_assert_no_file "$PM_MNT/secret_dir/notes.txt.posixmeta"
	pm_assert_eq "encrypted payload" "$(cat "$PM_MNT/secret_dir/notes.txt")" "mount payload"
	pm_assert_mount_has_no_sidecars "$PM_MNT"

	# Metadata resolves through the backend, including parent-owner inheritance.
	pm_assert_eq "0600" "$(pm_meta_of pm:secret_dir notes.txt mode)" "file mode"
	pm_assert_eq "1050" "$(pm_meta_of pm:secret_dir notes.txt uid)" "inherited uid"
	pm_assert_eq "1060" "$(pm_meta_of pm:secret_dir notes.txt gid)" "inherited gid"
	pm_assert_eq "2026-11-22T08:00:00Z" "$(pm_meta_of pm:secret_dir notes.txt btime)" "btime"
	pm_assert_eq "0755" "$(pm_meta_of pm: secret_dir mode)" "directory default mode"

	# Raw storage must be encrypted: no plaintext names, no sidecar suffix.
	pm_assert_raw_has_no_plaintext notes
	[[ -n $(ls -A "$PM_SRC") ]] || pm_fail "raw storage is empty; encrypted objects were not flushed"

	pm_mount_stop "$PM_MNT"
}

test_mount_rename_and_delete_lifecycle() {
	_mount_setup
	_mount_start

	mkdir -p "$PM_MNT/d"
	printf 'payload\n' >"$PM_MNT/d/file.txt"
	pm_wait_entry pm:d file.txt

	pm backend setmeta pm: d -o uid=1050 -o gid=1060 >/dev/null
	pm backend setmeta pm: d/file.txt -o mode=0600 -o btime=2026-11-22T08:00:00Z >/dev/null

	mv "$PM_MNT/d/file.txt" "$PM_MNT/d/renamed.txt"
	pm_wait_entry pm:d renamed.txt
	pm_wait_entry_gone pm:d file.txt
	pm_assert_eq "0600" "$(pm_meta_of pm:d renamed.txt mode)" "mode survives rename"
	pm_assert_eq "1050" "$(pm_meta_of pm:d renamed.txt uid)" "owner survives rename"
	pm_assert_eq "2026-11-22T08:00:00Z" "$(pm_meta_of pm:d renamed.txt btime)" "btime survives rename"

	rm "$PM_MNT/d/renamed.txt"
	pm_wait_empty pm:d
	pm_assert_no_file "$PM_MNT/d/renamed.txt"
	pm_assert_mount_has_no_sidecars "$PM_MNT"
	pm_assert_raw_has_no_plaintext renamed

	pm_mount_stop "$PM_MNT"
}

# Mounting posixmeta without --metadata-push must fail before the FUSE mount
# starts: otherwise stat would show mount defaults and chmod/chown would be
# silently dropped.
test_mount_requires_metadata_push() {
	_mount_setup

	local log="$PM_TEST_SCRATCH/require-metadata-push.log" rc=0
	timeout 30 "$PM_BINARY" --config "$PM_CONFIG" --cache-dir "$PM_VFS_CACHE" \
		mount pm: "$PM_MNT" --vfs-cache-mode writes >"$log" 2>&1 || rc=$?
	if ((rc == 0)); then
		pm_fail "mount without --metadata-push unexpectedly succeeded"
	fi
	if ((rc == 124)); then
		pm_fail "mount without --metadata-push hung instead of failing fast"
	fi
	pm_assert_contains "$(cat "$log")" "--metadata-push" \
		"error message points at --metadata-push"
	if mountpoint -q "$PM_MNT"; then
		pm_fail "rejected mount left a stale FUSE mount at $PM_MNT"
	fi
}

# Without --metadata-push the mount must behave exactly like stock rclone:
# attrs come from --file-perms and chmod must not touch the backing file.
test_mount_without_metadata_push_keeps_stock_behaviour() {
	pm_write_local_config "$PM_CONFIG" "$PM_SRC"
	printf 'local payload\n' >"$PM_SRC/local.txt"
	chmod 0600 "$PM_SRC/local.txt"

	pm_mount_start "raw:$PM_SRC" "$PM_MNT" \
		--file-perms 0640 \
		--dir-perms 0750 \
		--vfs-cache-mode writes \
		--vfs-write-back 200ms \
		--dir-cache-time 500ms

	pm_assert_eq "640" "$(stat -c '%a' "$PM_MNT/local.txt")" \
		"attrs come from --file-perms without the flag"

	chmod 0700 "$PM_MNT/local.txt"
	pm_assert_eq "600" "$(stat -c '%a' "$PM_SRC/local.txt")" \
		"chmod must not reach the backing file without the flag"

	pm_mount_stop "$PM_MNT"
}

# --metadata-push opts in to per-entry attrs and chmod/chown write-through,
# including for backends other than posixmeta.
test_mount_metadata_push_is_opt_in() {
	pm_write_local_config "$PM_CONFIG" "$PM_SRC"
	printf 'local payload\n' >"$PM_SRC/optin.txt"
	chmod 0600 "$PM_SRC/optin.txt"

	pm_mount_start "raw:$PM_SRC" "$PM_MNT" \
		--metadata-push \
		--file-perms 0640 \
		--dir-perms 0750 \
		--vfs-cache-mode writes \
		--vfs-write-back 200ms \
		--dir-cache-time 500ms

	pm_assert_eq "600" "$(stat -c '%a' "$PM_MNT/optin.txt")" \
		"backend mode overrides --file-perms when opted in"

	chmod 0700 "$PM_MNT/optin.txt"
	pm_assert_eq "700" "$(stat -c '%a' "$PM_SRC/optin.txt")" \
		"chmod reaches the backing file when opted in"
	pm_assert_eq "700" "$(stat -c '%a' "$PM_MNT/optin.txt")" \
		"getattr reports the pushed mode"

	pm_mount_stop "$PM_MNT"
}

# FUSE chmod/chown are forwarded through the VFS to fs.SetMetadataer and
# getattr/stat report the per-entry POSIX metadata instead of the mount-wide
# --file-perms/--dir-perms/--uid/--gid defaults.
test_mount_chmod_via_fuse_is_forwarded() {
	_mount_setup
	_mount_start

	mkdir -p "$PM_MNT/dir"
	printf 'payload\n' >"$PM_MNT/chmod.txt"
	printf 'payload\n' >"$PM_MNT/dir/nested.txt"
	pm_wait_entry pm: chmod.txt
	pm_wait_entry pm:dir nested.txt

	chmod 0700 "$PM_MNT/chmod.txt"
	pm_wait_meta pm: chmod.txt mode 0700 3
	pm_assert_eq "700" "$(stat -c '%a' "$PM_MNT/chmod.txt")" "stat reports chmod mode"

	# Directories made inside the mount start with a synthetic VFS entry;
	# chmod must refresh it from the backend before writing the sidecar.
	chmod 0710 "$PM_MNT/dir"
	pm_wait_meta pm: dir mode 0710 3
	pm_assert_eq "710" "$(stat -c '%a' "$PM_MNT/dir")" "stat reports directory chmod mode"

	pm_assert_mount_has_no_sidecars "$PM_MNT"
	pm_mount_stop "$PM_MNT"
}

# Chown only needs CAP_CHOWN when changing the owner uid, or the group to one
# the caller isn't a member of, so as an unprivileged user we chgrp to a
# supplementary group and skip otherwise.
test_mount_chown_via_fuse_is_forwarded() {
	local gid
	gid=$(id -G | tr ' ' '\n' | awk -v cur="$(id -g)" '$1 != cur { print; exit }')
	if [[ $(id -u) -ne 0 && -z $gid ]]; then
		skip "chown needs CAP_CHOWN or a supplementary group"
	fi
	[[ -n $gid ]] || gid=5678

	_mount_setup
	_mount_start

	mkdir -p "$PM_MNT/dir"
	printf 'payload\n' >"$PM_MNT/chown.txt"
	pm_wait_entry pm: chown.txt
	pm_wait_entry pm: dir

	chgrp "$gid" "$PM_MNT/chown.txt"
	pm_wait_meta pm: chown.txt gid "$gid" 3
	pm_assert_eq "$gid" "$(stat -c '%g' "$PM_MNT/chown.txt")" "stat reports file chown gid"

	chgrp "$gid" "$PM_MNT/dir"
	pm_wait_meta pm: dir gid "$gid" 3
	pm_assert_eq "$gid" "$(stat -c '%g' "$PM_MNT/dir")" "stat reports directory chown gid"

	pm_mount_stop "$PM_MNT"
}

# getattr must expose metadata which was stored before the mount started
# (defaults would be 0644/0755 and uid/gid 1000).
test_mount_getattr_reflects_sidecar_metadata() {
	_mount_setup

	pm mkdir pm:proj
	printf 'payload\n' >"$PM_IN/pre.txt"
	pm copyto "$PM_IN/pre.txt" pm:proj/pre.txt
	pm backend setmeta pm: proj -o mode=0750 -o uid=1051 -o gid=1052 >/dev/null
	pm backend setmeta pm: proj/pre.txt -o mode=0600 >/dev/null

	_mount_start

	pm_assert_eq "750" "$(stat -c '%a' "$PM_MNT/proj")" "directory mode from sidecar"
	pm_assert_eq "1051" "$(stat -c '%u' "$PM_MNT/proj")" "directory uid from sidecar"
	pm_assert_eq "1052" "$(stat -c '%g' "$PM_MNT/proj")" "directory gid from sidecar"

	pm_assert_eq "600" "$(stat -c '%a' "$PM_MNT/proj/pre.txt")" "file mode from sidecar"
	pm_assert_eq "1051" "$(stat -c '%u' "$PM_MNT/proj/pre.txt")" "inherited uid in getattr"
	pm_assert_eq "1052" "$(stat -c '%g' "$PM_MNT/proj/pre.txt")" "inherited gid in getattr"

	pm_mount_stop "$PM_MNT"
}

# Known gap from AGENTS.md roadmap item 2: symlinks are not wired through the
# VFS mount even though the backend persists the "symlink" sidecar attribute.
test_mount_symlink_via_fuse_is_forwarded() {
	xfail "roadmap: rclone VFS does not implement symlink creation/readlink"
	_mount_setup
	_mount_start

	ln -s /etc/hostname "$PM_MNT/link"
	pm_wait_meta pm: link symlink /etc/hostname 3

	pm_mount_stop "$PM_MNT"
}

# Subpath CLI operations over crypt: addressing pm:<dir> re-roots the wrapped
# crypt remote, so the directory's encrypted sidecar lives outside that root and
# must still be moved/removed by the backend.
test_crypt_subpath_dir_move_preserves_metadata() {
	_mount_setup
	pm mkdir pm:dir
	pm backend setmeta pm: dir -o uid=1050 -o gid=1060 >/dev/null

	pm moveto pm:dir pm:dir2

	pm_assert_eq "1" "$(pm lsjson pm: | jq 'length')" "exactly one entry after move"
	pm_assert_eq "1050" \
		"$(pm lsjson --metadata pm: | jq -r '.[] | select(.Name == "dir2") | .Metadata.uid')" \
		"directory uid preserved across subpath move"
	pm_assert_eq "1060" \
		"$(pm lsjson --metadata pm: | jq -r '.[] | select(.Name == "dir2") | .Metadata.gid')" \
		"directory gid preserved across subpath move"
}

test_crypt_subpath_rmdir_removes_encrypted_sidecar() {
	_mount_setup
	pm mkdir pm:secret_dir
	pm backend setmeta pm: secret_dir -o mode=0700 >/dev/null
	pm_assert_eq "1" "$(pm lsjson pm: | jq 'length')" "directory created"

	pm rmdir pm:secret_dir

	pm_assert_eq "0" "$(pm lsjson pm: | jq 'length')" "directory and its encrypted sidecar removed"
}

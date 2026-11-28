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
		--vfs-links \
		--vfs-cache-mode writes \
		--vfs-write-back 200ms \
		--dir-cache-time 500ms \
		"$@"
}

test_mount_roundtrip_metadata_inheritance_and_encryption() {
	_mount_setup

	# Directory owned by another user, plus an entry copied outside the
	# mount. The chown happens after the writes below so the kernel
	# permission check (--metadata-permissions) still lets the mounting
	# user create the file it owns.
	pm mkdir pm:secret_dir
	printf 'legacy payload\n' >"$PM_IN/legacy.txt"
	pm copyto "$PM_IN/legacy.txt" pm:secret_dir/legacy.txt

	_mount_start

	printf 'encrypted payload\n' >"$PM_MNT/secret_dir/notes.txt"
	pm_wait_entry pm:secret_dir notes.txt
	# The ownership recorded at create time is written back asynchronously;
	# wait for it before the CLI rewrites the same sidecar below.
	pm_wait_meta pm:secret_dir notes.txt uid "$(id -u)" 10

	pm backend setmeta pm: secret_dir -o uid=1050 -o gid=1060 >/dev/null

	# The directory chown above happened after the write, so the sparse
	# creator sidecar was skipped (uid/gid equalled the inherited values at
	# that time). Record the creator explicitly to keep asserting it.
	pm backend setmeta pm: secret_dir/notes.txt \
		-o uid="$(id -u)" -o gid="$(id -g)" \
		-o mode=0600 -o btime=2026-11-22T08:00:00Z >/dev/null

	# The mount must expose plaintext data and hide the sidecar.
	pm_assert_file "$PM_MNT/secret_dir/notes.txt"
	pm_assert_no_file "$PM_MNT/secret_dir/.posixmeta.notes.txt"
	pm_assert_eq "encrypted payload" "$(cat "$PM_MNT/secret_dir/notes.txt")" "mount payload"
	pm_assert_mount_has_no_sidecars "$PM_MNT"

	# Metadata resolves through the backend. The file created through the
	# mount belongs to its creating process (this user); the copied one has
	# no creator and inherits the directory owner.
	pm_assert_eq "0600" "$(pm_meta_of pm:secret_dir notes.txt mode)" "file mode"
	pm_assert_eq "$(id -u)" "$(pm_meta_of pm:secret_dir notes.txt uid)" "creator uid"
	pm_assert_eq "$(id -g)" "$(pm_meta_of pm:secret_dir notes.txt gid)" "creator gid"
	pm_assert_eq "2026-11-22T08:00:00Z" "$(pm_meta_of pm:secret_dir notes.txt btime)" "btime"
	pm_assert_eq "1050" "$(pm_meta_of pm:secret_dir legacy.txt uid)" "inherited uid"
	pm_assert_eq "1060" "$(pm_meta_of pm:secret_dir legacy.txt gid)" "inherited gid"
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

	# Only the file is handed to another user: the directory stays owned by
	# the mounting user so the kernel permission check still allows the
	# rename and delete below.
	pm backend setmeta pm: d/file.txt \
		-o uid=1050 -o gid=1060 -o mode=0600 -o btime=2026-11-22T08:00:00Z >/dev/null

	mv "$PM_MNT/d/file.txt" "$PM_MNT/d/renamed.txt"
	pm_wait_entry pm:d renamed.txt
	pm_wait_entry_gone pm:d file.txt
	pm_assert_eq "0600" "$(pm_meta_of pm:d renamed.txt mode)" "mode survives rename"
	pm_assert_eq "1050" "$(pm_meta_of pm:d renamed.txt uid)" "owner survives rename"
	pm_assert_eq "2026-11-22T08:00:00Z" "$(pm_meta_of pm:d renamed.txt btime)" "btime survives rename"

	# rm may think the 0600 file owned by another user is write protected and
	# ask for confirmation; unlinking only needs write access on the parent.
	rm -f "$PM_MNT/d/renamed.txt"
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

	# Stock rclone mounts are permissive: the kernel must not be asked to
	# enforce the (overlay-free) attrs without --metadata-push.
	pm_assert_not_contains \
		"$(awk -v m="$PM_MNT" '$2 == m { print $4 }' /proc/self/mounts)" \
		"default_permissions" "stock mount options omit default_permissions"

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
	pm backend setmeta pm: proj -o mode=0755 -o uid=1051 -o gid=1052 >/dev/null
	pm backend setmeta pm: proj/pre.txt -o mode=0600 >/dev/null

	_mount_start

	pm_assert_eq "755" "$(stat -c '%a' "$PM_MNT/proj")" "directory mode from sidecar"
	pm_assert_eq "1051" "$(stat -c '%u' "$PM_MNT/proj")" "directory uid from sidecar"
	pm_assert_eq "1052" "$(stat -c '%g' "$PM_MNT/proj")" "directory gid from sidecar"

	pm_assert_eq "600" "$(stat -c '%a' "$PM_MNT/proj/pre.txt")" "file mode from sidecar"
	pm_assert_eq "1051" "$(stat -c '%u' "$PM_MNT/proj/pre.txt")" "inherited uid in getattr"
	pm_assert_eq "1052" "$(stat -c '%g' "$PM_MNT/proj/pre.txt")" "inherited gid in getattr"

	pm_mount_stop "$PM_MNT"
}

# Upstream --vfs-links represents a symlink as a single <name>.rclonelink
# object whose payload is the target. posixmeta (over crypt) stores that one
# object like any other file, so no sidecar is created unless metadata is
# explicitly pushed. --metadata-push must still keep the symlink mode from
# --link-perms (plus the symlink type bit) instead of overlaying the
# posixmeta default file mode, while the creating process's ownership is
# recorded (so the directory owner is not adopted).
test_mount_symlinks_via_vfs_links() {
	local duid=1050 dgid=1060
	[[ $(id -u) == $duid ]] && duid=1051
	[[ $(id -g) == $dgid ]] && dgid=1061

	_mount_setup

	# Create the parent and its sidecar before mounting so uid/gid
	# inheritance is read fresh by the mount process. The directory is
	# world-writable so the kernel permission check still lets the mounting
	# user create the links inside a directory owned by somebody else.
	pm mkdir pm:dir
	printf 'payload\n' >"$PM_IN/target.txt"
	pm copyto "$PM_IN/target.txt" pm:dir/target.txt
	pm backend setmeta pm: dir -o uid=$duid -o gid=$dgid -o mode=0777 >/dev/null

	_mount_start --umask 0000 --link-perms 0777

	raw_before=$(find "$PM_SRC" -type f | wc -l | tr -d ' ')

	ln -s target.txt "$PM_MNT/dir/rel-link"
	ln -s /etc/hostname "$PM_MNT/dir/abs-link"
	ln -s missing "$PM_MNT/dir/dangling"

	pm_wait_entry pm:dir rel-link.rclonelink
	pm_wait_entry pm:dir abs-link.rclonelink
	pm_wait_entry pm:dir dangling.rclonelink

	# The VFS publishes the object asynchronously after the writeback;
	# wait until getattr resolves the recorded creator. The sidecar exists
	# once lsjson reports it, so the raw-object count below is stable.
	link_owner_resolved() { [[ $(stat -c '%u' "$1" 2>/dev/null) == "$(id -u)" ]]; }
	pm_wait_for 10 link_owner_resolved "$PM_MNT/dir/rel-link"
	pm_wait_meta pm:dir abs-link.rclonelink uid "$(id -u)" 10
	pm_wait_meta pm:dir dangling.rclonelink uid "$(id -u)" 10

	# Mount view: symlink type and mode from --link-perms, not the 0644
	# default stored/resolved for ordinary files.
	pm_assert_eq "symbolic link" "$(stat -c '%F' "$PM_MNT/dir/rel-link")" "stat reports a symlink"
	pm_assert_eq "777" "$(stat -c '%a' "$PM_MNT/dir/rel-link")" "symlink mode from --link-perms"
	pm_assert_eq "$(id -u)" "$(stat -c '%u' "$PM_MNT/dir/rel-link")" "symlink belongs to its creator"
	pm_assert_eq "$(id -g)" "$(stat -c '%g' "$PM_MNT/dir/rel-link")" "symlink keeps the creator group"
	pm_assert_eq "target.txt" "$(readlink "$PM_MNT/dir/rel-link")" "relative readlink"
	pm_assert_eq "/etc/hostname" "$(readlink "$PM_MNT/dir/abs-link")" "absolute readlink"
	pm_assert_eq "missing" "$(readlink "$PM_MNT/dir/dangling")" "dangling readlink"
	pm_assert_eq "payload" "$(cat "$PM_MNT/dir/rel-link")" "link resolves to its target"
	pm_assert_no_file "$PM_MNT/dir/rel-link.rclonelink"
	pm_assert_mount_has_no_sidecars "$PM_MNT"

	# Backend view: one plain object per link, target as payload, plus the
	# ownership sidecar recorded when the creator differs from the inherited
	# directory owner. lsjson reports the ordinary file default 0644: the
	# 0777 above is presentation-only.
	pm_assert_eq "target.txt" "$(pm cat pm:dir/rel-link.rclonelink)" "link payload on the backend"
	pm_assert_eq "0644" "$(pm_meta_of pm:dir rel-link.rclonelink mode)" "backend default mode without a mode override"
	pm_assert_eq "$(id -u)" "$(pm_meta_of pm:dir rel-link.rclonelink uid)" "backend creator uid"
	pm_assert_eq "$(id -g)" "$(pm_meta_of pm:dir rel-link.rclonelink gid)" "backend creator gid"
	# lsjson hides sidecars, so count the encrypted objects on raw storage:
	# exactly one new object and one ownership sidecar per link.
	pm_assert_eq "$((raw_before + 6))" \
		"$(find "$PM_SRC" -type f | wc -l | tr -d ' ')" \
		"one raw object and one sidecar per symlink"
	pm_assert_raw_has_no_plaintext rel-link

	mv "$PM_MNT/dir/rel-link" "$PM_MNT/dir/renamed-link"
	pm_wait_entry pm:dir renamed-link.rclonelink
	pm_wait_entry_gone pm:dir rel-link.rclonelink
	pm_assert_eq "target.txt" "$(readlink "$PM_MNT/dir/renamed-link")" "readlink survives rename"

	rm "$PM_MNT/dir/abs-link" "$PM_MNT/dir/dangling"
	pm_wait_entry_gone pm:dir abs-link.rclonelink
	pm_wait_entry_gone pm:dir dangling.rclonelink

	# Exactly one object for the remaining link plus the regular target, and
	# the remaining link's ownership sidecar.
	pm_assert_eq "2" "$(pm lsjson pm:dir | jq 'length')" "one backend object per symlink"
	pm_assert_eq "$((raw_before + 2))" \
		"$(find "$PM_SRC" -type f | wc -l | tr -d ' ')" \
		"deleted symlinks left no object or sidecar behind"

	# The suffix translation must also work after a remount (listing path).
	pm_mount_stop "$PM_MNT"
	_mount_start
	pm_assert_eq "target.txt" "$(readlink "$PM_MNT/dir/renamed-link")" "readlink survives remount"

	pm_mount_stop "$PM_MNT"
}

# Regression for the upstream bug fixed in patch 0001 (docs/adr/0007): a
# symlink created through the mount used to stay invisible to ls until
# --dir-cache-time expired *again*. cmd/mount.Dir.Symlink used to go through
# VFS.CreateSymlink, which re-resolves the parent with VFS.StatParent and can
# therefore return a different *vfs.Dir instance after a directory cache
# forget; the new virtual entry landed on that instance while the kernel kept
# reading the node it already had.
#
# vfs/forget puts the mount in exactly that state deterministically, without
# waiting for a cache expiry.
test_mount_symlink_listed_immediately() {
	_mount_setup
	local sock="$PM_TEST_SCRATCH/mount-rc.sock"
	_mount_start --rc --rc-no-auth --rc-addr "unix://$sock"
	pm_wait_for 10 test -S "$sock"

	mkdir -p "$PM_MNT/dir"
	printf 'payload\n' >"$PM_MNT/dir/target.txt"
	pm_wait_entry pm:dir target.txt

	# Populate the directory cache, then forget it: the backend object stays
	# and the kernel keeps its FUSE node for the directory, but ForgetAll
	# drops the cached children of a directory without virtual entries, so
	# the next lookup has to build a new *vfs.Dir for it.
	ls -A "$PM_MNT/dir" >/dev/null
	"$PM_BINARY" --config "$PM_CONFIG" rc --unix-socket "$sock" \
		vfs/forget >/dev/null

	ln -s target.txt "$PM_MNT/dir/link"

	# Deliberately no polling here: retrying would simply wait out the bug.
	pm_assert_contains "$(ls -A "$PM_MNT/dir" | tr '\n' ' ')" "link" \
		"a link created after a cache forget is listed immediately"

	# The link must still reach the backend like any other created entry.
	pm_wait_entry pm:dir link.rclonelink
	pm_assert_eq "target.txt" "$(pm cat pm:dir/link.rclonelink)" "link payload on the backend"
	pm_assert_eq "target.txt" "$(readlink "$PM_MNT/dir/link")" \
		"readlink works after a cache forget"
	pm_assert_mount_has_no_sidecars "$PM_MNT"

	rm -f "$PM_MNT/dir/link"
	pm_wait_entry_gone pm:dir link.rclonelink

	pm_mount_stop "$PM_MNT"
}

# Same regression without --rc: every iteration uses a fresh directory so the
# directory cache has to expire between the initial listing and the ln -s,
# which is what produced the extra *vfs.Dir instance in the field.
# --attr-timeout keeps the kernel from re-looking-up the directory (a lookup
# would hand it the new VFS node and mask the bug).
test_mount_symlink_listed_after_dir_cache_expiry() {
	_mount_setup
	pm_mount_start pm: "$PM_MNT" \
		--metadata-push \
		--vfs-links \
		--vfs-cache-mode writes \
		--vfs-write-back 200ms \
		--dir-cache-time 200ms \
		--attr-timeout 30s

	local i
	for i in {1..10}; do
		mkdir -p "$PM_MNT/d$i"
		ls -A "$PM_MNT/d$i" >/dev/null
		# Let --dir-cache-time expire before creating the link.
		sleep 0.4
		ln -s target "$PM_MNT/d$i/link"
		pm_assert_contains "$(ls -A "$PM_MNT/d$i" | tr '\n' ' ')" "link" \
			"link in d$i is listed immediately after the cache expired"
		rm -f "$PM_MNT/d$i/link"
	done

	pm_mount_stop "$PM_MNT"
}


# creation fails loudly instead of silently misbehaving, no .rclonelink object
# or sidecar is created, and ordinary files keep working.
test_mount_without_vfs_links_rejects_symlink() {
	# Keep the creator stamp sparse (the creating process owns the file and
	# the defaults are its own ids) so the raw object count stays exact.
	PM_CFG_OPTS[default_uid]=$(id -u)
	PM_CFG_OPTS[default_gid]=$(id -g)
	_mount_setup
	pm_mount_start pm: "$PM_MNT" \
		--metadata-push \
		--vfs-cache-mode writes \
		--vfs-write-back 200ms \
		--dir-cache-time 500ms

	printf 'payload\n' >"$PM_MNT/plain.txt"
	pm_wait_entry pm: plain.txt

	if ln -s target "$PM_MNT/link" 2>/dev/null; then
		pm_fail "ln -s unexpectedly succeeded without --vfs-links"
	fi
	if readlink "$PM_MNT/link" >/dev/null 2>&1; then
		pm_fail "readlink unexpectedly succeeded without --vfs-links"
	fi
	pm_assert_eq "1" "$(pm lsjson pm: | jq 'length')" "failed symlink leaves no object behind"
	pm_assert_eq "1" \
		"$(find "$PM_SRC" -type f | wc -l | tr -d ' ')" \
		"failed symlink leaves no raw object or sidecar"
	pm_assert_eq "payload" "$(cat "$PM_MNT/plain.txt")" "regular I/O still works"

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

# A chmod immediately followed by an unmount must still reach the remote: the
# debounced writeback queue is flushed by fs.Shutdowner when the mount's fs
# cache entry is torn down. A very long writeback_delay proves that only the
# shutdown path can have persisted it.
test_mount_chmod_survives_immediate_unmount() {
	PM_CFG_OPTS[writeback_delay]=1h
	_mount_setup
	_mount_start

	printf 'payload\n' >"$PM_MNT/persist.txt"
	pm_wait_entry pm: persist.txt
	chmod 0700 "$PM_MNT/persist.txt"
	# No wait: unmount as soon as chmod returns.
	pm_mount_stop "$PM_MNT"

	pm_assert_eq "0700" "$(pm_meta_of pm: persist.txt mode)" \
		"debounced chmod flushed on unmount"
}

# Rewriting a file through the mount must not lose its metadata: the VFS
# re-uploads the payload without repeating mode/uid/gid, so overwrite_metadata
# defaults to preserve.
test_mount_rewrite_preserves_metadata() {
	_mount_setup
	_mount_start

	printf 'first payload\n' >"$PM_MNT/over.txt"
	pm_wait_entry pm: over.txt
	chmod 0640 "$PM_MNT/over.txt"
	pm_wait_meta pm: over.txt mode 0640 5

	printf 'second payload\n' >"$PM_MNT/over.txt"
	pm_wait_for 10 grep -q 'second payload' "$PM_MNT/over.txt"

	pm_assert_eq "640" "$(stat -c '%a' "$PM_MNT/over.txt")" \
		"getattr keeps mode after rewrite"
	pm_assert_eq "0640" "$(pm_meta_of pm: over.txt mode)" \
		"metadata survives rewrite through the mount"
	pm_assert_mount_has_no_sidecars "$PM_MNT"

	pm_mount_stop "$PM_MNT"
}

# Entries created through the mount must belong to the creating process, like
# on a regular filesystem, instead of adopting the parent directory owner that
# inherit_parent_owner applies to entries without recorded credentials.
test_mount_create_records_creating_process() {
	# Directory owner chosen so it can never equal the creating process.
	local duid=1050 dgid=1060
	[[ $(id -u) == $duid ]] && duid=1051
	[[ $(id -g) == $dgid ]] && dgid=1061

	_mount_setup

	# Parent directory owned by "another user" before the mount starts, so
	# the mount resolves (and caches) the inherited owner. Its 0777 mode
	# keeps it writable for the mounting user under --metadata-permissions.
	pm mkdir pm:test
	printf 'legacy payload\n' >"$PM_IN/legacy.txt"
	pm copyto "$PM_IN/legacy.txt" pm:test/legacy.txt
	pm backend setmeta pm: test -o uid=$duid -o gid=$dgid -o mode=0777 >/dev/null

	_mount_start

	pm_assert_eq "$duid" "$(stat -c '%u' "$PM_MNT/test")" "directory owner from sidecar"

	# A file created inside that directory belongs to its creator.
	printf 'mine\n' >"$PM_MNT/test/mine.txt"
	pm_wait_entry pm:test mine.txt
	pm_wait_meta pm:test mine.txt uid "$(id -u)" 10
	pm_assert_eq "$(id -u)" "$(stat -c '%u' "$PM_MNT/test/mine.txt")" "getattr shows the creator uid"
	pm_assert_eq "$(id -g)" "$(stat -c '%g' "$PM_MNT/test/mine.txt")" "getattr shows the creator gid"
	pm_assert_eq "$(id -u)" "$(pm_meta_of pm:test mine.txt uid)" "backend stores the creator uid"
	pm_assert_eq "$(id -g)" "$(pm_meta_of pm:test mine.txt gid)" "backend stores the creator gid"

	# Directories created there do the same.
	mkdir "$PM_MNT/test/child"
	pm_wait_entry pm:test child
	pm_wait_meta pm:test child uid "$(id -u)" 10
	pm_assert_eq "$(id -u)" "$(stat -c '%u' "$PM_MNT/test/child")" "new directory belongs to the creator"

	# Entries copied outside the mount carry no creator and keep inheriting.
	pm_assert_eq "$duid" "$(pm_meta_of pm:test legacy.txt uid)" "copied entry still inherits"
	pm_assert_eq "$dgid" "$(pm_meta_of pm:test legacy.txt gid)" "copied entry still inherits the group"

	pm_mount_stop "$PM_MNT"
}

# A setgid directory forces its group on new entries (POSIX semantics) while
# their owner stays the creating process.
test_mount_create_in_setgid_dir_inherits_group() {
	local gid
	gid=$(id -G | tr ' ' '\n' | awk -v cur="$(id -g)" '$1 != cur { print; exit }')
	if [[ $(id -u) -ne 0 && -z $gid ]]; then
		skip "setgid group inheritance needs a supplementary group"
	fi
	[[ -n $gid ]] || gid=5678

	_mount_setup
	_mount_start

	mkdir -p "$PM_MNT/sg"
	chgrp "$gid" "$PM_MNT/sg"
	chmod g+s "$PM_MNT/sg"
	pm_wait_meta pm: sg mode 02755 5

	printf 'payload\n' >"$PM_MNT/sg/file.txt"
	pm_wait_entry pm:sg file.txt
	pm_wait_meta pm:sg file.txt gid "$gid" 10

	pm_assert_eq "$(id -u)" "$(stat -c '%u' "$PM_MNT/sg/file.txt")" "setgid keeps the creator uid"
	pm_assert_eq "$gid" "$(stat -c '%g' "$PM_MNT/sg/file.txt")" "setgid forces the directory group"
	# Regular files inherit the group, never the bit itself.
	pm_assert_eq "644" "$(stat -c '%a' "$PM_MNT/sg/file.txt")" "regular files never inherit setgid"

	# The bit itself is inherited by new subdirectories (S_ISDIR only), with
	# the permission bits the kernel asked for: `mkdir` under 2775 with umask
	# 022 yields 2755, not 2775 (the kernel copies S_ISGID, never the modes).
	mkdir "$PM_MNT/sg/sub"
	pm_wait_meta pm:sg sub mode 02755 10
	pm_assert_eq "2755" "$(stat -c '%a' "$PM_MNT/sg/sub")" "setgid propagated to new subdirectory"
	pm_assert_eq "$gid" "$(stat -c '%g' "$PM_MNT/sg/sub")" "new subdirectory inherits the group"
	pm_assert_eq "$(id -u)" "$(stat -c '%u' "$PM_MNT/sg/sub")" "new subdirectory belongs to its creator"

	# Regression: without S_ISGID on the child directory the file fell back to
	# the creator's primary group instead of the setgid directory's group.
	( umask 022; printf 'nested\n' >"$PM_MNT/sg/sub/file2.txt" )
	pm_wait_entry pm:sg/sub file2.txt
	pm_wait_meta pm:sg/sub file2.txt gid "$gid" 10
	pm_assert_eq "$gid" "$(stat -c '%g' "$PM_MNT/sg/sub/file2.txt")" "setgid survives one level down"
	pm_assert_eq "$(id -u)" "$(stat -c '%u' "$PM_MNT/sg/sub/file2.txt")" "nested file belongs to its creator"
	pm_assert_eq "644" "$(stat -c '%a' "$PM_MNT/sg/sub/file2.txt")" "nested file keeps the umasked mode"

	# mkdir -p keeps the bit on every new level created under a setgid parent.
	mkdir -p "$PM_MNT/sg/deep/deeper"
	pm_wait_meta pm:sg deep mode 02755 10
	pm_wait_meta pm:sg/deep deeper mode 02755 10
	pm_assert_eq "2755" "$(stat -c '%a' "$PM_MNT/sg/deep")" "setgid propagated to mkdir -p level 1"
	pm_assert_eq "2755" "$(stat -c '%a' "$PM_MNT/sg/deep/deeper")" "setgid propagated to mkdir -p level 2"

	pm_mount_stop "$PM_MNT"
}

# New entries take their mode from the FUSE request (the kernel has already
# masked it with the caller's umask) instead of default_file_mode/
# default_dir_mode, so the posixmeta mount behaves like ext4: mkdir -m and
# umask are honoured. Modes that equal the backend defaults still stay sparse.
test_mount_create_honours_requested_mode() {
	# Match the creator stamp with the resolved defaults so this test only
	# observes mode handling in the sidecars.
	PM_CFG_OPTS[default_uid]=$(id -u)
	PM_CFG_OPTS[default_gid]=$(id -g)
	pm_write_local_config "$PM_CONFIG" "$PM_SRC"
	_mount_start

	# The kernel masks mkdir(2) with the caller's umask before sending the
	# FUSE request, so a umasked mkdir stores 0700, exactly like ext4.
	( umask 077; mkdir "$PM_MNT/private" )
	pm_wait_meta pm: private mode 0700 10
	pm_assert_eq "700" "$(stat -c '%a' "$PM_MNT/private")" "mkdir honours the caller's umask"

	( umask 077; printf 'private\n' >"$PM_MNT/private/file.txt" )
	pm_wait_entry pm:private file.txt
	pm_wait_meta pm:private file.txt mode 0600 10
	pm_assert_eq "600" "$(stat -c '%a' "$PM_MNT/private/file.txt")" "file create honours the caller's umask"

	# mkdir -m is honoured instead of being replaced by default_dir_mode.
	mkdir -m 0700 "$PM_MNT/explicit"
	pm_wait_meta pm: explicit mode 0700 10
	pm_assert_eq "700" "$(stat -c '%a' "$PM_MNT/explicit")" "mkdir -m mode reaches the backend"

	# Default umask with the default modes: mode and owner equal the resolved
	# defaults, so the entry stays sparse (no sidecar object at all).
	( umask 022; mkdir "$PM_MNT/plain" )
	( umask 022; printf 'default\n' >"$PM_MNT/plain/file.txt" )
	pm_wait_entry pm:plain file.txt
	pm_wait_meta pm:plain file.txt mode 0644 10
	pm_assert_eq "755" "$(stat -c '%a' "$PM_MNT/plain")" "default directory mode"
	pm_assert_eq "644" "$(stat -c '%a' "$PM_MNT/plain/file.txt")" "default file mode"
	sleep 1 # leave room for a queued (buggy) sidecar writeback
	pm_assert_no_file "$(pm_sidecar_path plain)"
	pm_assert_no_file "$(pm_sidecar_path plain/file.txt)"
	pm_assert_file "$(pm_sidecar_path private)"
	pm_assert_file "$(pm_sidecar_path private/file.txt)"

	pm_mount_stop "$PM_MNT"
}

# enforce it. rclone mount does not implement FUSE_ACCESS, so without
# default_permissions any user allowed by --allow-other could traverse a 0700
# directory or read a 0600 file owned by somebody else. The mount enables
# default_permissions automatically when --metadata-push is set, while
# --metadata-permissions=false restores the stock permissive behaviour.
test_mount_enforces_recorded_permissions() {
	[[ $(id -u) -eq 0 ]] && skip "root bypasses kernel permission checks"
	local duid=1050 dgid=1060
	[[ $(id -u) == $duid ]] && duid=1051
	[[ $(id -g) == $dgid ]] && dgid=1061

	_mount_setup
	pm mkdir pm:locked
	pm mkdir pm:shared
	printf 'secret\n' >"$PM_IN/secret.txt"
	pm copyto "$PM_IN/secret.txt" pm:locked/secret.txt
	pm copyto "$PM_IN/secret.txt" pm:shared/secret.txt
	pm backend setmeta pm: locked -o uid=$duid -o gid=$dgid -o mode=0700 >/dev/null
	pm backend setmeta pm: shared -o uid=$duid -o gid=$dgid -o mode=0755 >/dev/null
	pm backend setmeta pm: locked/secret.txt -o uid=$duid -o gid=$dgid -o mode=0600 >/dev/null
	pm backend setmeta pm: shared/secret.txt -o uid=$duid -o gid=$dgid -o mode=0600 >/dev/null

	_mount_start

	# getattr is not permission checked, so the recorded metadata is still
	# visible even when access itself is denied.
	pm_assert_eq "$duid" "$(stat -c '%u' "$PM_MNT/locked")" "directory owner from sidecar"
	pm_assert_eq "700" "$(stat -c '%a' "$PM_MNT/locked")" "directory mode from sidecar"

	# 0700 owned by another user: no traversal, listing or reading.
	pm_assert_failure "cd into a directory without +x" \
		bash -c 'cd "$1"' _ "$PM_MNT/locked"
	pm_assert_failure "list a directory without +rx" ls "$PM_MNT/locked"
	pm_assert_failure "read a file inside an unsearchable directory" \
		cat "$PM_MNT/locked/secret.txt"

	# 0755 is traversable, but the 0600 file is still not readable.
	pm_assert_success "list a world-readable directory" ls "$PM_MNT/shared"
	pm_assert_failure "read a 0600 file owned by another user" \
		cat "$PM_MNT/shared/secret.txt"

	# Entries created by the mount user stay accessible.
	printf 'mine\n' >"$PM_MNT/mine.txt"
	pm_assert_eq "mine" "$(cat "$PM_MNT/mine.txt")" "own file stays readable"

	# Prove the enforcement is the kernel option and not an accident.
	pm_assert_contains \
		"$(awk -v m="$PM_MNT" '$2 == m { print $4 }' /proc/self/mounts)" \
		"default_permissions" "mount options include default_permissions"

	pm_mount_stop "$PM_MNT"

	# Escape hatch: the previous, permissive behaviour is one flag away.
	_mount_start --metadata-permissions=false
	pm_assert_eq "secret" "$(cat "$PM_MNT/locked/secret.txt")" \
		"--metadata-permissions=false keeps the permissive behaviour"
	pm_mount_stop "$PM_MNT"
}

# proxy_links=true stores the symlink target in the entry's sidecar, so a
# link is one encrypted object at rest instead of a payload plus a metadata
# sidecar. The mount still sees a normal symlink through --vfs-links.
test_mount_proxy_links_sidecar_only() {
	PM_CFG_OPTS[proxy_links]=true
	_mount_setup
	pm mkdir pm:dir
	printf 'payload\n' >"$PM_IN/target.txt"
	pm copyto "$PM_IN/target.txt" pm:dir/target.txt

	_mount_start --umask 0000 --link-perms 0777
	raw_before=$(find "$PM_SRC" -type f | wc -l | tr -d ' ')

	ln -s target.txt "$PM_MNT/dir/link"
	pm_wait_entry pm:dir link.rclonelink

	pm_assert_eq "target.txt" "$(readlink "$PM_MNT/dir/link")" "readlink via proxy link"
	pm_assert_eq "payload" "$(cat "$PM_MNT/dir/link")" "proxy link resolves to its target"
	pm_assert_no_file "$PM_MNT/dir/link.rclonelink"
	pm_assert_mount_has_no_sidecars "$PM_MNT"
	pm_assert_eq "target.txt" "$(pm cat pm:dir/link.rclonelink)" "backend target from sidecar"
	pm_assert_eq "1" "$(pm lsjson pm:dir | jq '[.[] | select(.Name == "link.rclonelink")] | length')" \
		"one virtual backend object per link"
	pm_assert_eq "$((raw_before + 1))" \
		"$(find "$PM_SRC" -type f | wc -l | tr -d ' ')" \
		"one raw object (the sidecar) per proxied link"
	pm_assert_raw_has_no_plaintext link

	mv "$PM_MNT/dir/link" "$PM_MNT/dir/renamed"
	pm_wait_entry pm:dir renamed.rclonelink
	pm_wait_entry_gone pm:dir link.rclonelink
	pm_assert_eq "target.txt" "$(readlink "$PM_MNT/dir/renamed")" "target survives rename"

	# The link must also be resolvable from a fresh listing after remount.
	pm_mount_stop "$PM_MNT"
	_mount_start
	pm_assert_eq "target.txt" "$(readlink "$PM_MNT/dir/renamed")" "target survives remount"
	pm_assert_eq "payload" "$(cat "$PM_MNT/dir/renamed")" "remounted link resolves"

	rm "$PM_MNT/dir/renamed"
	pm_wait_entry_gone pm:dir renamed.rclonelink
	pm_assert_eq "1" "$(pm lsjson pm:dir | jq 'length')" "only the regular target remains"
	pm_assert_eq "$raw_before" \
		"$(find "$PM_SRC" -type f | wc -l | tr -d ' ')" \
		"deleting a proxied link removes its sidecar"

	pm_mount_stop "$PM_MNT"
}

# sidecar_layout=dir keeps every entry's attributes in one aggregate map per
# directory, so a directory with many entries still costs one metadata object.
# The whole path (FUSE chmod/chown, rename, delete, remount, encryption at
# rest) must behave exactly like the sparse layout.
test_mount_dir_layout_roundtrip() {
	PM_CFG_OPTS[sidecar_layout]=dir
	_mount_setup
	_mount_start

	mkdir -p "$PM_MNT/proj"
	for name in a.txt b.txt c.txt; do
		printf 'payload %s\n' "$name" >"$PM_MNT/proj/$name"
	done
	pm_wait_entry pm:proj a.txt
	pm_wait_entry pm:proj c.txt

	for name in a.txt b.txt c.txt; do
		chmod 0700 "$PM_MNT/proj/$name"
	done
	pm_wait_meta pm:proj a.txt mode 0700 5
	pm_wait_meta pm:proj c.txt mode 0700 5

	# One payload per file plus the root map and the directory map.
	pm_assert_eq "5" "$(find "$PM_SRC" -type f | wc -l | tr -d ' ')" "one map per directory, not per entry"
	pm_assert_mount_has_no_sidecars "$PM_MNT"
	pm_assert_raw_has_no_plaintext proj
	pm_assert_raw_has_no_plaintext a.txt

	# S_ISGID is inherited by new subdirectories through the map too.
	mkdir "$PM_MNT/proj/sg"
	chmod g+s "$PM_MNT/proj/sg"
	pm_wait_meta pm:proj sg mode 02755 10
	mkdir "$PM_MNT/proj/sg/sub"
	pm_wait_meta pm:proj/sg sub mode 02755 10
	pm_assert_eq "2755" "$(stat -c '%a' "$PM_MNT/proj/sg/sub")" "dir layout propagates setgid"

	# Rename and delete must follow the map keys.
	mv "$PM_MNT/proj/a.txt" "$PM_MNT/proj/renamed.txt"
	pm_wait_entry pm:proj renamed.txt
	pm_wait_entry_gone pm:proj a.txt
	pm_assert_eq "700" "$(stat -c '%a' "$PM_MNT/proj/renamed.txt")" "mode follows the rename"

	rm -f "$PM_MNT/proj/c.txt"
	pm_wait_entry_gone pm:proj c.txt

	# Metadata must survive a remount, and the layout must be recognised again.
	pm_mount_stop "$PM_MNT"
	_mount_start
	pm_wait_entry pm:proj renamed.txt
	pm_assert_eq "700" "$(stat -c '%a' "$PM_MNT/proj/renamed.txt")" "mode survives a remount"
	pm_assert_eq "$(id -u)" "$(stat -c '%u' "$PM_MNT/proj/renamed.txt")" "owner survives a remount"

	pm_mount_stop "$PM_MNT"
}

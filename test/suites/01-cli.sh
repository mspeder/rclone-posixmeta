#!/usr/bin/env bash
# shellcheck shell=bash
#
# Suite 01: non-mount CLI verification of backend/posixmeta invariants.
#
# Covers sparse sidecar creation, sidecar hiding, uid/gid inheritance,
# atime/ctime suppression, move/delete/rmdir lifecycle, rejection of
# direct writes to sidecar paths (AGENTS.md Procedure A), and the bounds
# reported by `rclone backend cachestats`.

suite_require_cmd jq
suite_require_cmd sha256sum

# Common local -> posixmeta setup used by every test in this suite.
_cli_setup() {
	pm_write_local_config "$PM_CONFIG" "$PM_SRC"
}

_cli_make_file() {
	local remote=$1 body=${2:-"hello"}
	mkdir -p "$PM_IN/$(dirname "$remote")"
	printf '%s\n' "$body" >"$PM_IN/$remote"
	pm copyto "$PM_IN/$remote" "pm:$remote"
}

test_cli_default_upload_is_sparse_and_resolves_defaults() {
	_cli_setup
	_cli_make_file default.txt

	pm_assert_no_file "$(pm_sidecar_path default.txt)"
	pm_assert_eq "0644" "$(pm_meta_of pm: default.txt mode)" "file mode"
	pm_assert_eq "1000" "$(pm_meta_of pm: default.txt uid)" "file uid"
	pm_assert_eq "1000" "$(pm_meta_of pm: default.txt gid)" "file gid"
}

test_cli_parent_owner_inheritance() {
	_cli_setup
	pm mkdir pm:proj
	pm backend setmeta pm: proj -o uid=2001 -o gid=2002 >/dev/null

	pm_assert_file "$(pm_sidecar_path proj)"
	pm_assert_json "$(pm_sidecar_json proj)" '.uid' "2001" "dir sidecar uid"
	pm_assert_json "$(pm_sidecar_json proj)" '.gid' "2002" "dir sidecar gid"
	pm_assert_json "$(pm_sidecar_json proj)" 'has("mode")' "false" "dir sidecar stays sparse"

	_cli_make_file proj/inherited.txt
	pm_assert_no_file "$(pm_sidecar_path proj/inherited.txt)"
	pm_assert_eq "2001" "$(pm_meta_of pm:proj inherited.txt uid)" "inherited uid"
	pm_assert_eq "2002" "$(pm_meta_of pm:proj inherited.txt gid)" "inherited gid"
	pm_assert_eq "0644" "$(pm_meta_of pm:proj inherited.txt mode)" "inherited default mode"
}

test_cli_mode_override_is_sparse_and_hidden() {
	_cli_setup
	_cli_make_file x.txt
	pm backend setmeta pm: x.txt -o mode=0750 >/dev/null

	pm_assert_file "$(pm_sidecar_path x.txt)"
	pm_assert_json "$(pm_sidecar_json x.txt)" '.mode' "0750" "sidecar mode"
	pm_assert_json "$(pm_sidecar_json x.txt)" 'has("uid")' "false" "uid omitted when inherited"
	pm_assert_json "$(pm_sidecar_json x.txt)" 'has("gid")' "false" "gid omitted when inherited"

	pm_assert_not_contains "$(pm lsf pm:)" ".posixmeta" "sidecar hidden from lsf"
	pm_assert_eq "0750" "$(pm_meta_of pm: x.txt mode)" "resolved mode"
	pm_assert_eq "1000" "$(pm_meta_of pm: x.txt uid)" "resolved uid"
}

test_cli_sidecar_deleted_when_attributes_revert() {
	_cli_setup
	_cli_make_file x.txt
	pm backend setmeta pm: x.txt -o mode=0700 >/dev/null
	pm_assert_file "$(pm_sidecar_path x.txt)"

	pm backend setmeta pm: x.txt -o mode=0644 >/dev/null
	pm_assert_no_file "$(pm_sidecar_path x.txt)"
	pm_assert_eq "0644" "$(pm_meta_of pm: x.txt mode)" "mode reverted"
}

test_cli_btime_and_symlink_roundtrip() {
	_cli_setup
	_cli_make_file x.txt
	pm backend setmeta pm: x.txt \
		-o btime=2026-11-22T08:00:00Z \
		-o symlink=/target/one >/dev/null

	pm_assert_json "$(pm_sidecar_json x.txt)" '.btime' "2026-11-22T08:00:00Z" "sidecar btime"
	pm_assert_json "$(pm_sidecar_json x.txt)" '.symlink' "/target/one" "sidecar symlink"
	pm_assert_eq "2026-11-22T08:00:00Z" "$(pm_meta_of pm: x.txt btime)" "resolved btime"
	pm_assert_eq "/target/one" "$(pm_meta_of pm: x.txt symlink)" "resolved symlink"
}

test_cli_atime_ctime_do_not_write_sidecar() {
	_cli_setup
	_cli_make_file x.txt
	pm backend setmeta pm: x.txt -o mode=0700 >/dev/null

	local before after
	before=$(sha256sum "$(pm_sidecar_path x.txt)" | awk '{print $1}')
	pm backend setmeta pm: x.txt \
		-o atime=2020-01-01T00:00:00Z \
		-o ctime=2021-01-01T00:00:00Z >/dev/null
	after=$(sha256sum "$(pm_sidecar_path x.txt)" | awk '{print $1}')

	pm_assert_eq "$before" "$after" "atime/ctime must not rewrite the sidecar"
	pm_assert_json "$(pm_sidecar_json x.txt)" 'has("atime")' "false" "no atime in sidecar"
	pm_assert_json "$(pm_sidecar_json x.txt)" 'has("ctime")' "false" "no ctime in sidecar"
}

test_cli_move_propagates_sidecar() {
	_cli_setup
	_cli_make_file a.txt
	pm backend setmeta pm: a.txt -o mode=0700 >/dev/null
	local before
	before=$(pm_sidecar_json a.txt)

	pm moveto pm:a.txt pm:b.txt

	pm_assert_no_file "$(pm_sidecar_path a.txt)"
	pm_assert_file "$(pm_sidecar_path b.txt)"
	pm_assert_eq "$before" "$(pm_sidecar_json b.txt)" "sidecar content preserved across move"
	pm_assert_eq "0700" "$(pm_meta_of pm: b.txt mode)" "metadata preserved across move"
}

test_cli_delete_removes_sidecar() {
	_cli_setup
	_cli_make_file x.txt
	pm backend setmeta pm: x.txt -o mode=0700 >/dev/null

	pm delete pm:x.txt

	pm_assert_no_file "$PM_SRC/x.txt"
	pm_assert_no_file "$(pm_sidecar_path x.txt)"
}

test_cli_direct_sidecar_write_rejected() {
	_cli_setup
	printf 'payload\n' >"$PM_IN/payload.txt"

	pm_assert_failure "copyto onto sidecar path" \
		pm copyto "$PM_IN/payload.txt" pm:.posixmeta.x.txt
	pm_assert_no_file "$PM_SRC/.posixmeta.x.txt"
}

test_cli_hide_sidecars_false_exposes_sidecar() {
	PM_CFG_OPTS[hide_sidecars]=false
	_cli_setup
	_cli_make_file x.txt
	pm backend setmeta pm: x.txt -o mode=0700 >/dev/null

	pm_assert_contains "$(pm lsf pm:)" ".posixmeta.x.txt" "sidecar visible when hide_sidecars=false"
	pm_assert_eq "0700" "$(pm_meta_of pm: x.txt mode)" "metadata still resolved"
}

test_cli_invalid_mode_rejected() {
	_cli_setup
	_cli_make_file x.txt

	pm_assert_failure "invalid octal mode" \
		pm backend setmeta pm: x.txt -o mode=not-a-mode
	pm_assert_no_file "$(pm_sidecar_path x.txt)"
	pm_assert_eq "0644" "$(pm_meta_of pm: x.txt mode)" "default mode retained"
}

test_cli_rmdir_removes_directory_sidecar() {
	_cli_setup
	pm mkdir pm:dir
	pm backend setmeta pm: dir -o mode=0700 >/dev/null
	pm_assert_file "$(pm_sidecar_path dir)"

	pm rmdir pm:dir

	pm_assert_no_file "$PM_SRC/dir"
	pm_assert_no_file "$(pm_sidecar_path dir)"
}

test_cli_directory_defaults_resolve() {
	_cli_setup
	pm mkdir pm:dir

	pm_assert_eq "0755" "$(pm_meta_of pm: dir mode)" "default dir mode"
	pm_assert_eq "1000" "$(pm_meta_of pm: dir uid)" "default dir uid"
	pm_assert_eq "1000" "$(pm_meta_of pm: dir gid)" "default dir gid"
	pm_assert_no_file "$(pm_sidecar_path dir)"
}

test_cli_subpath_metadata_inherits_ancestor_owner() {
	_cli_setup
	pm mkdir pm:proj
	pm backend setmeta pm: proj -o uid=2001 -o gid=2002 >/dev/null
	_cli_make_file proj/inh.txt

	# Addressing the file directly re-roots the wrapped remote at proj, so the
	# owning directory's sidecar lives above the wrapped root. It must still be
	# discovered for inheritance to work.
	pm_assert_eq "2001" \
		"$(pm lsjson --metadata pm:proj/inh.txt | jq -r '.[0].Metadata.uid')" \
		"uid inherited across the subpath root boundary"
	pm_assert_eq "2002" \
		"$(pm lsjson --metadata pm:proj/inh.txt | jq -r '.[0].Metadata.gid')" \
		"gid inherited across the subpath root boundary"
	pm_assert_no_file "$(pm_sidecar_path proj/inh.txt)"
}

test_cli_dir_move_propagates_directory_sidecar() {
	_cli_setup
	pm mkdir pm:d1
	pm backend setmeta pm: d1 -o uid=2001 >/dev/null

	pm moveto pm:d1 pm:d2

	pm_assert_no_file "$PM_SRC/d1"
	pm_assert_dir "$PM_SRC/d2"
	pm_assert_no_file "$PM_SRC/.posixmeta.d1"
	pm_assert_file "$PM_SRC/.posixmeta.d2"
	pm_assert_eq "2001" \
		"$(pm lsjson --metadata pm: | jq -r '.[] | select(.Name == "d2") | .Metadata.uid')" \
		"directory sidecar preserved across move"
}

test_cli_purge_removes_directory_sidecar() {
	_cli_setup
	pm mkdir pm:proj
	_cli_make_file proj/a.txt
	pm backend setmeta pm: proj -o mode=0700 >/dev/null
	pm_assert_file "$(pm_sidecar_path proj)"

	pm purge pm:proj

	pm_assert_no_file "$PM_SRC/proj"
	pm_assert_no_file "$(pm_sidecar_path proj)"
}

# The sidecar cache must expose its configured bounds so long-lived mounts can
# be monitored. A fresh CLI process starts with an empty cache, so only the
# caps and counters are meaningful here (residency is covered by the Go unit
# tests in suite 00).
test_cli_cachestats_reports_cache_caps() {
	PM_CFG_OPTS[cache_max_entries]=1234
	PM_CFG_OPTS[cache_max_size]=1M
	_cli_setup
	_cli_make_file a.txt

	local stats
	stats=$(pm backend cachestats pm: 2>/dev/null)

	pm_assert_contains "$stats" '"max_entries": 1234' "configured entry cap reported"
	pm_assert_contains "$stats" '"max_bytes": 1048576' "configured byte cap reported"
	pm_assert_contains "$stats" '"entries": 0' "fresh process starts with an empty cache"
	pm_assert_contains "$stats" '"ttl": "5m0s"' "cache ttl reported"
	pm_assert_contains "$stats" '"evictions": 0' "eviction counter reported"
}

# Sidecars live next to their entry and carry the entry name after the
# prefix, so `dir/x.txt` is described by `dir/.posixmeta.x.txt`. The old
# `<name>.posixmeta` layout is not read or written any more.
test_cli_sidecar_prefix_layout() {
	_cli_setup
	_cli_make_file dir/x.txt
	pm backend setmeta pm: dir/x.txt -o mode=0700 >/dev/null

	pm_assert_file "$PM_SRC/dir/.posixmeta.x.txt"
	pm_assert_no_file "$PM_SRC/dir/x.txt.posixmeta"
	pm_assert_eq "0700" "$(pm_meta_of pm:dir x.txt mode)" "prefixed sidecar is read back"
}

# proxy_links keeps the whole symlink in the entry's sidecar: no
# `.rclonelink` payload is uploaded, and the virtual object is served from the
# `symlink` attribute.
test_cli_proxy_links_roundtrip() {
	PM_CFG_OPTS[proxy_links]=true
	_cli_setup
	printf 'target/path.txt' >"$PM_IN/link-target"
	pm copyto "$PM_IN/link-target" pm:dir/link.txt.rclonelink

	pm_assert_eq "target/path.txt" "$(pm cat pm:dir/link.txt.rclonelink)" "target served from sidecar"
	pm_assert_file "$(pm_sidecar_path dir/link.txt)"
	pm_assert_json "$(pm_sidecar_json dir/link.txt)" '.symlink' "target/path.txt" "sidecar symlink"
	pm_assert_no_file "$PM_SRC/dir/link.txt.rclonelink"
	pm_assert_contains "$(pm lsf pm:dir)" "link.txt.rclonelink" "link listed"
	pm_assert_not_contains "$(pm lsf pm:dir)" ".posixmeta" "sidecar hidden"
	pm_assert_eq "1" "$(find "$PM_SRC/dir" -type f | wc -l | tr -d ' ')" "one object per link"

	pm moveto pm:dir/link.txt.rclonelink pm:dir/renamed.rclonelink
	pm_assert_no_file "$(pm_sidecar_path dir/link.txt)"
	pm_assert_file "$(pm_sidecar_path dir/renamed)"
	pm_assert_eq "target/path.txt" "$(pm cat pm:dir/renamed.rclonelink)" "target preserved across move"

	pm delete pm:dir/renamed.rclonelink
	pm_assert_no_file "$(pm_sidecar_path dir/renamed)"
	pm_assert_eq "0" "$(pm lsjson pm:dir | jq 'length')" "link removed"
}

# sidecar_layout=dir stores one aggregate map per directory instead of a
# sidecar per entry. The map is hidden from listings, resolves like a sparse
# sidecar, and stays a single object per directory.
test_cli_dir_layout_one_map_per_directory() {
	PM_CFG_OPTS[sidecar_layout]=dir
	_cli_setup
	_cli_make_file a.txt
	_cli_make_file dir/b.txt
	pm backend setmeta pm: a.txt -o mode=0700 >/dev/null
	pm backend setmeta pm: dir/b.txt -o mode=0600 >/dev/null
	pm backend setmeta pm: dir -o uid=2001 -o gid=2002 >/dev/null

	pm_assert_file "$(pm_dir_map_path '')"
	pm_assert_file "$(pm_dir_map_path dir)"
	pm_assert_no_file "$PM_SRC/.posixmeta.a.txt"
	pm_assert_no_file "$PM_SRC/dir/.posixmeta.b.txt"
	pm_assert_json "$(cat "$(pm_dir_map_path '')")" '.v' "2" "map schema version"
	pm_assert_json "$(cat "$(pm_dir_map_path '')")" '.layout' "null" "no separate layout tag"
	pm_assert_json "$(cat "$(pm_dir_map_path '')")" '.crc | test("^[0-9a-f]{8}$")' "true" "bare hex checksum"
	pm_assert_json "$(cat "$(pm_dir_map_path '')")" '.entries["a.txt"].mode' "0700" "entry mode stored"
	pm_assert_json "$(cat "$(pm_dir_map_path '')")" '.entries["dir"].uid' "2001" "entry uid stored"
	pm_assert_json "$(cat "$(pm_dir_map_path dir)")" '.entries["b.txt"].mode' "0600" "child entry mode stored"

	pm_assert_not_contains "$(pm lsf pm:)" ".posixmeta" "root map hidden from lsf"
	pm_assert_not_contains "$(pm lsf pm:dir)" ".posixmeta" "inner map hidden from lsf"
	pm_assert_eq "0700" "$(pm_meta_of pm: a.txt mode)" "mode resolved"
	pm_assert_eq "2001" "$(pm_meta_of pm:dir b.txt uid)" "inherited uid resolved"
	pm_assert_eq "2" "$(find "$PM_SRC" -name '.posixmeta' | wc -l | tr -d ' ')" "one map per directory"
}

# Moving and deleting entries edits the directory map in place, and a map with
# no entries left is removed while the root map stays as the layout marker.
test_cli_dir_layout_move_delete_rmdir() {
	PM_CFG_OPTS[sidecar_layout]=dir
	_cli_setup
	_cli_make_file dir/x.txt
	pm backend setmeta pm: dir/x.txt -o mode=0700 >/dev/null

	pm moveto pm:dir/x.txt pm:dir/y.txt
	pm_assert_json "$(cat "$(pm_dir_map_path dir)")" '.entries | has("x.txt")' "false" "old key dropped"
	pm_assert_json "$(cat "$(pm_dir_map_path dir)")" '.entries["y.txt"].mode' "0700" "mode moved"
	pm_assert_eq "0700" "$(pm_meta_of pm:dir y.txt mode)" "moved mode resolved"

	pm delete pm:dir/y.txt
	pm_assert_no_file "$(pm_dir_map_path dir)"
	pm_assert_json "$(cat "$(pm_dir_map_path '')")" '.entries | length' "0" "root map kept as the layout marker"
}

# A remote written by one layout must not be opened with the other: the
# metadata is not interoperable and ignoring it would show wrong ownership.
test_cli_dir_layout_rejects_sparse_data() {
	_cli_setup
	_cli_make_file a.txt
	pm backend setmeta pm: a.txt -o mode=0700 >/dev/null

	PM_CFG_OPTS[sidecar_layout]=dir
	_cli_setup
	pm_assert_failure "dir layout over sparse data" pm lsf pm:

	local err
	err=$(pm lsf pm: 2>&1 || true)
	pm_assert_contains "$err" "refusing to mix layouts" "explicit layout mismatch error"
}

# repair is a dry run by default, requires confirm=REPAIR to write, and
# quarantines the damaged document before rebuilding it.
test_cli_dir_layout_repair() {
	PM_CFG_OPTS[sidecar_layout]=dir
	_cli_setup
	_cli_make_file a.txt
	pm backend setmeta pm: a.txt -o mode=0700 >/dev/null

	local map
	map=$(pm_dir_map_path '')
	printf '{"v":2,"crc":"00000000","entries":{"a.txt":{"mode":"0700"}}}' >"$map"

	local report
	report=$(pm backend repair pm: 2>/dev/null)
	pm_assert_contains "$report" '"would_repair": 1' "dry run reports the repair"
	pm_assert_json "$report" '.repaired' "0" "dry run writes nothing"

	pm_assert_failure "repair without confirmation" pm backend repair pm: -o dry_run=false

	report=$(pm backend repair pm: -o dry_run=false -o confirm=REPAIR 2>/dev/null)
	pm_assert_json "$report" '.repaired' "1" "repair performed"
	pm_assert_eq "1" "$(find "$PM_SRC" -maxdepth 1 -name '.posixmeta.corrupt.*' | wc -l | tr -d ' ')" "original document quarantined"
	pm_assert_eq "0700" "$(pm_meta_of pm: a.txt mode)" "salvaged metadata served"
}

# proxy_links and the dir layout compose: a link is one key in the directory
# map, with no payload object and no per-entry sidecar.
test_cli_dir_layout_proxy_links() {
	PM_CFG_OPTS[sidecar_layout]=dir
	PM_CFG_OPTS[proxy_links]=true
	_cli_setup
	printf 'target/path.txt' >"$PM_IN/link-target"
	pm copyto "$PM_IN/link-target" pm:link.txt.rclonelink

	pm_assert_eq "target/path.txt" "$(pm cat pm:link.txt.rclonelink)" "target served from the map"
	pm_assert_json "$(cat "$(pm_dir_map_path '')")" '.entries["link.txt"].symlink' "target/path.txt" "symlink stored in the map"
	pm_assert_eq "1" "$(find "$PM_SRC" -type f | wc -l | tr -d ' ')" "one object for the whole directory"
	pm_assert_contains "$(pm lsf pm:)" "link.txt.rclonelink" "link listed"
	pm_assert_not_contains "$(pm lsf pm:)" ".posixmeta" "map hidden"

	pm moveto pm:link.txt.rclonelink pm:renamed.rclonelink
	pm_assert_json "$(cat "$(pm_dir_map_path '')")" '.entries | has("link.txt")' "false" "old key dropped"
	pm_assert_eq "target/path.txt" "$(pm cat pm:renamed.rclonelink)" "target preserved across move"
}

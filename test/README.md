# rclone-posixmeta test harness

Automated verification for the `posixmeta` wrapper backend. The harness
implements the procedures in `AGENTS.md` as repeatable, isolated suites and adds
Go unit tests for the backend internals.

## Quick start

```bash
# Build (CGO required for FUSE) and run everything
./test/run.sh

# Run a single suite by name
./test/run.sh 01-cli
./test/run.sh 02-mount-crypt

# List tests without running them
./test/run.sh --list

# Run a subset
./test/run.sh --filter 'inheritance|move'

# CI-friendly output
./test/run.sh --tap
```

`make test`, `make test-go`, `make test-cli` and `make test-mount` are thin
wrappers around the same entry point.

## Layout

```text
test/
  run.sh                  # runner: build, discovery, execution, reporting
  lib/harness.sh          # assertions, isolation, config generation, mounts
  suites/00-go-unit.sh    # go test ./backend/posixmeta/
  suites/01-cli.sh        # sidecars, inheritance, hiding, move/delete (no mount)
  suites/02-mount-crypt.sh# FUSE mount over crypt(local) -> posixmeta
```

## Isolation and safety

Every test runs in a fresh child process with:

* its own scratch tree under `$PM_RUN_ROOT` (`/tmp/pm-harness.XXXXXX`);
* `HOME`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME`, `RCLONE_CONFIG` and
  `RCLONE_CACHE_DIR` redirected into that tree;
* an explicit `--config` and `--cache-dir` for every rclone invocation, so
  `~/.config/rclone` and `~/.cache/rclone` are never touched;
* crypt passwords obscured with `rclone obscure` (plaintext crypt passwords are
  rejected by rclone);
* a per-test kernel timeout (`--timeout`, default 180s) plus `--kill-after`;
* an `EXIT` trap that unmounts FUSE filesystems, with a parent-side sweep of
  `/proc`/`findmnt` as a backstop;
* scratch directories removed on success, kept on failure (and with `--keep`).

The runner also removes `$PM_RUN_ROOT` when the whole run succeeds.

## Test statuses

| Status | Meaning |
| --- | --- |
| `PASS` | test completed successfully |
| `FAIL` | test failed; log tail and scratch path are printed |
| `SKIP` | suite capability missing (e.g. `/dev/fuse`, `fusermount`, `jq`) |
| `XFAIL` | test called `xfail "reason"` and failed as expected (known gap) |
| `XPASS` | test called `xfail` but passed: the gap may have been fixed |

Exit code is non-zero on `FAIL`. `XPASS` is reported but not fatal unless
`--strict-xpass` is given.

## Runner options

```text
--binary PATH        rclone binary to test (default: ./rclone-posixmeta)
--build              force a CGO-enabled rebuild
--no-build           never build; fail if the binary is missing
--filter REGEX       only run tests whose "suite/test" matches REGEX
--list               list discovered tests and exit
--timeout SECONDS    per-test timeout (default: 180)
--keep               keep scratch directories/logs for passing tests
--verbose            stream each test's output as it runs
--tap                emit TAP-13 output
--strict-xpass       treat an unexpected XPASS as a failure
```

By default the runner rebuilds only when a `*.go` file is newer than the
binary. A binary supplied with `--binary` is used as-is unless `--build` is
also given.

## Writing tests

A suite is a file in `test/suites/` that is sourced by the runner. It may call
suite-level requirements at the top:

```bash
suite_require_cmd jq
suite_require_fuse
```

and defines tests as functions named `test_*`:

```bash
test_cli_something() {
    pm_write_local_config "$PM_CONFIG" "$PM_SRC"
    printf 'hello\n' >"$PM_IN/x.txt"
    pm copyto "$PM_IN/x.txt" pm:x.txt

    pm_assert_no_file "$(pm_sidecar_path x.txt)"
    pm_assert_eq "0644" "$(pm_meta_of pm: x.txt mode)" "default mode"
}
```

Useful helpers from `test/lib/harness.sh`:

* assertions: `pm_assert_eq`, `pm_assert_ne`, `pm_assert_contains`,
  `pm_assert_file`, `pm_assert_no_file`, `pm_assert_json`,
  `pm_assert_success`, `pm_assert_failure`;
* rclone: `pm <args>` (isolated config/cache), `pm_obscure`,
  `pm_meta_of`, `pm_wait_meta`, `pm_wait_entry`, `pm_wait_empty`,
  `pm_sidecar_path`, `pm_sidecar_json`;
* configs: `pm_write_local_config`, `pm_write_crypt_config`, with overrides via
  the `PM_CFG_OPTS` associative array;
* mounts: `pm_mount_start`, `pm_mount_stop` (also run automatically on exit),
  `pm_assert_mount_has_no_sidecars`; posixmeta mounts must pass
  `--metadata-push` and pass `--vfs-links` to enable symlinks (see
  `_mount_start` in `02-mount-crypt.sh`). `--metadata-push` is enforced by a
  backend preflight; `--vfs-links` is intentionally optional and its absence
  is covered by a negative test. `--metadata-push` also enables kernel
  permission checks (`--metadata-permissions`, default on); tests that write
  into a directory owned by another user must keep it group/world writable or
  create their entries before handing it over;
* control flow: `skip "reason"`, `xfail "reason"`.

Tests must be independent: each runs in its own child process and scratch
directory, and the runner never reuses a mounted filesystem between tests.

Subpath semantics (`pm:subdir`) are covered explicitly: the backend mirrors
`fs.ErrorIsFile` roots and reads sidecars at and above the wrapped root, so
`moveto`, `rmdir`, `purge`, and `lsjson --metadata pm:dir/file` keep directory
sidecars and parent-owner inheritance intact.

## FUSE symlinks

Symlinks use upstream rclone's `.rclonelink` model and are enabled with
`--vfs-links` (or the global `--links`). A link is a single backend object
whose payload is the target; no `.posixmeta` sidecar is created unless metadata
is explicitly pushed. `--vfs-links` is opt-in and has no mount preflight:
without it `ln -s`/`readlink` fail loudly and nothing is silently lost.

Covered by `test_mount_symlinks_via_vfs_links` (relative/absolute/dangling
links, symlink type and `--link-perms` mode, creator uid/gid instead of the
inherited directory owner, backend `.rclonelink` payload, one object plus one
ownership sidecar per link, rename, delete and remount),
`test_mount_without_vfs_links_rejects_symlink` (opt-in failure leaves no object
behind), and the listing regression from
`docs/adr/0007-mount-symlink-listing.md`:
`test_mount_symlink_listed_immediately` (forget the VFS directory cache with
`vfs/forget` over `--rc`, create the link, require it in `ls` with no polling)
and `test_mount_symlink_listed_after_dir_cache_expiry` (same scenario without
`--rc`, using ten fresh subdirectories and a real `--dir-cache-time` expiry).
See `docs/adr/0002-fuse-symlinks-via-vfs-links.md`.

With `PM_CFG_OPTS[proxy_links]=true` the target is stored in the entry's
sidecar instead of a `.rclonelink` payload (`docs/adr/0008-proxy-link-files.md`).
`test_mount_proxy_links_sidecar_only` covers `ln -s`, `readlink`, reading
through the link, the virtual `link.rclonelink` listing, exactly one raw object
(the sidecar) per link, rename, delete and remount over `crypt`; the CLI side is
covered by `test_cli_proxy_links_roundtrip`, and
`test_cli_sidecar_prefix_layout` pins the `<dir>/.posixmeta.<entry>` naming
(`docs/adr/0009-sidecar-prefix-naming.md`).

## Per-directory aggregate layout

Set `PM_CFG_OPTS[sidecar_layout]=dir` to exercise the aggregate layout, where one
`<dir>/.posixmeta` map holds every entry of a directory
(`docs/adr/0010-per-directory-aggregate-sidecars.md`). `pm_dir_map_path <dir>`
returns its on-disk path. Covered by `test_cli_dir_layout_one_map_per_directory`
(one object per directory, map hidden, metadata/inheritance resolved),
`test_cli_dir_layout_move_delete_rmdir` (keys follow renames, empty maps are
removed, the root map stays), `test_cli_dir_layout_rejects_sparse_data` and
`test_cli_dir_layout_repair` (dry run, `confirm=REPAIR`, quarantine, salvage),
`test_cli_dir_layout_proxy_links`, `test_mount_dir_layout_roundtrip` (chmod
through FUSE, rename, delete, remount, encryption at rest) and the Go tests
`TestDirLayout*` in `backend/posixmeta/dirmap_test.go`.

FUSE `chmod`/`chown`/`stat` propagation is implemented in the local rclone fork
(`third_party/rclone`) and is **opt-in** via `--metadata-push`; without the flag
`rclone mount` keeps stock behaviour (mount-wide `--file-perms`/`--dir-perms`,
chmod/chown ignored). `posixmeta` declares `RequiresMetadataPush()`, so mounting
it without the flag fails fast instead of silently dropping metadata.

Creation metadata follows the same flag: `create`, `mkdir`, `mknod` and
`symlink` record the creating process's uid/gid (the parent's group when the
directory has the setgid bit) and the kernel-provided mode (already masked by the
caller's umask), with `S_ISGID` inherited onto new subdirectories only, so a file
created inside a directory owned by another user belongs to its creator and a
setgid tree keeps its group all the way down (`mkdir -m` and `umask` behave like
ext4). Modes equal to `default_file_mode`/`default_dir_mode` still write no
sidecar. See `docs/adr/0005-create-ownership-from-fuse-request.md` and
`docs/adr/0011-create-mode-from-fuse-request.md`.

Covered by `test_mount_chmod_via_fuse_is_forwarded`,
`test_mount_chown_via_fuse_is_forwarded`,
`test_mount_getattr_reflects_sidecar_metadata`,
`test_mount_create_records_creating_process`,
`test_mount_create_in_setgid_dir_inherits_group`,
`test_mount_create_honours_requested_mode`,
`test_mount_roundtrip_metadata_inheritance_and_encryption`,
`test_mount_requires_metadata_push`,
`test_mount_without_metadata_push_keeps_stock_behaviour` and
`test_mount_metadata_push_is_opt_in`.

## Kernel permission enforcement

Reporting `mode`/`uid`/`gid` without letting the kernel check them made
`chmod 0700` cosmetic: `rclone mount` does not implement `FUSE_ACCESS`, so any
caller allowed by `--allow-other` could `cd` into a `0700` directory owned by
another user and read its `0600` files. `cmd/mount` now passes
`default_permissions` whenever `--metadata-push` is set (option
`--metadata-permissions`, default true); `--metadata-permissions=false` restores
the previous permissive behaviour.

Covered by `test_mount_enforces_recorded_permissions`: `cd`/`ls`/`cat` are
denied for a `0700` directory owned by another uid, `ls` succeeds but `cat`
fails for a `0600` file in a world-readable directory, the mounting user keeps
access to its own entries, the mount options contain `default_permissions`, and
the opt-out flag restores access. The test skips as root (root bypasses kernel
permission checks). See
`docs/adr/0006-kernel-permission-enforcement.md`.

## Durability and scale

The debounced sidecar writeback is durable across slow/failing remotes and
across unmount: updates are retried with backoff until they succeed, applied in
order per path, and flushed by `Shutdown` and an `lib/atexit` handler (a mount
process never evicts its `Fs`). Destructive operations cancel queued updates and
wait for in-flight writes. Directory listings prime the cache, metadata
resolution reads the entry first and only then its ancestors, and concurrent
cold reads of one sidecar are coalesced.

Covered by `test_mount_chmod_survives_immediate_unmount` (chmod then immediate
unmount with `writeback_delay=1h` still persists) and
`test_mount_rewrite_preserves_metadata` (rewriting content through the mount
keeps the sidecar), plus the engine unit tests in
`backend/posixmeta/engine_test.go`. See `docs/durability-and-scale.md` for the
full list of fixes and the accepted residual limitations.

When a gap is fixed, the corresponding xfail test flips to `XPASS`; run with
`--strict-xpass` to make that a hard failure and then remove the `xfail` call.

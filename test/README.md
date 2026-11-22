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
  is covered by a negative test;
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
links, symlink type and `--link-perms` mode, inherited uid/gid, backend
`.rclonelink` payload, absence of a sidecar, rename, delete and remount) and
`test_mount_without_vfs_links_rejects_symlink` (opt-in failure leaves no object
behind). See `docs/adr/0002-fuse-symlinks-via-vfs-links.md`.

FUSE `chmod`/`chown`/`stat` propagation is implemented in the local rclone fork
(`third_party/rclone`) and is **opt-in** via `--metadata-push`; without the flag
`rclone mount` keeps stock behaviour (mount-wide `--file-perms`/`--dir-perms`,
chmod/chown ignored). `posixmeta` declares `RequiresMetadataPush()`, so mounting
it without the flag fails fast instead of silently dropping metadata.

Covered by `test_mount_chmod_via_fuse_is_forwarded`,
`test_mount_chown_via_fuse_is_forwarded`,
`test_mount_getattr_reflects_sidecar_metadata`,
`test_mount_requires_metadata_push`,
`test_mount_without_metadata_push_keeps_stock_behaviour` and
`test_mount_metadata_push_is_opt_in`.

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

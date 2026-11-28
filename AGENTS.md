# AGENTS.md — `rclone-posixmeta` Development & Testing Guide

This file provides architectural context, build rules, design invariants, and repeatable verification procedures (including `rclone mount` over `crypt`) so autonomous coding agents (such as Cline) can safely maintain and extend `rclone-posixmeta`.

## 1. Project Overview

`rclone-posixmeta` is a custom `rclone` build featuring a wrapper backend (`backend/posixmeta`) that persists live POSIX metadata (`mode`, `uid`, `gid`, `symlink`, and `btime`) in **sparse per-entry JSON sidecar files** (`<dir>/.posixmeta.<entry>`) while delegating file payloads and `mtime` to any underlying `rclone` remote (e.g. `local`, `pcloud`, or `crypt`).

A second layout, `sidecar_layout = dir`, is available for trees with many
overridden entries: it stores one aggregate JSON map per directory
(`<dir>/.posixmeta`) instead of one file per entry. Both layouts are documented
in invariant 11 below and in `docs/adr/0010-per-directory-aggregate-sidecars.md`;
`sparse` remains the default and the two are never converted automatically (a
mismatch fails with an explicit error).

### Core Architecture & Layering

When used for encrypted cloud FUSE mounts, the stack is ordered from bottom to top:

1. **Backing Storage Remote** (`local`, `pcloud`, S3, etc.)
2. **`crypt` Remote** — Encrypts both file payloads and filenames (including `<dir>/.posixmeta.<entry>` sidecar files and their filenames) at rest.
3. **`posixmeta` Remote** — Sits **above** `crypt` so it can inspect plaintext entry names, hide `.posixmeta` sidecars during `List`/`NewObject`, resolve sparse metadata with parent directory owner inheritance, and maintain an in-memory TTL cache + debounced async writeback queue.
4. **`rclone mount` (VFS / FUSE)** — Exposes the `posixmeta` remote to the OS kernel.

## 2. Repository & Build Conventions

* **Primary Backend Implementation:** `backend/posixmeta/posixmeta.go` (core
  wrapper, sidecar resolution/cache/writeback) and
  `backend/posixmeta/link.go` (`proxy_links` virtual `.rclonelink` objects).
  The `sidecar_layout = dir` map layout lives in
  `backend/posixmeta/dirmap.go` and its `repair` command in
  `backend/posixmeta/repair.go`.
* **Durability / scale notes:** `docs/durability-and-scale.md` lists what was
  hardened (writeback retries, ordered per-path writes, shutdown/atexit flush,
  listing priming, feature delegation) and every accepted residual limitation.
  Design rationale: `docs/adr/0004-durable-writeback-and-delegation.md`.
* **Backend Registration:** Imported in `backend/all/all.go` (`_ "github.com/rclone/rclone/backend/posixmeta"`).
* **Build Command:**
  ```bash
  make build        # bootstraps the rclone fork if needed, then builds
  # or
  ./scripts/setup-fork.sh
  CGO_ENABLED=1 go build -o ./rclone-posixmeta .
  ```
  > **Note:** `CGO_ENABLED=1` is required on Linux for `rclone mount` FUSE support.
  > **Note:** `third_party/rclone/` is **not versioned**. Only
  > `third_party/patches/*.patch` are committed; `scripts/setup-fork.sh`
  > (also run automatically by `make build`/`make test` and `test/run.sh`)
  > materialises the fork from the `go.mod`-pinned upstream release and applies
  > the patches in lexical order (`0001` upstream symlink-listing fix, then
  > `0002` posixmeta metadata). A fresh clone therefore needs `go` and `patch`,
  > nothing else.
* **Local rclone fork (`third_party/rclone`):** `go.mod` has
  `replace github.com/rclone/rclone => ./third_party/rclone` (rclone v1.76.0).
  The fork carries the VFS/FUSE patches which are not upstream yet:
  * `vfs/` + `cmd/mount/` symlink listing (patch `0001`, an upstream bug fix):
    `Dir.Symlink` creates the link through the receiver via the new
    `(*vfs.Dir).CreateSymlink`, and `VFS.CreateSymlink` is now a thin wrapper
    over it, instead of re-resolving the parent with `VFS.StatParent`. Before
    the fix, after a directory cache forget the link landed on a different
    `*vfs.Dir` instance and only showed up in `ls` once `--dir-cache-time`
    expired (`docs/adr/0007-mount-symlink-listing.md`).
  * `vfs/`: `File`/`Dir` gained `Metadata`/`SetMetadata` only — the public
    `VFS.Chmod`/`VFS.Chown` API is untouched (still `ENOSYS`, as upstream), so
    other VFS users such as `rclone serve nfs` are unaffected. Pending metadata
    is queued while a file is still being written and applied in `setObject`;
    freshly `Mkdir`'d (synthetic) directory entries are refreshed from the
    backend before metadata is read or written.
  * `cmd/mount/`: `Attr` overlays per-entry `mode`/`uid`/`gid` from
    `fs.Metadataer` onto the mount-wide defaults, and `Setattr` turns kernel
    `chmod`/`chown` into `SetMetadata` on the node (silently ignored with
    ENOSYS when the backend doesn't support metadata).
    `Create`/`Mkdir`/`Mknod`/`Symlink` stamp
    the new entry from the FUSE request like a regular filesystem: the creating
    process's `uid`/`gid` (setgid directories force their group) and the
    kernel-provided `mode` (already masked by the caller's umask), with
    `S_ISGID` inherited onto new subdirectories only
    (`docs/adr/0005-create-ownership-from-fuse-request.md`,
    `docs/adr/0011-create-mode-from-fuse-request.md`). All paths are gated
    on the `--metadata-push` mount option (default off) to preserve stock
    rclone behaviour for `local`/`crypt`/cloud mounts. Symlink nodes keep their
    mode from `--link-perms` (`S_IFLNK` plus the link permissions) while
    `uid`/`gid` are still recorded and overlaid.
  * `cmd/mountlib/`: `--metadata-push` option and a preflight that rejects a
    backend implementing `RequiresMetadataPush() bool` when the flag is
    absent; `posixmeta` returns true, so it cannot be mounted in a silently
    degraded state. `--metadata-permissions` (default true) makes
    `cmd/mount/mount.go` pass `default_permissions` whenever
    `--metadata-push` is set, so the kernel enforces the reported
    `mode`/`uid`/`gid` (`docs/adr/0006-kernel-permission-enforcement.md`);
    `--metadata-permissions=false` restores the permissive stock behaviour.
  Keep patches minimal and upstream-compatible so the fork can be refreshed
  (regeneration instructions: `third_party/README.md`); re-run
  `test/suites/02-mount-crypt.sh` after any rclone version bump. Unified diffs
  against pristine v1.76.0 are kept in `third_party/patches/` and applied in
  lexical order: `0001-mount-symlink-listing.patch` (upstream fix, applied
  first) and `0002-posixmeta-metadata.patch` (posixmeta overlay, diffed against
  pristine + `0001`); see also
  `docs/adr/0001-fuse-chmod-chown-propagation.md`,
  `docs/adr/0002-fuse-symlinks-via-vfs-links.md` and
  `docs/adr/0007-mount-symlink-listing.md`.

### Known Benign Log Message

When running `./rclone-posixmeta`, rclone may emit:

```text
ERROR internal error: no overview data found for "posixmeta"
```

This occurs because rclone's embedded `docs/data` overview lookup table is generated at upstream release time from built-in backends. It is non-fatal and does not affect backend execution, VFS mounts, or metadata operations. Do **not** treat this message as a test failure unless accompanied by a non-zero exit code or functional error.

## 3. Design Invariants & Critical Rules

Agents modifying `backend/posixmeta/posixmeta.go` **must** preserve the following invariants:

1. **Sparse Sidecars Only (`sidecar_layout = sparse`, the default):**
   * A `<dir>/.posixmeta.<entry>` file must **only** be written when at least one attribute (`mode`, `uid`, `gid`, `symlink`, `btime`) differs from the effective default (`default_file_mode` / `default_dir_mode`, `default_uid` / `default_gid`, or inherited ancestor `uid`/`gid` when `inherit_parent_owner = true`).
   * If an update reverts all attributes back to defaults/inherited values, `buildSparseSidecar` returns `nil` and the sidecar must be deleted from the wrapped remote.
2. **Zero `atime` / `ctime` Write Churn:**
   * Never persist `atime` or `ctime` into `.posixmeta` sidecars; `SetMetadata` explicitly ignores them so file reads do not trigger remote sidecar uploads.
3. **Sidecar Hiding & Lifecycle Coupling:**
   * When `hide_sidecars = true` (default), `List` and `NewObject` must filter out any path whose last component is `sidecar_prefix` or starts with `<sidecar_prefix>.`, and `Put` must reject direct writes to sidecar paths. Sidecars are named `<prefix>.<name>` next to their entry (`dir/file` -> `dir/.posixmeta.file`). The one exception is the remote's own root (an `Fs` with no subpath, e.g. `remote = raw:/srv/store` mounted as `pm:`): it has no name, so its sidecar is the bare prefix *inside* it (`/srv/store/.posixmeta`), never in `/srv`. A subpath root keeps the normal form (`pm:dir` -> `<base>/.posixmeta.dir`) (`docs/adr/0009-sidecar-prefix-naming.md`).
   * `Move`, `DirMove`, `Remove`, `Rmdir`, and `Purge` must keep sidecars synchronized (moving or deleting `.posixmeta.<entry>` alongside `<entry>`, never deleting a sidecar that stores a proxied symlink during orphan cleanup).
   * With `sidecar_layout = dir` the reserved namespace is the bare `<prefix>`
     (the directory map), `<prefix>.tmp` (its staging object) and
     `<prefix>.corrupt.*` (repair quarantines). All of them are hidden,
     `Put` rejects them, and a listing that shows `<prefix>.<name>` is a layout
     mismatch error rather than an entry (invariant 11).
4. **Coalesced Writeback, Bounded Cache & Flush Safety:**
   * Rapid metadata updates are coalesced in memory via `writeback_delay` and
     flushed by a pool of `writeback_workers` goroutines. Timers only enqueue a
     path; the `f.pending` entry is the source of truth and is deleted **only
     after** a successful write, so failures are retried with exponential
     backoff (`writeback_retry_max`) instead of being dropped.
   * At most one write per path is in flight (`f.inflight` + per-path
     generation); a superseding update is re-queued when the older write
     finishes, so a stale snapshot can never land after a newer one.
   * `f.pending` is bounded by `pending_max_entries`; on overflow the update is
     persisted synchronously by the caller (never dropped).
   * `CleanUp`, `backend setmeta` and `fs.Shutdowner` (`Shutdown`) flush pending
     writes. `rclone mount` never evicts its `Fs`, so `NewFs` also registers one
     `lib/atexit` handler (`flushLiveFs`) which flushes every live `Fs` with
     queued updates on normal exit and on SIGINT/SIGTERM.
   * Destructive or replacing operations (`Remove`, `Rmdir`, `Purge`, `Move`,
     `DirMove`, `Copy`, `Mkdir`) must call `quiesceAbs`/`quiescePrefix` (cancel
     queued updates **and** wait out in-flight writes) so a late writeback cannot
     resurrect a deleted sidecar or overwrite new metadata.
   * The read-through sidecar cache must stay bounded: `cache_max_entries`
     (default 65536) and `cache_max_size` (default 64 MiB, approximate) evict
     least recently used entries, expired entries are dropped on access instead of
     accumulating, and every insert/remove keeps `f.cache`, `f.cacheLRU` and
     `f.cacheBytes` consistent. Eviction must never change observable metadata —
     it is a pure cache, and `f.pending` remains the source of truth for
     unflushed updates.
   * Concurrent cold reads of the same sidecar are coalesced (singleflight), and
     sidecar reads are capped at 1 MiB; corrupt/oversized/future-schema sidecars
     are counted, logged and treated as absent (never fatal).
   * `rclone backend cachestats pm:` reports occupancy (`entries`, `bytes`, caps,
     `pending_writes`, `inflight_writes`) and health (`hits`/`misses`/
     `evictions`, `written`, `failed_writes`, `retried_writes`,
     `overflow_writes`, `last_write_error`). A standalone CLI process always
     starts with an empty cache; to observe a live mount's cache, start the mount
     with `--rc` and call `rclone rc backend/command command=cachestats fs=pm:`
     (the mount's `fs/cache` entry is reused).
   * With `sidecar_layout = dir` the debounce queue, the in-flight set and the
     generation counters are keyed by the *directory map* instead of the entry
     path (`writeUnit`), and `cachestats` additionally reports `layout`, `maps`,
     `maps_bytes`, `corrupt_maps` and `dirty_maps`. `pending.data` is unused
     because the map state is the source of truth: cancelling a timer only
     postpones a flush, and `flushAllPending` scans dirty maps rather than the
     queue (invariant 11).
5. **Crypt Password Obscuring:**
   * Any `rclone.conf` created for automated tests using `type = crypt` **must** obscure `password` and `password2` via `./rclone-posixmeta obscure <plaintext>` before writing the config file. Plaintext passwords cause `input too short when revealing password - is it obscured?`.
6. **Root-Boundary Sidecar Resolution:**
   * `NewFs` must mirror wrapped backends that support file roots: when the wrapped remote returns `fs.ErrorIsFile`, return an `Fs` rooted at the file's parent and propagate `fs.ErrorIsFile` (so `rclone moveto pm:a.txt pm:b.txt` works).
   * Sidecar paths are identity-mapped through `absRemote` (`f.root + "/" + remote`). All sidecar reads/writes/moves/deletes and ancestor-owner lookups use `f.base` (the wrapped remote opened at the `remote` option root) so the sidecars that fall outside the rooted `f.wrapped` remain reachable from subpath filesystems: the root entry's own sidecar (`<parent>/.posixmeta.<root>`, which for a subpath root lives outside the directory it describes) and ancestor-owner sidecars (`<parent>/.posixmeta.<ancestor>`). The remote root itself (`f.root == ""`) has no name and keeps its attributes in the bare prefix inside the wrapped root. Payload I/O keeps using `f.wrapped` (rooted for performance).
   * `Rmdir`, `Purge`, and `DirMove` of a filesystem root must remove/move the root entry's own sidecar: for a subpath root it lives in the parent directory (`<parent>/.posixmeta.<root>`), for the remote root it is the bare prefix inside it.
7. **Opt-in FUSE metadata:**
   * `rclone mount` must not read or write POSIX metadata unless
     `--metadata-push` is set. Without it, `stat`/`getattr` must keep using
     `--file-perms`/`--dir-perms`/`--uid`/`--gid` and `chmod`/`chown` must remain
     no-ops for **all** backends (stock rclone behaviour).
   * With `--metadata-push`, `create`/`mkdir`/`mknod`/`symlink` record the
     creating process's `uid`/`gid` on the new entry (the group comes from the
     parent directory when it has `S_ISGID`) and the kernel-provided `mode`
     (already masked by the caller's umask), like a regular filesystem, instead
     of leaving it to `default_uid`/`default_gid`, `inherit_parent_owner`,
     `--file-perms`/`--dir-perms` or `default_file_mode`/`default_dir_mode`.
     `S_ISGID` is copied to new **subdirectories** only (never to files), so
     setgid directories stay setgid and their children keep their group.
     Symlink nodes record ownership only (`--link-perms` keeps the mode).
     See `docs/adr/0005-create-ownership-from-fuse-request.md` and
     `docs/adr/0011-create-mode-from-fuse-request.md`.
     `default_*_mode` remains the resolved mode (and the sparseness baseline)
     for entries without metadata: `rclone copy`/`sync`, other clients,
     pre-existing trees. With `umask 022` a create therefore still writes no
     sidecar, because the stamped mode equals the default.
   * `inherit_parent_owner` therefore applies to entries without recorded
     credentials: `rclone copy`/`sync`, objects written before the flag existed,
     and entries whose owner was written through the mount are unaffected by a
     later directory `chown`.
   * `posixmeta` implements `RequiresMetadataPush() bool` returning true, so a
     mount without the flag fails fast with an actionable error instead of
     silently ignoring sidecars.
   * With `--metadata-push` the kernel must **enforce** the reported
     `mode`/`uid`/`gid`: `cmd/mount` passes `default_permissions` unless
     `--metadata-permissions=false`. Without it `rclone mount` never implements
     `FUSE_ACCESS`, so any user allowed by `--allow-other` could traverse a
     `0700` directory or read a `0600` file of another owner (see
     `docs/adr/0006-kernel-permission-enforcement.md`). Reporting metadata that
     is not enforced is a bug, not a preference.
8. **Symlinks via upstream `--vfs-links`:**
   * By default a FUSE symlink is one `<name>.rclonelink` object whose payload
     is the target (see `docs/adr/0002-fuse-symlinks-via-vfs-links.md`). No
     `.posixmeta` sidecar is created for a plain link, except for the sparse
     ownership sidecar written when the link's creator differs from the owner it
     would inherit (`docs/adr/0005-create-ownership-from-fuse-request.md`).
   * With `proxy_links = true` the link is stored entirely in the entry's
     sidecar (`<dir>/.posixmeta.<name>` with `symlink` set, no payload), and
     `List`/`ListR`/`NewObject` must emulate the `<name>.rclonelink` object from
     the sidecar (`docs/adr/0008-proxy-link-files.md`). Link sidecar writes are
     synchronous (a failed write must fail `ln -s`), targets are bounded to
     64 KiB and validated (non-empty, no NUL), `Remove`/`Move`/`Copy` keep the
     sidecar in sync, renaming to a non-link name materialises the target as a
     regular file, and `NewFs` mirrors `fs.ErrorIsFile` for a link root. A real
     payload always wins over a sidecar `symlink`; a real `<name>.rclonelink`
     payload is shadowed only when the sidecar for `<name>` has a target.
   * For symlink nodes the mount must not overlay the backend `mode`
     (`--link-perms`/`S_IFLNK` wins because Linux ignores `chmod` on links),
     but must still record and overlay `uid`/`gid`.
   * `--vfs-links` is deliberately opt-in and has no mount preflight: without
     it `ln -s`/`readlink` fail loudly, and forcing it would reinterpret
     literal `*.rclonelink` objects and break existing mounts.
9. **Listing-driven cache priming:**
   * `List` must derive sidecar presence from the raw listing it already has: key
     every non-sidecar entry as absent for free (`primeNegative`), and prefetch
     the sidecars that do exist with `prefetch_workers` goroutines (capped at
     `64*prefetch_workers` entries and 30 s per listing). This is what keeps a
     `stat` storm after a directory read from costing one remote read per entry.
   * `ListR` must delegate to the wrapped `ListR` (falling back to a local
     recursion) and must **not** negative-prime: batched recursive callbacks can
     split a directory, so "no sibling sidecar in this batch" is not proof of
     absence (see `docs/durability-and-scale.md`, residual 6).
   * With `sidecar_layout = dir` there is nothing per entry to prime: `List`
     reads the directory's map exactly once (a cache hit after the first read)
     and `ListR` loads the map of every directory it touches, so neither needs
     prefetch workers. Proxied symlinks are derived from the map instead of
     per-entry sidecar reads.
10. **Wrapped-feature delegation and overwrite policy:**
   * Forward `Copy`, `ListR`, `ChangeNotify`, `MkdirMetadata`, `PutUnchecked`,
     `About`, `DirCacheFlush` (+ `SlowModTime`/`NoMultiThreading`) to the wrapped
     backend, keeping sidecars in sync (`Copy` copies/removes the destination
     sidecar; `MkdirMetadata` routes POSIX keys to the sidecar).
   * Only advertise `Copy`, `ChangeNotify` and `About` when the wrapped backend
     implements them, so callers fall back to their generic paths.
   * `PartialUploads` must **not** be advertised: rclone's `.partial` staging name
     would bypass the "reject writes to a sidecar path" guard.
   * `overwrite_metadata = preserve` (default) keeps the previous sidecar across
     an overwrite unless the upload carries POSIX metadata; `reset` drops it. Do
     not change the default: the VFS re-upload of a file carries no metadata, so
     `reset` would lose `chmod`/`chown` on every write through the mount. See
     `docs/durability-and-scale.md` and
     `docs/adr/0004-durable-writeback-and-delegation.md`.
11. **Optional per-directory aggregate map (`sidecar_layout = dir`):**
   * The document is `{"v":2,"crc":"<8 hex>","entries":{…}}` stored as the bare
     `<prefix>` **inside** the directory it describes. The checksum is the
     CRC-32 (IEEE) of the canonical (sorted-key) JSON of `entries`, written as 8
     lowercase hex digits; keys are the directory's entry names, and the
     reserved key `"."` holds the base root entry's own attributes. A child's
     attributes always live in its *parent's* map, so `pm:dir` reads its own
     attributes from `<base>/.posixmeta`'s `dir` key via `f.base`.
   * Schema version is the sole layout discriminator: v1 = sparse, v2 = dir. A v1
     document under `dir` (or a v2 directory map document under `sparse`) must fail
     `NewFs` with an actionable error, and a listing that shows the other
     layout's objects must fail the operation (`refusing to mix layouts`).
     Never convert, never ignore, never delete the other layout's data.
   * The root map is created once (empty) on the first metadata write and is
     never deleted: it is the layout declaration. An empty non-root map is
     deleted instead of written.
   * **Fail closed.** Unparsable JSON, checksum mismatch, unknown schema,
     `map_max_entries` or `map_max_bytes` overflow ⇒ reads error for that
     directory, mutations are refused, and the object is never rewritten.
     `on_corrupt = warn` logs it and serves defaults but still refuses writes.
     Degrading silently is a bug.
   * The map is the unit of caching and writeback: TTL/LRU bounded
     `mapState` per directory, single-flight loads, one in-flight whole-map
     write per map (`mapState.wmu` + generation), staging `<prefix>.tmp` plus a
     server-side `Move` when available, and a no-op write when nothing changed.
     Dirty maps are never evicted, reloaded or dropped by invalidation; a reload
     refreshes the existing state in place so in-flight updates keep working.
   * The engine keys `pending`/`inflight` by `writeUnit(abs)` (the map directory
     in this layout). `quiesceAbs`/`quiescePrefix` cancel timers and wait for an
     in-flight write; `Rmdir` removes the directory's own map object first (it
     would otherwise make the directory non-empty), restores it if the removal
     fails, and `Purge`/`DirMove` discard the subtree's map states.
   * `rclone backend repair` is the only supported recovery: dry run unless
     `-o dry_run=false -o confirm=REPAIR`, quarantine `<prefix>.corrupt.<ts>`
     before writing, salvage or reset, drop invalid entries, optionally remove
     orphan keys, remove stale `<prefix>.tmp`. It never converts layouts.
   * `proxy_links` stores the target in the entry's map key and
     `List`/`ListR`/`NewObject` synthesise the `<name>.rclonelink` object from
     it. `sparse` remains the default; do not change it.
   * Design notes and residuals: `docs/adr/0010-per-directory-aggregate-sidecars.md`.

## 4. Automated Verification Procedures

Always run tests inside isolated temporary directories (`/tmp/pm-*`) with an explicit `--config` and `--cache-dir` so user state (`~/.config/rclone` and `~/.cache/rclone`) is never touched. Always unmount FUSE mounts and delete `/tmp/pm-*` after each test.

### Test Harness (preferred entry point)

The shell procedures below are implemented as the repository harness; prefer it
over running the snippets by hand. It builds the binary, isolates every test
(own process, `HOME`, config, cache and scratch tree), unmounts FUSE filesystems
via traps, and reports `PASS`/`FAIL`/`SKIP`/`XFAIL`/`XPASS`.

```bash
./test/run.sh                    # go unit tests + CLI + mount/crypt
./test/run.sh 01-cli             # suite by name
./test/run.sh --list             # enumerate tests
./test/run.sh --filter 'move'    # substring/regex selection
./test/run.sh --tap              # TAP-13 output for CI
./test/run.sh --strict-xpass     # fail if a known gap is fixed
```

* Runner: `test/run.sh`
* Framework: `test/lib/harness.sh`
* Suites: `test/suites/00-go-unit.sh`, `01-cli.sh` (Procedure A), `02-mount-crypt.sh` (Procedure B)
* Both layouts are covered: `TestDirLayout*` (`backend/posixmeta/dirmap_test.go`),
  `test_cli_dir_layout_*` (`01-cli.sh`, using `PM_CFG_OPTS[sidecar_layout]=dir`
  and `pm_dir_map_path`) and `test_mount_dir_layout_roundtrip`
  (`02-mount-crypt.sh`).
* Go unit tests: `backend/posixmeta/posixmeta_test.go`
* Documentation: `test/README.md`

No tests are currently `XFAIL`. `XPASS` means a test marked with `xfail` now
passes: make it fatal in CI with `--strict-xpass`, then remove the `xfail` call.

### Procedure A: Non-Mount Unit/CLI Verification (Sidecars, Inheritance, Moves, Deletes)

Verifies sparse sidecar creation, hiding, `uid`/`gid` inheritance from parent directories, move propagation, and cleanup.

```bash
set -euo pipefail
rm -rf /tmp/pm-cli-test
mkdir -p /tmp/pm-cli-test/src /tmp/pm-cli-test/in

cat <<'EOF' > /tmp/pm-cli-test/rclone.conf
[raw_local]
type = local

[pm_local]
type = posixmeta
remote = raw_local:/tmp/pm-cli-test/src
default_file_mode = 0644
default_dir_mode = 0755
inherit_parent_owner = true
sidecar_prefix = .posixmeta
hide_sidecars = true
cache_ttl = 5m
cache_max_entries = 65536
cache_max_size = 64M
writeback_delay = 50ms
EOF

# 1. Upload default file -> no sidecar should be created
printf "hello\n" > /tmp/pm-cli-test/in/default.txt
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf copyto /tmp/pm-cli-test/in/default.txt pm_local:default.txt
test ! -e /tmp/pm-cli-test/src/.posixmeta.default.txt

# 2. Set directory owner -> child inherits uid/gid without needing its own sidecar
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf mkdir pm_local:proj
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf backend setmeta pm_local: proj -o uid=2001 -o gid=2002
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf copyto /tmp/pm-cli-test/in/default.txt pm_local:proj/inherited.txt
test ! -e /tmp/pm-cli-test/src/proj/.posixmeta.inherited.txt
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf lsjson --metadata pm_local:proj/inherited.txt | grep -q '"uid":"2001"'

# 3. Override mode on child -> sparse sidecar created, hidden in ls, moved with file
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf backend setmeta pm_local: proj/inherited.txt -o mode=0750
test -e /tmp/pm-cli-test/src/proj/.posixmeta.inherited.txt
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf moveto pm_local:proj/inherited.txt pm_local:proj/renamed.txt
test ! -e /tmp/pm-cli-test/src/proj/.posixmeta.inherited.txt
test -e /tmp/pm-cli-test/src/proj/.posixmeta.renamed.txt

rm -rf /tmp/pm-cli-test
echo "PASS: Procedure A"
```

### Procedure B: End-to-End `rclone mount` over `crypt` Verification

This is the primary integration test for the target production topology (`local` -> `crypt` -> `posixmeta` -> `FUSE mount`).

#### Critical VFS / Mount Testing Notes:

* Always pass `--cache-dir /tmp/pm-crypt-mount/vfs-cache` so VFS writeback state is isolated from `~/.cache/rclone`.
* Always pass `--vfs-cache-mode writes --vfs-write-back 200ms --dir-cache-time 500ms` (or wait ≥ 1s after closing a file) so VFS flushes dirty files to the wrapped `crypt` remote before asserting on backing files or `lsjson --metadata`.
* Always register an `EXIT` trap to unmount `/tmp/pm-crypt-mount/mnt` (`fusermount -u` or `umount`) so failed assertions never leave stale FUSE mounts behind.

```bash
set -euo pipefail

TMPDIR="/tmp/pm-crypt-mount"
MNT="$TMPDIR/mnt"
RAW="$TMPDIR/raw"
VFS="$TMPDIR/vfs-cache"
CONF="$TMPDIR/rclone.conf"

cleanup() {
  fusermount -u "$MNT" 2>/dev/null || umount "$MNT" 2>/dev/null || true
  rm -rf "$TMPDIR"
}
trap cleanup EXIT

rm -rf "$TMPDIR"
mkdir -p "$RAW" "$MNT" "$VFS"

OBS1=$(./rclone-posixmeta obscure "test-crypt-pass-1")
OBS2=$(./rclone-posixmeta obscure "test-crypt-salt-2")

cat <<EOF > "$CONF"
[raw_store]
type = local

[enc_store]
type = crypt
remote = raw_store:$RAW
password = $OBS1
password2 = $OBS2

[pm_crypt]
type = posixmeta
remote = enc_store:
default_file_mode = 0644
default_dir_mode = 0755
inherit_parent_owner = true
sidecar_prefix = .posixmeta
hide_sidecars = true
cache_ttl = 5m
cache_max_entries = 65536
cache_max_size = 64M
writeback_delay = 50ms
EOF

# 1. Mount posixmeta(crypt(local)) in background
./rclone-posixmeta --config "$CONF" mount pm_crypt: "$MNT" \
  --cache-dir "$VFS" \
  --metadata-push \
  --vfs-links \
  --vfs-cache-mode writes \
  --vfs-write-back 200ms \
  --dir-cache-time 500ms \
  --daemon

sleep 1

# 2. Create directory and file through FUSE mount
mkdir -p "$MNT/secret_dir"
printf "encrypted payload\n" > "$MNT/secret_dir/notes.txt"
sleep 1

# 3. Tag the file with a mode and btime (ownership is still inherited from
#    the directory, which is owned by the mount user at this point)
./rclone-posixmeta --config "$CONF" backend setmeta pm_crypt: secret_dir/notes.txt \
  -o mode=0600 \
  -o btime="2026-11-22T08:00:00Z"

sleep 1

# 4. Verify mount visibility (no .posixmeta sidecar leaked into mount directory)
test -f "$MNT/secret_dir/notes.txt"
test ! -e "$MNT/secret_dir/.posixmeta.notes.txt"
test "$(cat "$MNT/secret_dir/notes.txt")" = "encrypted payload"

# 5. Hand the directory to another user, then verify metadata resolution and
#    parent-owner inheritance via lsjson --metadata. The mount enforces the
#    reported permissions (--metadata-permissions, on by default with
#    --metadata-push), so the directory stays world-writable: a 0755 directory
#    owned by 1050 would make every later write below fail with EACCES.
#    notes.txt keeps its sparse creator stamp, so it now inherits 1050/1060.
./rclone-posixmeta --config "$CONF" backend setmeta pm_crypt: secret_dir \
  -o uid=1050 \
  -o gid=1060 \
  -o mode=0777

sleep 1

META_JSON=$(./rclone-posixmeta --config "$CONF" lsjson --metadata pm_crypt:secret_dir)
echo "$META_JSON" | grep -q '"Name":"notes.txt"'
echo "$META_JSON" | grep -q '"mode":"0600"'
echo "$META_JSON" | grep -q '"uid":"1050"'
echo "$META_JSON" | grep -q '"gid":"1060"'
echo "$META_JSON" | grep -q '"btime":"2026-11-22T08:00:00Z"'

# 6. Verify at-rest encryption on raw storage (no plaintext filenames or sidecar suffixes)
if find "$RAW" \( -name '*notes*' -o -name '.posixmeta' -o -name '.posixmeta.*' \) | grep -q .; then
  echo "FAIL: Plaintext filename or unencrypted .posixmeta leaked into raw storage"
  exit 1
fi

# 7. Verify file rename inside FUSE mount preserves metadata & moves encrypted sidecar
mv "$MNT/secret_dir/notes.txt" "$MNT/secret_dir/notes_renamed.txt"
sleep 1

META_RENAMED=$(./rclone-posixmeta --config "$CONF" lsjson --metadata pm_crypt:secret_dir)
echo "$META_RENAMED" | grep -q '"Name":"notes_renamed.txt"'
echo "$META_RENAMED" | grep -q '"mode":"0600"'
echo "$META_RENAMED" | grep -q '"uid":"1050"'

# 8. Verify deletion inside FUSE mount removes encrypted sidecar as well.
#    rm -f: the 0600 file now belongs to another user, so plain rm would ask
#    for confirmation; unlinking only needs the directory's write permission.
rm -f "$MNT/secret_dir/notes_renamed.txt"
sleep 1
test -z "$(./rclone-posixmeta --config "$CONF" ls pm_crypt:secret_dir)"

# 9. Verify FUSE chmod/chown propagation and getattr reporting.  Changing only
#    the group needs a group the caller belongs to (CAP_CHOWN is not required).
printf "mode payload\n" > "$MNT/secret_dir/mode.txt"
sleep 1
chmod 0700 "$MNT/secret_dir/mode.txt"
CHGID=$(id -G | tr ' ' '\n' | awk -v cur="$(id -g)" '$1 != cur { print; exit }')
if [[ -n $CHGID ]]; then
  chgrp "$CHGID" "$MNT/secret_dir/mode.txt"
fi
sleep 1
test "$(stat -c '%a' "$MNT/secret_dir/mode.txt")" = "700"
if [[ -n $CHGID ]]; then
  test "$(stat -c '%g' "$MNT/secret_dir/mode.txt")" = "$CHGID"
fi
META_MODE=$(./rclone-posixmeta --config "$CONF" lsjson --metadata pm_crypt:secret_dir)
echo "$META_MODE" | grep -q '"Name":"mode.txt"'
echo "$META_MODE" | grep -q '"mode":"0700"'
if [[ -n $CHGID ]]; then
  echo "$META_MODE" | grep -q "\"gid\":\"$CHGID\""
fi

# 10. Verify symlinks via --vfs-links: one backend object holding the target,
#     symlink type from --link-perms and the creating process as owner. The link
#     gets a sparse ownership sidecar because secret_dir is owned by 1050.
RAW_BEFORE=$(find "$RAW" -type f | wc -l | tr -d ' ')
ln -s mode.txt "$MNT/secret_dir/link.txt"
sleep 1
test -L "$MNT/secret_dir/link.txt"
test "$(readlink "$MNT/secret_dir/link.txt")" = "mode.txt"
test "$(stat -c '%F' "$MNT/secret_dir/link.txt")" = "symbolic link"
test "$(stat -c '%u' "$MNT/secret_dir/link.txt")" = "$(id -u)"
./rclone-posixmeta --config "$CONF" lsjson pm_crypt:secret_dir |
  grep -q '"Name":"link.txt.rclonelink"'
test "$(find "$RAW" -type f | wc -l | tr -d ' ')" = "$((RAW_BEFORE + 2))"

echo "PASS: Procedure B (mount + crypt)"
```

## 5. Roadmap / Next Areas for Improvement

### 1. Native Kernel FUSE `chmod` / `chown` / `stat` Propagation — Fixed

Implemented in the local rclone fork (`third_party/rclone`, see section 2):

* **`vfs/file.go` & `vfs/dir.go`:** `File`/`Dir` expose `Metadata(ctx)`,
  `SetMetadata(ctx, fs.Metadata)`.
  * `File.SetMetadata` queues metadata in `pendingMetadata` while the file is
    still being written (`o == nil` or open writers) and applies it from
    `setObject` once the upload has produced the real object. `Metadata`
    overlays pending values so getattr is correct immediately.
  * `Dir.backendEntry` re-reads the `fs.Directory` from the backend when the
    VFS holds a synthetic `fs.NewDir` (freshly `Mkdir`'d or renamed
    directories); without this, `chmod` right after `mkdir` silently no-ops.
  * `VFS.Chmod`/`VFS.Chown` are deliberately left as upstream (`ENOSYS`), so
    `rclone serve nfs` and other VFS users are unaffected by the fork. The
    mount path does not use them: `cmd/mount` calls `SetMetadata` on the node.
* **`cmd/mount/file.go` & `cmd/mount/dir.go`:** `Attr` reads `Metadata` and
  overlays `mode` (including setuid/setgid/sticky), `uid` and `gid` onto the
  mount-wide `--file-perms`/`--dir-perms`/`--uid`/`--gid` defaults. `Setattr`
  forwards `req.Valid.Mode()`, `req.Valid.Uid()` and `req.Valid.Gid()` as
  `SetMetadata`, swallowing `ENOSYS` to preserve the legacy no-op behaviour
  for backends without metadata. Both paths require `--metadata-push`.
* **`cmd/mountlib/mount.go`:** `--metadata-push` (default off) plus a
  preflight rejecting backends implementing `RequiresMetadataPush() bool`
  (`posixmeta`) when the flag is missing.
* **Creation metadata (`cmd/mount/posixmeta.go`, `cmd/mount/dir.go`):**
  `Create`, `Mkdir`, `Mknod` and `Symlink` stamp the new entry from the FUSE
  request when `--metadata-push` is set: the creating process's `uid`/`gid`
  (the parent's group is used when the parent directory has `S_ISGID`) and the
  kernel-provided `mode`, which the kernel has already masked with the caller's
  umask (bazil/fuse never negotiates `FUSE_DONT_MASK`). `S_ISGID` is copied onto
  new subdirectories only, exactly like `inode_init_owner()` on ext4, so
  `mkdir -m`, umask and setgid directories behave normally
  (`docs/adr/0005-create-ownership-from-fuse-request.md`,
  `docs/adr/0011-create-mode-from-fuse-request.md`). Symlinks record ownership
  only (`--link-perms` keeps the mode).
* **Kernel permission enforcement (`cmd/mount/mount.go`):** with
  `--metadata-push`, `default_permissions` is passed so the kernel checks the
  overlaid `mode`/`uid`/`gid` on path traversal, open/read/write and directory
  modification. `--metadata-permissions` (default true) is the opt-out; without
  enforcement, `cmd/mount` never implements `FUSE_ACCESS` and reported
  ownership is cosmetic
  (`docs/adr/0006-kernel-permission-enforcement.md`).

Covered by `02-mount-crypt` tests
`test_mount_chmod_via_fuse_is_forwarded`,
`test_mount_chown_via_fuse_is_forwarded`,
`test_mount_getattr_reflects_sidecar_metadata`,
`test_mount_create_records_creating_process`,
`test_mount_create_in_setgid_dir_inherits_group`,
`test_mount_create_honours_requested_mode`,
`test_mount_enforces_recorded_permissions`,
`test_mount_requires_metadata_push`,
`test_mount_without_metadata_push_keeps_stock_behaviour` and
`test_mount_metadata_push_is_opt_in`, plus Procedure B step 9. The
`proxy_links` path is covered by `TestProxyLink*`
(`backend/posixmeta/link_test.go`), `test_cli_proxy_links_roundtrip`,
`test_cli_sidecar_prefix_layout` and
`test_mount_proxy_links_sidecar_only`.

Known limitations:

* The `cmount`/`mount2` path (`cmd/cmount/`) is not patched; use `rclone mount`.
* `chown` of the mount root is not propagated: the VFS root holds a synthetic
  directory entry with no backend counterpart.
* Entries created through the mount keep the caller's mode/umask and inherit
  `S_ISGID` onto new subdirectories, but entries written by `rclone copy`/`sync`
  or another client carry no creator: they still resolve to
  `default_file_mode`/`default_dir_mode` and never propagate `S_ISGID`.
* An entry created while its owner already equalled the inherited value and
  whose mode equalled the default stays sparse and therefore still follows a
  later `chown`/`chmod` policy change of an ancestor directory; only entries
  with their own sidecar keep their recorded values.
* Metadata changed by another process is invisible until `cache_ttl` expires
  in the mount process (same as any backend cache), so a `backend setmeta`
  `chown`/`chmod` is enforced only once the mount refreshes its cached attrs.
* `--metadata-permissions=false` deliberately skips the access checks again
  (stock rclone behaviour); it does not disable the metadata overlay itself.
* `sudo` needs `--allow-other`: a FUSE mount only accepts requests whose
  uid/gid match `user_id=`/`group_id=` in `/proc/self/mounts`, so root gets
  `EACCES` from `fuse_setattr()` before the daemon runs, and `sudo
  chmod`/`sudo chown` fail with `Operation not permitted`/`Permission denied`
  on a normally created mount. `--metadata-permissions=false` does not help
  (`chmod` still requires ownership or `CAP_FOWNER`); the remedies are
  `--allow-other` (+ `user_allow_other`), `backend setmeta`, or mounting as
  root.

### 2. Symlink Support in FUSE Mount — Fixed

Uses the upstream `.rclonelink` model (design A in
`docs/adr/0002-fuse-symlinks-via-vfs-links.md`):

* Mount with `--vfs-links` (or the global `--links`). `ln -s` stores one
  `<name>.rclonelink` object whose payload is the target; `readlink`, `stat`,
  rename, delete and remount work through the VFS. The backend needed no
  change; the VFS only needed the listing fix below.
* Creating a link through the mount also needs `0001-mount-symlink-listing.patch`:
  upstream `cmd/mount` created it via `VFS.CreateSymlink`, which re-resolves the
  parent and can therefore return a second `*vfs.Dir` instance after a
  directory cache forget, leaving the new link unlisted until
  `--dir-cache-time` expired again. `Dir.Symlink` now uses the new
  `(*vfs.Dir).CreateSymlink` on its own node
  (`docs/adr/0007-mount-symlink-listing.md`). Covered by
  `test_mount_symlink_listed_immediately` and
  `test_mount_symlink_listed_after_dir_cache_expiry`.
* `posixmeta` over `crypt` keeps that as a single encrypted object: no
  plaintext link name/target at rest and no `.posixmeta` sidecar for a plain
  link. With `proxy_links = true` the target is stored in the sidecar instead
  (`docs/adr/0008-proxy-link-files.md`), so a link is still one encrypted
  object; `test_mount_proxy_links_sidecar_only` covers creation, `readlink`,
  rename, delete and remount over `crypt`.
* The fork's `Attr` overlay skips `mode` for symlink nodes so `--link-perms`
  (plus `S_IFLNK`) wins, while `uid`/`gid` (including parent-owner inheritance)
  are still applied.
* `--vfs-links` is opt-in with **no** mount preflight: without it `ln -s`
  fails with `ENOSYS` rather than silently misbehaving. Covered by
  `test_mount_without_vfs_links_rejects_symlink` and
  `test_mount_symlinks_via_vfs_links`.
* The separate `"symlink"` sidecar attribute remains available via
  `backend setmeta`/`lsjson --metadata`; set `proxy_links = true` to make the
  FUSE path consume it instead of the `.rclonelink` payload.

### 3. Production `pcloud` + `crypt` + `posixmeta` Stack

* Once local `mount + crypt` tests pass completely, configure the production stack in `~/.config/rclone/rclone.conf`:
  ```ini
  [pcloud_crypt]
  type = crypt
  remote = pcloud:<encrypted-folder>
  password = <obscured-password>
  password2 = <obscured-salt>

  [pcloud_posix]
  type = posixmeta
  remote = pcloud_crypt:
  default_file_mode = 0644
  default_dir_mode = 0755
  inherit_parent_owner = true
  sidecar_prefix = .posixmeta
  hide_sidecars = true
  cache_ttl = 5m
  cache_max_entries = 65536
  cache_max_size = 64M
  writeback_delay = 500ms
  writeback_workers = 4
  writeback_retry_max = 5m
  pending_max_entries = 65536
  prefetch_workers = 8
  overwrite_metadata = preserve
  proxy_links = true
  ```
* Mount `pcloud_posix:` with
  `--metadata-push --vfs-links --vfs-cache-mode writes --vfs-write-back 0` (or
  `full`) and verify that `chmod`, `chown`, `stat`, symlinks, file creation,
  renames, and deletes persist across remounts.
* Step-by-step production guide, including the systemd user unit, live cache
  inspection and pCloud caveats:
  [`docs/mount-pcloud-crypt-posixmeta.md`](docs/mount-pcloud-crypt-posixmeta.md).
* Verify with `chmod` + immediate unmount (`--vfs-write-back 0` uploads at
  close, so nothing is cancelled), then remount and `stat`. Note that a VFS
  *payload* upload still deferred by `--vfs-write-back` is cancelled, not
  flushed, when the mount disappears; only `posixmeta`'s sidecar queue is
  flushed by `Shutdown`/`atexit`.

### 4. CLI Subpath Re-rooting (`pm:<path>`) — Fixed

* `rclone moveto pm:a.txt pm:b.txt`, `rclone moveto pm:dir pm:dir2`, `rclone rmdir pm:dir` and `rclone purge pm:dir` open the wrapped remote re-rooted at the requested subpath. Originally, sidecars at or above that root (`<parent>/.posixmeta.<dir>`, ancestor owner sidecars) were invisible, so moves/deletes leaked them and `lsjson --metadata pm:dir/file` lost parent-owner inheritance.
* Implemented in `backend/posixmeta/posixmeta.go`: `NewFs` now mirrors `fs.ErrorIsFile` by rooting at the file's parent, and keeps `f.base` (wrapped base root) for identity-mapped sidecar I/O (`absRemote`). `Move`, `DirMove`, `Rmdir`, `Purge`, `getSidecar`, writeback, cache invalidation, and ancestor resolution all use base-relative paths.
* Covered by `01-cli` tests `test_cli_move_propagates_sidecar`, `test_cli_rmdir_removes_directory_sidecar`, `test_cli_dir_move_propagates_directory_sidecar`, `test_cli_purge_removes_directory_sidecar`, `test_cli_subpath_metadata_inherits_ancestor_owner`, `02-mount-crypt` tests `test_crypt_subpath_dir_move_preserves_metadata`, `test_crypt_subpath_rmdir_removes_encrypted_sidecar`, plus Go unit tests `TestNewFsFileRootReturnsParent`, `TestSubpathInheritsOwnerFromSidecarOutsideRoot`, `TestRmdirAtRootRemovesSidecarOutsideRoot`.

### 5. Bounded Sidecar Cache — Fixed

* The sidecar TTL cache used to be an unbounded `map[string]cacheEntry`: every distinct path resolved (including the negative entries produced by parent-owner ancestor lookups) stayed resident for the lifetime of the process, which is unsafe for months-long mounts over large remotes.
* It is now an LRU (`container/list` + map) bounded by `cache_max_entries` (default 65536) and `cache_max_size` (default 64 MiB, approximate accounting in `cacheEntrySize`). Expired entries are removed when they are touched instead of accumulating, and every invalidation path keeps the LRU order and byte counter exact (`removeCacheEntryLocked`, `clearCacheLocked`).
* Eviction never affects correctness: reads fall back to the wrapped remote, mutations invalidate before writing, and unflushed updates live in `f.pending`, which is consulted before the cache. A single entry larger than the byte budget stays cached rather than thrashing.
* Monitor long-lived mounts with `rclone backend cachestats pm:` (`entries`, `bytes`, `max_entries`, `max_bytes`, `hits`, `misses`, `evictions`, `ttl`, `pending_writes`). A standalone CLI invocation reports a fresh, empty cache; for live numbers mount with `--rc` and use `rclone rc backend/command command=cachestats fs=pm:`.
* Covered by Go unit tests `TestCacheMaxEntriesIsEnforced`, `TestCacheKeepsRecentlyUsedEntries`, `TestCacheDropsExpiredEntriesOnAccess`, `TestCacheByteBudget`, `TestCacheInvalidatePrefixFreesBudget`, `TestCacheUnlimitedWhenZero`, `TestCacheStatsCommand`, `TestCacheConcurrentAccess`.


### 6. Durable, Ordered Sidecar Writeback — Fixed

* The debounce queue used to be fire-and-forget: the pending entry was deleted
  before the upload, failures were logged once and dropped, N dirty entries
  meant N concurrent upload goroutines, and an unmount lost everything still
  inside `writeback_delay` (a mount never evicts its `Fs`, and
  `vfs.VFS.Shutdown` does not call the backend).
* It is now one engine: timers enqueue paths into a bounded channel served by
  `writeback_workers`; `f.pending` is the source of truth and is cleared only
  after a successful write; failures retry with exponential backoff
  (`writeback_retry_max`); `pending_max_entries` bounds the queue (overflow is
  written synchronously); one in-flight write per path + generation numbers
  guarantee that a stale snapshot can never overwrite a newer one.
* `Shutdown` (`fs.Shutdowner`) flushes and stops the workers, and a single
  `lib/atexit` handler flushes every live `Fs` at process exit (normal exit and
  SIGINT/SIGTERM), which is what makes "`chmod` then `fusermount -u`" safe.
* Destructive/replacing operations quiesce first (`quiesceAbs`,
  `quiescePrefix`): queued updates are cancelled *and* in-flight writes awaited,
  so `Remove`/`Rmdir`/`Purge`/`Move`/`DirMove`/`Copy`/`Mkdir` cannot be undone
  by a late writeback.
* Covered by `TestWritebackRetriesUntilSuccess`, `TestWritebackKeepsNewestUpdate`,
  `TestPendingCancelledOnRemove`, `TestShutdownFlushesPending`,
  `TestPendingCapFallsBackToSync`, plus `02-mount-crypt`
  `test_mount_chmod_survives_immediate_unmount` and
  `test_mount_rewrite_preserves_metadata`.

### 7. Scale: Listing Priming, Singleflight and Cheaper Resolution — Fixed

* `List` now derives sidecar presence from the listing it already has: entries
  without a `.posixmeta` sibling are cached as absent for free, existing
  sidecars are prefetched with `prefetch_workers` (bounded), so the `stat` storm
  that follows a directory read is served from the cache.
* `resolve` reads the entry's own sidecar first and only walks ancestors for the
  ownership fields it does not override; `inheritedOwner` is shared with
  `buildSparseSidecar` instead of duplicating the walk.
* Concurrent cold reads of the same sidecar are coalesced (singleflight), and
  sidecar reads are capped at 1 MiB with corrupt/oversized/future-schema
  payloads treated as absent (counted and logged, never fatal).
* `posixDirectory.SetMetadata` only invalidates the subtree when uid/gid
  actually changed (a plain `chmod` keeps descendant cache entries).
* Covered by `TestGetSidecarSingleflight`, `TestListPrimesSidecarCache`,
  `TestListRHidesSidecars`, `TestResolveIgnoresUnusableSidecars`.

### 8. Wrapped-Feature Delegation — Fixed

* `Copy` (server-side copy + sidecar copy/cleanup), `ListR` (bulk recursive
  listing, sidecars hidden), `ChangeNotify` (cache invalidation), `MkdirMetadata`
  (directory metadata from `copy --metadata`), `PutUnchecked`, `About`,
  `DirCacheFlush` and the `SlowModTime`/`NoMultiThreading` flags are forwarded to
  the wrapped backend.
* `Copy`/`ChangeNotify`/`About` are only advertised when the wrapped backend has
  them, and `PartialUploads` is deliberately not propagated (rclone's `.partial`
  name would bypass the sidecar-path guard).
* `overwrite_metadata` (new option, default `preserve`) defines what happens to
  an existing sidecar when an object is overwritten.
* Covered by `TestMkdirMetadataStoresSidecar`, `TestPutAppliesIncomingMetadata`,
  `TestPutOverwriteMetadataPolicy`, `TestInvalidOverwriteMetadataRejected`,
  `TestPurgeFallbackRemovesSidecars`, `TestRmdirRemovesOrphanSidecar`.

### 9. Sidecar Naming and Proxied Symlinks — Fixed

* Sidecars moved from `<entry>.posixmeta` to `<dir>/.posixmeta.<entry>`, with
  the remote root's own sidecar being the bare `.posixmeta` stored *inside* it
  (a subpath root keeps `<parent>/.posixmeta.<root>`)
  (`sidecar_prefix`, default `.posixmeta`). The old `sidecar_suffix` option is
  removed and only logs a warning when set; **old sidecars are not migrated**
  and simply become visible files
  (`docs/adr/0009-sidecar-prefix-naming.md`).
* `proxy_links = true` stores a symlink entirely in the entry's sidecar
  (`symlink` attribute, no `.rclonelink` payload) and emulates the
  `<name>.rclonelink` object in `List`/`ListR`/`NewObject`
  (`docs/adr/0008-proxy-link-files.md`). Link writes are synchronous and
  validated; remove/move/copy keep the sidecar in sync; `NewFs` mirrors
  `fs.ErrorIsFile` for a link root. `proxy_links = false` (default) keeps the
  upstream two-object representation and treats literal `*.rclonelink` files as
  ordinary files.
* Covered by `TestProxyLinkStoresOnlySidecar`,
  `TestProxyLinkRejectsInvalidTargets`, `TestProxyLinkRemoveDeletesSidecar`,
  `TestProxyLinkMoveMovesSidecar`, `TestProxyLinkCopyKeepsLink`,
  `TestProxyLinkShadowsLegacyPayload`, `TestProxyLinkDisabledUploadsPayload`,
  `TestProxyLinkSetMetaCreatesLink`, `TestProxyLinkNewFsFileRoot`,
  `TestProxyLinkListR`, `test_cli_sidecar_prefix_layout`,
  `test_cli_proxy_links_roundtrip` and `test_mount_proxy_links_sidecar_only`.

### 10. Next Areas / Accepted Residuals

The full, itemised list lives in `docs/durability-and-scale.md` §3. The ones
worth planning for:

* The debounce queue is memory-only: a `SIGKILL`/power loss can lose the last
  `writeback_delay` window (use `writeback_delay = 0` when that matters).
* `overwrite_metadata = preserve` (default) means plain `copy`/`sync` over an
  existing object inherits its metadata; use `reset` for copy-only remotes.
* Payload + sidecar are two objects (not atomic as a pair), and there is no
  single-writer coordination between two mounts of the same remote.
* Recursive metadata enumeration (`lsjson -R --metadata`) still costs one
  sidecar lookup per entry because `ListR` cannot safely prime the cache.
* Prefix invalidation and quiescing scan the cache/pending maps (O(entries),
  only on directory moves/purges and owner changes).
* The old `<name>.posixmeta` layout is not migrated: pre-upgrade sidecars show
  up as ordinary files and their metadata is ignored. Rename them to
  `<dir>/.posixmeta.<name>` (through `rclone`, also for crypt remotes) or reset
  the metadata.
* `proxy_links` changes the meaning of literal `*.rclonelink` files: while it is
  enabled, a payload is shadowed by a sidecar `symlink` for the same visible
  name. Link mtime is the sidecar's mtime (`SetModTime` is a no-op), and a link
  still costs a sidecar object plus an upload even though it has no payload.


### 11. Per-Directory Aggregate Metadata Layout — Fixed

* `sidecar_layout = dir` (opt-in, default stays `sparse`) stores one schema-v2
  JSON map per directory (`<dir>/.posixmeta`) instead of one
  `<prefix>.<name>` sidecar per overridden entry, so a directory with thousands
  of `chmod`/`chown` entries costs one metadata object and one read.
* The map holds its children's attributes (keys are entry names, `"."` is the
  base root entry) and carries a CRC-32 of its canonical JSON written as 8
  lowercase hex digits; the root map is the layout declaration (created once,
  never deleted). Empty non-root maps are deleted instead of written.
* The schema version is the sole layout discriminator: v1 = sparse, v2 = dir
  (there is no `layout` field). Mismatches are explicit errors, both at `NewFs`
  and when a listing reveals the other layout's objects; metadata is never
  converted or ignored. The retired `layout` field is ignored on read, but a
  legacy `"crc":"crc32:…"` checksum is corrupt and needs `rclone backend repair`.
* Corruption is fail-closed: unparsable/oversized/CRC-mismatched maps make the
  directory's reads fail and refuse mutations, and the object is never
  rewritten. `on_corrupt = warn` serves defaults but still refuses writes.
  Recovery is `rclone backend repair` (dry run by default; quarantine + salvage
  or reset; `-o dry_run=false -o confirm=REPAIR` to write).
* The map is the cache and writeback unit: LRU/TTL bounded `mapState` per
  directory, single-flight reads, whole-map snapshots written through a
  server-side staging object, coalesced per directory by the existing debounce
  engine (`writeUnit`), with dirty state never evicted and `Rmdir` removing the
  directory's own map object first.
* Covered by `TestDirLayout*` (Go), `test_cli_dir_layout_*`,
  `test_mount_dir_layout_roundtrip`, and `docs/adr/0010-per-directory-aggregate-sidecars.md`.

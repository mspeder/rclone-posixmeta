# posixmeta durability, ordering and scale — what changed, what is left

This document records the hardening pass over `backend/posixmeta` for
months-long, large-file-count production mounts (`local -> crypt -> posixmeta ->
FUSE`). It is the companion to `docs/adr/0004-durable-writeback-and-delegation.md`
(design decisions) and lists both the fixes and the **residual limitations** that
were deliberately left in place.

No file under `third_party/rclone` was modified: every change is inside
`backend/posixmeta/`, using interfaces the upstream `fs.Features.Fill` already
discovers (`fs.Shutdowner`, `fs.ListRer`, `fs.ChangeNotifier`, `fs.Copier`,
`fs.MkdirMetadataer`, `fs.PutUncheckeder`, `fs.Abouter`, `fs.DirCacheFlusher`).

## 1. Fixed

### 1.1 Durability of debounced sidecar writes

| Problem (before) | Fix |
| --- | --- |
| `f.pending` deleted the entry *before* writing, so a failed remote write was logged once and lost. | The pending entry is the source of truth and is only deleted after a successful write (`finishWriteback`). Failures are retried with exponential backoff up to `writeback_retry_max`, for the lifetime of the process, and counted/logged each time. |
| A single `time.AfterFunc` goroutine per path did the I/O, so N dirty entries meant N concurrent remote writes. | Timers now only *enqueue the path*; a fixed pool of `writeback_workers` goroutines performs the I/O against a bounded channel. |
| No ordering guarantee: an older snapshot could be uploaded after a newer one. | One in-flight write per path (`f.inflight` + per-path generation); a superseding update is re-queued when the older write completes, and `flushPendingAbs` waits for the in-flight write before applying the newer value. |
| Unbounded queue. | `pending_max_entries` (default 65536); on overflow the update is persisted synchronously by the caller instead of growing the queue. |
| Unmount lost everything still inside the debounce window (`rclone mount` never evicts its `Fs`, and `vfs.VFS.Shutdown` does not call the backend). | New `fs.Shutdowner` implementation flushes and stops the workers **and** `NewFs` registers one `lib/atexit` handler that flushes every live `Fs` with queued updates. rclone runs atexit handlers on normal exit and on SIGINT/SIGTERM. |
| `Remove`/`Rmdir`/`Purge`/`DirMove` left queued updates behind, so a late writeback recreated sidecars of deleted objects (invisible junk, "directory not empty"). | Destructive operations call `quiesceAbs`/`quiescePrefix`: cancel queued updates **and** wait for in-flight writes before touching sidecars. |
| `Put`/`Move`/`Copy` left the destination's queued update running, which could overwrite the new metadata. | Those paths quiesce the destination first. |

### 1.2 Read path: fewer round-trips, bounded memory

| Problem | Fix |
| --- | --- |
| Every `stat` after a directory read cost one remote read per entry, and one per ancestor for ownership inheritance. | `List` derives the `.posixmeta.*` sibling set from the listing it already has: entries without a sidecar are cached as "absent" for free, existing sidecars are prefetched with `prefetch_workers` (capped at `64*prefetch_workers` entries and 30s per listing). |
| `resolve` walked every ancestor before reading the entry's own sidecar. | The entry's sidecar is read first; ancestors are only consulted for the ownership fields it does not override. `buildSparseSidecar` reuses the same `inheritedOwner` helper instead of duplicating the walk. |
| A cold cache with many concurrent stat calls produced a burst of identical remote reads. | `getSidecarAbs` coalesces concurrent reads of the same path (singleflight). |
| Corrupt sidecars were re-read (and re-logged) forever, and an arbitrarily large object would be loaded into memory. | Reads are limited to 1 MiB (`maxSidecarSize`); corrupt, oversized and future-schema (`"v" > 1`) sidecars are counted once, logged, treated as absent, and cached as absent. |


### 1.3 Feature delegation

Forwarded to the wrapped backend, with sidecar bookkeeping around them:

* **`Copy`** — keeps the wrapped backend's server-side copy instead of a
  download+upload, and copies the source sidecar (or removes a stale destination
  sidecar) so `rclone copy pm:a pm:b` is metadata-correct.
* **`ListR`** — recursive walks (`copy`, `check`, `size`, `ncdu`) reuse the
  wrapped bulk listing and hide sidecars; falls back to a local recursion when
  the wrapped backend has no `ListR`.
* **`MkdirMetadata`** — makes `rclone copy --metadata`/`sync` preserve directory
  mode/uid/gid (POSIX keys go to the sidecar, the rest to the wrapped backend).
  `ReadDirMetadata`/`WriteDirMetadata`/`UserDirMetadata` are advertised.
* **`ChangeNotify`** — notifications invalidate the affected cache entries and
  the subtree below the changed directory. Only advertised when the wrapped
  backend supports it (otherwise stock "poll-interval not supported" behaviour).
* **`PutUnchecked`**, **`About`**, **`DirCacheFlush`**, **`SlowModTime`**,
  **`NoMultiThreading`** — forwarded when available.
* `PartialUploads` is deliberately **not** forwarded (see residual 4).

### 1.4 Hardening and correctness details

* `parseOctalMode` masks with `0o7777`, so a stray file-type bit can never end up
  in `getattr`; `Chmod` now records setuid/setgid/sticky instead of dropping them
  (`0%o` of `mode & 0o7777`).
* `overwrite_metadata` (new, `preserve` by default): overwriting an object keeps
  the previous sidecar unless the upload carries POSIX metadata (`--metadata`
  does) — so a VFS re-upload cannot silently drop a chmod. Set it to `reset` for
  `copy`/`sync` workloads where stale metadata must never be inherited.
* `NewFs` fails instead of silently degrading when the sidecar base remote
  cannot be opened, rejects unknown `overwrite_metadata` values, and warns when
  the cache is unbounded.
* `Rmdir` removes orphaned sidecars (whose payload is gone) and retries once, so
  a hidden `.posixmeta.*` file can no longer cause an unexplainable "directory not
  empty".
* `Purge` falls back to a sidecar-aware recursive delete when the wrapped backend
  returns `fs.ErrorCantPurge` (previously the whole directory could not be
  purged at all, leaking sidecars).
* `Move`/`DirMove` no longer swallow sidecar-move failures: they are logged and
  counted, and a move without a source sidecar clears the destination sidecar
  instead of leaving the previous object's metadata behind.
* `cachestats` reports writeback health as well (`written`, `failed_writes`,
  `retried_writes`, `overflow_writes`, `inflight_writes`, `last_write_error`).
* Dead code removed (`pendingWrite.isDir`, `resolvedMeta.sidecarSeen`, unused
  `fserrors` assertion); the duplicated POSIX-metadata parsers in
  `posixObject.SetMetadata`, `posixDirectory.SetMetadata`, `Chmod` and `Chown`
  are now one `applyPOSIXAttrs` helper.

## 2. New / changed options

| Option | Default | Meaning |
| --- | --- | --- |
| `writeback_workers` | `4` | Concurrent sidecar writeback workers. `0` persists synchronously in the calling goroutine (useful for debugging). |
| `writeback_retry_max` | `5m` | Maximum backoff between retries of a failed sidecar write. |
| `pending_max_entries` | `65536` | Max debounced updates held in memory; beyond this, updates are written synchronously (`0` = unlimited). |
| `prefetch_workers` | `8` | Concurrent sidecar prefetches per directory listing (`0` disables prefetching; "absent" is still cached for free). |
| `overwrite_metadata` | `preserve` | `preserve` keeps the previous object's metadata across overwrites; `reset` drops it unless the upload carries POSIX metadata. |
| `sidecar_layout` | `sparse` | `sparse` writes `<dir>/.posixmeta.<entry>` per overridden entry; `dir` writes one aggregate `<dir>/.posixmeta` map per directory. Recorded in the remote's root map; the two are never converted automatically. |
| `on_corrupt` | `error` | `dir` layout only: `error` fails reads/mutations of a corrupt map, `warn` logs it and serves defaults while still refusing to overwrite it. |
| `map_max_entries` | `100000` | `dir` layout only: entries accepted in one directory map; a larger map is treated as corrupt (`0` = unlimited). |
| `map_max_bytes` | `8M` | `dir` layout only: accepted size of one directory map; larger maps are treated as corrupt (`0` = unlimited). |

Existing options (`cache_ttl`, `cache_max_entries`, `cache_max_size`,
`writeback_delay`, `inherit_parent_owner`, `hide_sidecars`, ...) keep their
meaning and defaults.

## 3. Residual limitations (known and accepted)

Each item names the impact and the supported workaround. Nothing here is silent:
the affected paths log and/or count errors.

1. **The debounce queue is memory-only.** A `SIGKILL`, power loss or OOM kill
   loses up to `writeback_delay` of queued updates (normal exit, `fusermount -u`
   and SIGINT/SIGTERM are safe: both `Shutdown` and the atexit flush run).
   *Workaround:* `writeback_delay = 0` for maximum durability (one sidecar write
   per metadata change, no coalescing).
2. **`overwrite_metadata = preserve` (default) keeps stale metadata.** A plain
   `rclone copy`/`sync` over an existing object inherits the previous object's
   mode/uid/gid, because the VFS re-upload of a file carries no metadata and
   dropping it would lose `chmod` on every write through the mount.
   *Workaround:* set `overwrite_metadata = reset` on remotes used only for
   `copy`/`sync` (never together with a `--metadata-push` mount, where a rewrite
   would then drop `chmod`/`chown`).
3. **Payload and sidecar are two objects, so the pair is not atomic.** A crash
   between the payload upload and the sidecar write can leave new content with
   the previous metadata (never new metadata on old content, because the sidecar
   is written after the payload). *Workaround:* none inside this backend; keep
   `writeback_delay` small when metadata must track content closely.
4. **`PartialUploads` is not advertised.** rclone therefore writes to the final
   name through `posixmeta` (as it always did) instead of a
   `<name>.<hash>.partial` object. That is deliberate: `checkPartial`'s staging
   name would bypass the "reject writes to a sidecar path" guard (a
   `.posixmeta.x.txt.<hash>.partial` payload would be staged and then renamed
   onto a sidecar). Atomicity of the payload stays the wrapped backend's job.
5. **Ancestor-owner lookups still walk the tree on a cold cache.** Each level is
   one sidecar read (404 included), cached as absent for `cache_ttl`. Deep trees
   pay per level again after each TTL expiry. *Workaround:* raise `cache_ttl`
   and `cache_max_entries` so the hot ancestor set stays resident.
6. **Recursive metadata enumeration costs one sidecar lookup per entry.**
   `ListR` deliberately does **not** prime the cache: a negative prime from a
   batched walk can be wrong (a backend may split one directory across callback
   batches) and prefetching every sidecar would turn a walk into a fetch storm.
   So `lsjson -R --metadata`, `check --metadata` and `copy --metadata` pay one
   lookup per entry, 404s included. *Workaround:* per-directory operations
   (`lsjson --metadata`, mount `stat`) benefit from listing-driven priming; for a
   one-off audit raise `prefetch_workers` and `cache_ttl`.
7. **Prefix operations scan the cache map.** `invalidatePrefix`,
   `invalidateSubtree`, `cancelPendingPrefix` and `quiescePrefix` are O(cached
   entries) while holding `f.mu` (~65k iterations ≈ 1 ms at the default cap).
   They only run on directory moves/purges and on owner changes — not on plain
   `chmod` (which only invalidates descendants) or per-entry writes.
8. **Prefetch is synchronous inside `List`.** A directory containing many
   sidecars delays that listing by up to `64 * prefetch_workers` fetches
   (hard-capped at 30 s); the rest are read on demand. *Workaround:*
   `prefetch_workers = 0` disables prefetch (absence priming stays free).
9. **The exit flush is bounded and not retried.** `flushAllPending` runs once
   with a 30 s context on shutdown/atexit; a slow or failing remote can lose the
   tail, which is logged (`posixmeta: could not flush N sidecar update(s) on
   exit`) and counted. Retries only exist while the process lives.
10. **No dead-letter queue.** If the remote stays unreachable for the lifetime of
    a mount, queued updates accumulate to `pending_max_entries` and new updates
    then block the caller with a synchronous write. Once the process exits, an
    update that still cannot be written is lost (logged and counted).
11. **Single-writer assumption.** Two processes writing the same entry (two
    mounts, or a mount plus `rclone backend setmeta`) race: the sidecar JSON has
    a schema version but no CAS/etag, so the last writer wins. *Workaround:* do
    not mount the same remote read-write from two places.
12. **`Copy` fails if the source sidecar cannot be read.** The payload has
    already been copied at that point, but the command reports an error rather
    than silently dropping metadata.
13. **Unchanged fork limitations.** `chown` of the mount root is not propagated
    (synthetic VFS root entry) and the `cmount`/`mount2` path is not patched; use
    `rclone mount`. See `docs/adr/0001-fuse-chmod-chown-propagation.md`.
14. **External metadata changes lag by `cache_ttl`.** A `backend setmeta` run
    from another process is invisible to a mount until the cached entry expires,
    unless the wrapped backend implements `ChangeNotify` (and the backend chain
    below `posixmeta` forwards it: `pcloud` implements it, and `crypt`
    delegates it).
15. **`atime`/`ctime` are never stored, and the `symlink` sidecar attribute is
    not consumed by the FUSE path.** Both are intentional invariants
    (`S_IFLNK` + `--link-perms` comes from the fork/`--vfs-links`, see
    `docs/adr/0002-fuse-symlinks-via-vfs-links.md`).
16. **Directory metadata through `Put`-style writers is still "directory only".**
    `MkdirMetadata` covers `copy --metadata`/`sync`; a backend or tool that
    creates directories with plain `Mkdir` and sets metadata afterwards relies on
    a following `SetMetadata` (as the mount does).
17. **A VFS payload upload still deferred by `--vfs-write-back` is cancelled when
    the mount disappears.** `fusermount -u` ("Unmounted externally") and
    SIGINT/SIGTERM make `rclone mount` exit without draining the VFS upload
    queue, so a file closed less than `--vfs-write-back` (default 5 s) before a
    stop never reaches the remote. This is stock rclone behaviour and unrelated
    to the sidecar queue: `posixmeta`'s own debounced updates *are* flushed by
    `Shutdown` and the atexit handler. *Workaround:* mount with
    `--vfs-write-back 0` (upload on close, as
    `docs/mount-pcloud-crypt-posixmeta.md` does) or wait out the delay before
    unmounting.


## 4. Operating a long-lived mount

Recommended production section (all values are the documented defaults except
`writeback_delay`, which suits a WAN remote):

```ini
[pcloud_posix]
type = posixmeta
remote = pcloud_crypt:
default_file_mode = 0644
default_dir_mode = 0755
inherit_parent_owner = true
sidecar_prefix = .posixmeta
proxy_links = true
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
```

Mount with `--metadata-push --vfs-links --vfs-cache-mode writes` (or `full`).

### 1.5 Sidecar layout and proxied symlinks

* Sidecars are now `<dir>/.posixmeta.<entry>` (`sidecar_prefix`). The remote
  root (no subpath) has no name, so its own sidecar is the bare prefix stored
  *inside* it; a subpath root keeps `<parent>/.posixmeta.<root>`
  (`docs/adr/0009-sidecar-prefix-naming.md`). This is a **breaking on-disk
  change**: `*.posixmeta` files from an older tree become ordinary visible files
  and are not migrated. Rename them through `rclone` (also on crypt remotes) or
  reset the metadata. The rename is cheap for `List`/pairing (a purely local
  `sidecarTarget` mapping, no extra round-trips), and it reserves the
  `.posixmeta.*` namespace so a user file cannot be mistaken for a sidecar.
* `proxy_links = true` (`docs/adr/0008-proxy-link-files.md`) stores a symlink's
  target in the entry's sidecar instead of uploading a `<name>.rclonelink`
  payload, and synthesises the `.rclonelink` object in `List`/`ListR`/
  `NewObject`. Consequences for durability: the sidecar write is synchronous
  (a failed write fails `ln -s` instead of being retried after the link was
  reported as created), targets are limited to 64 KiB and validated, `Move`/
  `Copy`/`Remove` carry the sidecar, renaming a link to a non-link name
  materialises the target as a regular file, and `NewFs` probes the sidecar to
  mirror `fs.ErrorIsFile` for a link root.
* Residual: link payloads and sidecars are still two objects for ordinary
  files; `proxy_links` only removes the payload for links. Link mtime is the
  sidecar's mtime, and `SetModTime` on a link is a no-op. While `proxy_links`
  is enabled, a literal `<name>.rclonelink` payload is shadowed by a sidecar
  `symlink` for the same visible name.

### 1.6 Per-directory aggregate layout (`sidecar_layout = dir`)

* `sidecar_layout = sparse` (default) is the per-entry layout above.
  `sidecar_layout = dir` writes one schema-v2 JSON map per directory,
  `<dir>/.posixmeta`, holding every entry that has an override (the key is the
  entry name; `"."` is the base root entry). The map carries a CRC-32 of its
  canonical JSON (8 lowercase hex digits) and the remote's root map (created
  once, never deleted) records the layout, so `pm:dir` still reads its own
  attributes from the parent map. The schema version is the sole layout
  discriminator (v1 = sparse, v2 = dir); there is no separate `layout` field.
* **One object and one read per directory.** `List` loads the directory's map
  once and derives all metadata (and any proxied symlink) from it, so a
  `stat` storm after a listing costs nothing extra; `cachestats` on a live mount
  reports `maps`, `maps_bytes`, `dirty_maps` and `corrupt_maps`.
* **Coalesced, ordered writeback.** The map is the writeback unit: updates are
  merged in memory, the debounce engine keys its queue/in-flight set by the map
  directory, and each flush serialises one whole snapshot through a
  `<prefix>.tmp` staging object plus a server-side `Move`. One in-flight write
  per map and generation checks mean an older snapshot can never land after a
  newer one, and an unchanged map uploads nothing.
* **Fail closed on corruption.** Unparsable JSON, a CRC mismatch, an unknown
  schema, or a map beyond `map_max_entries`/`map_max_bytes` makes the whole
  directory's metadata unusable: reads error, mutations are refused, and the
  object is never rewritten. `on_corrupt = error` is the default;
  `on_corrupt = warn` serves defaults while still refusing writes. Recover with
  `rclone backend repair pm:` (dry run by default; add
  `-o dry_run=false -o confirm=REPAIR` to quarantine and rewrite).
* **Layout mismatches are explicit.** The schema version is the only layout
  discriminator (v1 = sparse, v2 = dir); opening a remote with the other layout
  (or listing a directory that contains the other layout's objects) fails with
  `refusing to mix layouts` instead of silently ignoring metadata. There is no
  migration tooling: rewriting the tree with the other layout is the conversion.
* Residuals: a corrupt or oversized map costs a whole directory's metadata, not
  one entry; every change in a directory rewrites its map (a cross-directory
  rename rewrites two), which is why the caps exist; the mount's `Attr` swallows
  `Metadata()` errors, so a cached `stat` shows defaults on a corrupt map while
  `ls`/`chmod`/`NewObject` fail; and a map is still two objects with its
  payloads (no atomic multi-object commit).

### Monitoring

`rclone backend cachestats pm:` (or, against a live mount started with `--rc`,
`rclone rc backend/command command=cachestats fs=pm:`) reports:

| Field | Healthy | What it means |
| --- | --- | --- |
| `entries` / `bytes` | below `max_entries` / `max_bytes` | Cache occupancy. Steady at the cap with a low `hits` ratio means `cache_max_*` is too small for the working set. |
| `hits` / `misses` | hits ≫ misses after warm-up | Effectiveness of listing priming + prefetch + TTL. |
| `evictions` | low | High eviction alongside high misses = undersized cache (correctness is unaffected, only latency). |
| `pending_writes` | small, drains to 0 | Debounced updates not yet uploaded. Growing without bound means the remote is slow/unreachable. |
| `inflight_writes` | ≤ `writeback_workers` | Concurrent writebacks in progress. |
| `written` / `failed_writes` / `retried_writes` | `failed_writes` 0 or stagnant | Each failure is retried with backoff; a growing `failed_writes` means uploads are failing (permissions, quota, network). |
| `overflow_writes` | 0 | Updates persisted synchronously because `pending_max_entries` was reached: raise the cap or `writeback_workers`. |
| `last_write_error` | empty | Last writeback error, kept for diagnosis after the retries succeed. |

The message `ERROR internal error: no overview data found for "posixmeta"` is a
benign upstream docs-lookup warning (see `AGENTS.md`).

## 5. Verification

```bash
./test/run.sh                 # go unit + CLI + mount/crypt (32 tests)
./test/run.sh 01-cli
./test/run.sh 02-mount-crypt
```

New coverage added by this pass:

* `backend/posixmeta/engine_test.go`
  * `TestWritebackRetriesUntilSuccess` — injected `Put` failures are retried and
    the sidecar eventually matches, with `failed`/`retried` counters and
    `last_write_error` set.
  * `TestWritebackKeepsNewestUpdate` — a newer update that arrives while an older
    one is in flight wins (no reordering).
  * `TestPendingCancelledOnRemove` — `Remove` cancels the queued update; no
    sidecar is resurrected.
  * `TestShutdownFlushesPending` — `Shutdown` flushes a 1 h-debounced update,
    stops the engine, and later updates are written synchronously; `Shutdown` is
    idempotent.
  * `TestPendingCapFallsBackToSync` — overflow past `pending_max_entries` is
    persisted synchronously and counted.
  * `TestGetSidecarSingleflight` — 8 concurrent cold reads become one remote read.
  * `TestListPrimesSidecarCache` — after `List`, lookups of both a metadata-
    carrying and a plain entry are cache hits.
  * `TestListRHidesSidecars` — recursive listing hides sidecars and does not
    poison the cache.
  * `TestRmdirRemovesOrphanSidecar`, `TestPurgeFallbackRemovesSidecars`,
    `TestResolveIgnoresUnusableSidecars` (oversized + future schema),
    `TestMkdirMetadataStoresSidecar`, `TestPutAppliesIncomingMetadata`,
    `TestPutOverwriteMetadataPolicy` (preserve/reset),
    `TestInvalidOverwriteMetadataRejected`, `TestChmodMetadataKeepsSpecialBits`.
* `test/suites/02-mount-crypt.sh`
  * `test_mount_chmod_survives_immediate_unmount` — `chmod` then immediate
    unmount with `writeback_delay=1h` still persists the mode (proves the
    shutdown/atexit flush).
  * `test_mount_rewrite_preserves_metadata` — rewriting content through the
    mount keeps the sidecar metadata (`overwrite_metadata=preserve`).


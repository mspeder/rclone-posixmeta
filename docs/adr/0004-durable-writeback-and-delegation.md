# 0004 — Durable, ordered sidecar writeback and feature delegation

* Status: Accepted
* Date: 2026-11-22
* Supersedes: none
* Related: `0003-bounded-sidecar-cache.md`, `0001-fuse-chmod-chown-propagation.md`

## Context

A review of `backend/posixmeta` aimed at months-long production mounts found
that the debounced writeback path could lose metadata:

* `writeback_delay` queued updates in `f.pending` and a `time.AfterFunc`
  goroutine deleted the entry *before* writing it, so a failed remote write was
  logged once and dropped.
* `rclone mount` does not evict its `fs.Fs` on unmount, and `vfs.VFS.Shutdown`
  does not call the backend, so `CleanUp`/`Shutdown` never ran for a mount: every
  update still inside the debounce window was lost on `fusermount -u`.
* `Remove`/`Rmdir`/`Purge`/`DirMove` did not cancel queued updates, so a late
  writeback resurrected sidecars of deleted objects (invisible orphan objects).
* Nothing bounded the number of queued updates, ordered two writes of the same
  path, or coalesced concurrent reads of the same sidecar.
* `List` already returns every `<entry>.posixmeta` sibling but threw that
  information away, so a `stat` storm after a directory read cost one remote
  round-trip per entry (plus one per ancestor for ownership inheritance).
* Optional wrapped-backend features (`Copy`, `ListR`, `ChangeNotify`,
  `MkdirMetadata`, `PutUnchecked`, `About`, `DirCacheFlush`) were not forwarded,
  so `rclone copy` over the backend silently lost server-side copy and
  directory metadata.

The review also established that every fix could live in `backend/posixmeta`:
`fs.Features.Fill` discovers `fs.Shutdowner`/`fs.ListRer`/... automatically, and
`lib/atexit` is already imported by other backends.

## Decision

1. **One engine for debounced updates.** `scheduleSidecarUpdate` stores the
   update in `f.pending` (which is the source of truth) and arms a
   `time.AfterFunc` that only *enqueues the path* into a bounded channel served
   by `writeback_workers` goroutines. Workers claim a generation, persist it, and

   * drop the pending entry when the written generation is still current,
   * re-enqueue an immediate retry when a newer generation arrived meanwhile,
   * retry with exponential backoff (`writeback_retry_max`) on failure, keeping
     the entry queued for the lifetime of the process.

   `pending_max_entries` bounds the queue; overflow persists synchronously in the
   caller instead of growing without limit.

2. **Ordering.** Every path has at most one in-flight write (`f.inflight`), and
   `flushPendingAbs` waits on the in-flight write before writing the newer value,
   so an older payload can never land after a newer one.

3. **Flush is guaranteed at exit.** `Shutdown` flushes and stops the workers.
   Because a mount process never evicts its Fs, `NewFs` also registers a single
   `lib/atexit` handler (see `flushLiveFs`) which flushes every live `Fs` that
   still has queued updates; rclone runs those handlers on normal exit and on
   SIGINT/SIGTERM.

4. **Destructive operations quiesce first.** `Remove`, `Rmdir`, `Purge`,
   `DirMove`, `Move`, `Copy`, `Mkdir` and the synchronous `scheduleSidecarUpdate`
   paths cancel queued updates for the affected paths and wait out in-flight
   writes (`quiesceAbs`/`quiescePrefix`) before touching sidecars.

5. **Directory listings prime the cache.** `List` builds the set of
   `*.posixmeta` siblings, caches "absent" for entries without one for free, and
   prefetches existing sidecars with `prefetch_workers` goroutines (capped at
   `64*prefetch_workers` entries and 30s per listing).

6. **Delegation, not reimplementation.** `Copy`, `ListR`, `ChangeNotify`,
   `MkdirMetadata`, `PutUnchecked`, `About` and `DirCacheFlush` forward to the
   wrapped backend, with sidecar bookkeeping around them. `Copy`,
   `ChangeNotify` and `About` are only advertised when the wrapped backend
   implements them, so callers fall back to their generic paths.

## Consequences

* Metadata survives a failed write, a slow remote, and an unmount.
* Recursive walks (`copy`, `check`, `size`, `ncdu`) keep the wrapped backend's
  bulk listing, and server-side copies keep their sidecar.
* Extra cost: a sidecar `stat` storm is replaced by a bounded prefetch burst on
  listing; the debounce queue is memory-resident, so a `SIGKILL` or power loss
  can still lose the last `writeback_delay` window (documented in
  `docs/durability-and-scale.md`).
* `overwrite_metadata` was added because "drop the stale sidecar on overwrite"
  and "keep metadata when the VFS re-uploads a file without repeating it" are
  incompatible; `preserve` is the default so mount semantics never regress.

# 0003 — Bounded sidecar metadata cache

* Status: Accepted
* Date: 2026-11-22
* Supersedes: none
* Related: `0001-fuse-chmod-chown-propagation.md`, `0002-fuse-symlinks-via-vfs-links.md`

## Context

The `posixmeta` backend resolves `mode`/`uid`/`gid`/`symlink`/`btime` from
sparse `.posixmeta` sidecars on every `stat`, `List` and metadata read, and it
walks ancestor directories for `inherit_parent_owner`. To keep a FUSE mount
usable those resolutions are cached in memory by absolute remote path, with
`cache_ttl` (default 5 minutes) controlling staleness.

That cache was a plain `map[string]cacheEntry` with no capacity limit:

* Every distinct path ever resolved stayed resident for the lifetime of the
  process. Hits and misses include **negative** entries (one per missing
  sidecar per ancestor), which dominate on large trees.
* Expired entries were ignored on read but never removed, so an entry for a
  path that was touched once and never again was retained forever.
* A long-lived mount (`rclone mount`, one process over months) therefore grew
  monotonically with the number of distinct entries traversed — unbounded
  memory for unbounded backends.

rclone's own caches (`--dir-cache-time`, `--vfs-cache-max-size`,
`--vfs-cache-max-age`) do not cover this: they bound the VFS layer, not backend
metadata resolutions.

## Decision

Keep the cache in the backend (rclone has no generic metadata cache) but make it
bounded, using the least recently used policy already implied by "TTL cache":

* `cache_max_entries` (default 65536) and `cache_max_size` (default 64 MiB,
  approximate via `cacheEntrySize`) cap residency. `0` (or negative) disables
  either cap; `cache_ttl <= 0` still disables caching entirely.
* Ordering is an explicit LRU list (`container/list`) with map lookups, so both
  a hit (`MoveToBack`) and an eviction are O(1); no periodic scan or goroutine
  is needed.
* Expired entries are removed when touched, so dead keys cannot pin memory.
* A single entry larger than the byte budget stays cached (the just-refreshed
  key is never evicted), avoiding thrash without breaking the bound by more
  than one entry.
* `rclone backend cachestats pm:` exposes `entries`, `bytes`, caps,
  `hits`/`misses`/`evictions`, `ttl` and `pending_writes` for monitoring.
  Because the counters live in the `Fs` instance, use
  `rclone rc backend/command command=cachestats fs=pm:` against a mount started
  with `--rc` to read the live mount's cache; a fresh CLI process starts empty.

## Why eviction is semantically safe

* The cache is pure read-through state. A miss simply re-reads the sidecar from
  the wrapped remote.
* All mutations invalidate the affected keys before writing, and coalesced
  updates live in `f.pending`, which `getSidecarAbs` consults **before** the
  cache. Evicting a cached value therefore cannot lose or revert a pending
  update.
* Eviction changes no observable metadata; it only changes how many remote
  round-trips a later lookup costs.

## Consequences

* Resident memory is predictable on months-long mounts; worst-case overshoot is
  one entry over `cache_max_size`.
* Cold working sets larger than the budget pay an extra `NewObject` per
  resolution (latency only). Defaults are deliberately generous, and both caps
  are configurable per remote.
* `invalidatePrefix` remains O(n) over cached keys, but `n` is now bounded.
* Stats are cumulative per process; restarting the mount resets the counters.

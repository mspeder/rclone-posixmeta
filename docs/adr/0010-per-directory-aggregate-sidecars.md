# ADR 0010 — Optional per-directory aggregate metadata map (`sidecar_layout = dir`)

* Status: Accepted
* Date: 2026-11-28
* Related: `0003-bounded-sidecar-cache.md`,
  `0004-durable-writeback-and-delegation.md`, `0008-proxy-link-files.md`,
  `0009-sidecar-prefix-naming.md`

## Context

The sparse layout (`0009`) writes one object per entry *that has an override*:
`dir/.posixmeta.a`, `dir/.posixmeta.b`, … That is excellent for a handful of
`chmod`/`chown` calls, but it scales badly in the number of overridden entries:

1. **Object count.** A tree where a script chowns/chmods every entry doubles its
   object count. On pCloud/S3-class remotes each object has a cost (quota,
   request count, listing size) and a name derived from the entry name, which
   pushes short filename limits closer to the edge.
2. **Read amplification.** `List` primes the sidecar cache from the sibling
   names it already has, but a cold `stat` of *N* overridden entries in one
   directory still costs *N* reads, and every move/rename/copy/delete has to
   move or delete a second object.
3. **Consistency.** Payload and sidecar are two independent objects, so a
   rename is two remote operations that can fail between each other and leave an
   orphan that only `Rmdir`/`Purge` cleanup can repair.

Storing all of a directory's metadata in one object removes all three costs at
once: a directory with 10 000 overridden entries costs one object and one read.

## Decision

* New option `sidecar_layout = sparse | dir` (default `sparse`, so nothing
  changes for existing remotes).
* `dir` stores one JSON document per directory in `<dir>/<prefix>` (bare
  `.posixmeta`). The document is

  ```json
  {"v":2,"crc":"1a2b3c4d",
   "entries":{"<name>":{"mode":"0644","uid":1000,"gid":1000,
                        "symlink":"…","btime":"…"}}}
  ```

  where `crc` is the CRC-32 (IEEE) of the canonical, sorted-key JSON of
  `entries`, written as 8 lowercase hex digits, and the keys are the
  directory's *entry names*.
* **`v` is the sole layout discriminator.** v1 is the per-entry sparse sidecar
  format, v2 this directory map; there is no separate `layout` field (it was
  removed because it repeated `v` in every object). The decoder ignores the
  retired field if it is present, but the checksum is bare hex only: a document
  written by an older build (`"crc":"crc32:…"`) fails validation and is treated
  as corrupt, so it must be rewritten with `rclone backend repair`. Documents
  written by this build are likewise rejected (fail closed, never rewritten) by
  older builds.
* **A child's attributes live in its parent's map.** The base root entry has no
  name, so it uses the reserved key `"."` in the root map. A subpath root keeps
  the normal rule: `pm:dir` stores `dir`'s attributes in the key `dir` of
  `<base>/.posixmeta` (outside the rooted wrapped remote, reached through
  `f.base` like every other sidecar).
* **The root map is the layout declaration.** It is created once (empty) on the
  first metadata write, and never deleted, so the remote always records which
  layout wrote its metadata. An empty *non-root* map is deleted instead of
  written, which keeps the at-rest tree clean.
* **Layout mismatch is a hard error, never a conversion.** `NewFs` probes the
  root map: a v1 document under `sidecar_layout=dir` (or a v2 directory map
  document under `sparse`) fails with an actionable message, and so does the
  reverse case detected from a listing (a `<prefix>.*` object in `dir` mode, a
  bare `<prefix>` that is not the base root's own sidecar in `sparse` mode). The
  schema version **is** the layout tag (v1 = sparse, v2 = dir), so every map
  states on its own which layout wrote it. There is no migration tooling;
  converting means rewriting one layout with the other.
* **Fail closed on corruption.** An unparsable document, a checksum mismatch, an
  unknown schema, more than `map_max_entries` entries or more than
  `map_max_bytes` bytes makes the whole directory's metadata unusable: reads
  fail, mutations are refused, and the object is **never rewritten**. The error
  names the remedies (`rclone backend repair`, or delete the object to reset the
  directory). `on_corrupt = error` (default) fails the operation;
  `on_corrupt = warn` logs it and serves default attributes while still refusing
  to overwrite the map.
* **The map is the unit of caching and writeback.** `mapStateFor` caches one
  `mapState` per directory (TTL from `cache_ttl`, LRU bounded by
  `cache_max_entries`/`cache_max_size`, single-flight loads). Mutations update
  the in-memory map and bump its generation; the debounced writer serialises
  whole snapshots, one in flight per map (`mapState.wmu` plus generation
  comparison), through a staging object `<prefix>.tmp` and a server-side `Move`
  when the wrapped backend has one. Consecutive updates in one directory
  therefore coalesce into a single upload, and an unchanged map writes nothing.
* **Ordering.** Dirty map state is never evicted, never reloaded, and never
  discarded by cache invalidation; `quiesceAbs`/`quiescePrefix` cancel queued
  timers and wait for an in-flight write before destructive operations, and
  `Rmdir` first removes the directory's own map object (which would otherwise
  make the directory non-empty) and restores it if the removal fails.
* **`repair` backend command.** Dry run by default; `-o dry_run=false
  -o confirm=REPAIR` is required to write. It quarantines a damaged document as
  `<prefix>.corrupt.<RFC3339>` before replacing it, salvages the longest
  parseable run of entries (`salvage`, default) or empties it (`reset`), drops
  invalid names/values, optionally removes keys whose payload vanished
  (`remove_orphans`), and removes stale `<prefix>.tmp` staging objects older than
  `min_age`. It never converts layouts.
* **Symlinks compose unchanged.** `proxy_links = true` stores the target in the
  entry's key, and `List`/`ListR`/`NewObject` synthesise `<name>.rclonelink` from
  the map — one encrypted object for the whole directory of links.
* No `third_party/rclone` change was needed: the backend still implements the
  same `fs` interfaces (`Metadataer`, `SetMetadataer`, `Commander`,
  `DirectoryMover`), and the `--metadata-push` plumbing is unchanged.

## Alternatives considered

* **One global map per remote.** Simple, but every write rewrites the metadata of
  the entire tree, and a single corrupt document would lose every attribute at
  once. Rejected.
* **One map per directory without a checksum.** Saves a few bytes and makes
  silent partial writes (truncated upload, interleaved writer)
  indistinguishable from valid metadata. A directory's metadata is small, so
  the checksum is free.
* **Convert sparse ↔ dir automatically at `NewFs`.** Unbounded work at open,
  races with other writers, and can lose metadata if interrupted. Rejected in
  favour of an explicit mismatch error and operator action.
* **Keep sparse sidecars and only add a per-directory index.** Two sources of
  truth for the same attribute. Rejected.
* **A binary/protobuf document.** Smaller, but undebuggable with `rclone cat`
  and incompatible with the JSON sidecars the repair path already salvages.

## Consequences

* The document header is minimal (`{"v":2,"crc":"<8 hex>",…}`): the version
  alone identifies the layout, and the checksum is bare hex, which saves 29
  bytes per map object. Because the checksum format changed, maps written by an
  older build are treated as corrupt (fail closed, never rewritten) until
  `rclone backend repair` rewrites them; the retired `layout` field itself is
  ignored on read, so its removal alone is forward-compatible.
* A directory with overridden entries costs one metadata object and one read
  instead of one object/read per entry; `List` needs no per-entry prefetch at
  all, and a directory of many proxied symlinks is one object.
* Writes are coarser: any change in a directory rewrites that directory's map (a
  cross-directory rename rewrites two). With a very large directory this is a
  bigger upload than a single sparse sidecar, which is why `map_max_entries` and
  `map_max_bytes` exist.
* A corrupt map costs the metadata of the whole directory (not one entry) until
  it is repaired; that is the price of the checksum-and-layout guarantee.
* Known limitation: the mount's `Attr` swallows `Metadata()` errors, so on a
  corrupt map a cached `stat` shows default attributes while `ls`, `chmod` and
  `NewObject` fail. `on_corrupt=warn` makes that behaviour explicit everywhere.
* Empty non-root maps are removed, so removing the last overridden entry in a
  directory leaves no metadata object behind; the root map stays as the layout
  declaration and costs one object.
* Documented in `AGENTS.md`, `README.md`, `docs/durability-and-scale.md`,
  `docs/mount-pcloud-crypt-posixmeta.md` and `test/README.md`; covered by
  `TestDirLayout*` (`backend/posixmeta/dirmap_test.go`),
  `test_cli_dir_layout_*`, `test_mount_dir_layout_roundtrip` and
  `pm_dir_map_path` in the harness.

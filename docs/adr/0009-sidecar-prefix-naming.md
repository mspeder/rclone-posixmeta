# ADR 0009 — Sidecars are named `<dir>/.posixmeta.<entry>`

* Status: Accepted
* Date: 2026-11-28
* Supersedes: the implicit `<entry>.posixmeta` layout of
  `0001`–`0008`
* Related: `0008-proxy-link-files.md`

## Context

Sidecars used to be named by appending a suffix to the entry name:
`dir/file.txt` was described by `dir/file.txt.posixmeta`. That layout has three
problems:

1. **Links collide with their own metadata.** A symlink is a
   `<name>.rclonelink` object, so its sidecar was `<name>.rclonelink.posixmeta`.
   With `proxy_links` the entry that owns the sidecar is the *visible* name
   (`<name>`), because the link is stored in the sidecar and no payload exists;
   a suffix scheme needs two different rules for the same directory.
2. **Name growth.** Every entry gains 10 characters after its extension, which
   pushes cloud remotes with short filename limits closer to the edge, and hides
   the real extension of the file being described.
3. **Listing pairing is ambiguous.** Suffix stripping cannot distinguish a file
   literally named `x.posixmeta` from the sidecar of `x`; the prefix form has a
   dedicated namespace (`.posixmeta.*`).

The prefix form also makes the sidecar of a directory stay in that directory's
*parent* while its entries' sidecars live inside it, which matches the
"sidecar lives next to its entry" rule: `dir` ↔ `parent/.posixmeta.dir`,
`dir/file` ↔ `dir/.posixmeta.file`.

## Decision

* Replace the `sidecar_suffix` option with `sidecar_prefix` (default
  `.posixmeta`). Sidecars are `<dir>/<prefix>.<name>`; the remote's base root has
  no name, so its own sidecar is the bare `<prefix>` inside that directory,
  while a subpath root keeps the normal form (mounting `pm:dir` stores the
  attributes of `dir` in `<base>/<prefix>.dir`).
* `sidecarPathFor(prefix, remote)` is the single mapping used by reads, writes,
  moves, deletes, `List`/`ListR` pairing, cache priming and ancestor-owner
  lookups. `isSidecar` recognises the bare prefix and `<prefix>.*` in the last
  path component; `sidecarTarget` maps a sidecar back to its entry locally
  (no remote round-trip).
* Setting the removed `sidecar_suffix` option only logs a warning; it is ignored.
* **No migration.** Old `<name>.posixmeta` files are no longer recognised: they
  become ordinary (visible) files, and their metadata is not applied. Operators
  upgrading an existing remote must rename them with the new layout (for
  example `for f in <dir>/*.posixmeta; do mv "$f" <dir>/.posixmeta.$(basename
  "${f%.posixmeta}"); done`) or delete them and reset the metadata. This is a
  deliberate breaking on-disk change: carrying two layouts and two sets of
  pairing rules would make every listing, move and orphan cleanup ambiguous,
  and the project has no deployed remote that needs compatibility.

## Alternatives considered

* **Keep the suffix and special-case links.** `<name>.rclonelink.posixmeta`
  works today, but `proxy_links` needs the sidecar of `<name>`, so a link would
  have to read `<name>.posixmeta` too, and a listing would need to try both
  names per entry. Rejected.
* **`<name>.posixmeta` with a migration pass in `NewFs`.** A remote-wide scan on
  mount is unbounded, races with other writers, and cannot be undone safely.
  Rejected in favour of an explicit operator step (also for encrypted remotes,
  where a rename must go through `rclone`).
* **Make the prefix an arbitrary path (`.meta/info.<name>`).** A subdirectory
  per metadata file doubles the object count and breaks `Rmdir` on directories
  whose entries' sidecars are elsewhere. Rejected.

## Consequences

* `dir/file.posixmeta` → `dir/.posixmeta.file`; `dir`'s own sidecar is
  `parent/.posixmeta.dir`; the wrapped root's sidecar is `.posixmeta`.
* At-rest filenames no longer end in the marker, and a user file literally named
  `.posixmeta` (or `.posixmeta.*`) is reserved and hidden.
* `hide_sidecars = false` now exposes `.posixmeta.*` entries, and
  `Put` rejects writes to any path whose last component matches the prefix
  (including a file that would otherwise be called `.posixmeta`).
* Cache priming still costs zero extra round-trips: the listing already contains
  every `<prefix>.*` sibling, and `sidecarTarget` derives the entry name
  without I/O.
* Documented in `AGENTS.md`, `README.md`, `docs/durability-and-scale.md` and
  `docs/mount-pcloud-crypt-posixmeta.md`; covered by the updated Go tests,
  `test/lib/harness.sh::pm_sidecar_path` and `test_cli_sidecar_prefix_layout`.

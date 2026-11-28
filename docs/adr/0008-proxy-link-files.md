# ADR 0008 — Symlinks stored entirely in the POSIX sidecar (`proxy_links`)

* Status: Accepted
* Date: 2026-11-28
* Supersedes: none
* Related: `0002-fuse-symlinks-via-vfs-links.md`,
  `0005-create-ownership-from-fuse-request.md`, `0009-sidecar-prefix-naming.md`

## Context

With `--vfs-links`, upstream rclone stores a symlink as a regular object called
`<name>.rclonelink` whose payload is the target string. `posixmeta` already
owns a per-entry sidecar that can carry a `symlink` attribute, so the same link
was stored twice at rest: the `.rclonelink` payload (the target) and, whenever
the creator's uid/gid differed from the inherited owner, the metadata sidecar.
On an encrypted cloud remote that is two encrypted objects, two uploads and two
sets of filenames for one link.

The backend already exposes `symlink` through `lsjson --metadata` and
`backend setmeta`, and `docs/adr/0002` explicitly left that attribute unused by
the FUSE path. Making it the actual link storage removes the duplication without
touching the VFS: all of the `.rclonelink` handling (`fs.LinkSuffix`,
translation of listings, cache paths) lives above the backend.

## Decision

Add the opt-in option `proxy_links` (default `false`) to the `posixmeta` remote.

* The sidecar is the *only* storage for a link. `Put`/`PutUnchecked` of a
  `<name>.rclonelink` remote reads the target (bounded to 64 KiB, rejecting
  empty targets and embedded NUL bytes), merges the upload's POSIX metadata and
  writes `<dir>/.posixmeta.<name>` synchronously. No payload is uploaded. The
  write is synchronous because the sidecar is the link: returning success for
  `ln -s` before the target is stored would lose the link.
* `List`, `ListR` and `NewObject` publish a virtual `<name>.rclonelink` object
  whose payload is the sidecar's `symlink`, whose `Size` is the target length,
  whose hash is the hash of the target and whose mtime is the sidecar object's
  mtime. Listings synthesise it only when the entry has no payload
  (`dir/link.txt` absent) or when the sidecar shadows a stale
  `<name>.rclonelink` payload; a real payload for the visible name always wins,
  so ordinary files keep their behaviour.
* `Remove`, `Move` and `Copy` of the virtual object move/delete the sidecar.
  Renaming to a non-link name materialises the target as a regular file.
* `backend setmeta -o symlink=...` creates a link when no payload exists.
  Clearing `symlink` turns the sidecar back into plain metadata (and removes it
  when nothing else is set), so the link disappears.
* `NewFs` mirrors `fs.ErrorIsFile` for a link root by probing the sidecar, since
  there is no payload for the wrapped backend to detect.

The default is `false`: existing remotes keep the upstream two-object link
representation, and a literal file named `*.rclonelink` keeps being an ordinary
file. With `proxy_links = true`, a literal `*.rclonelink` payload is
reinterpreted as a link only when a sidecar for the same visible name carries a
`symlink` attribute (see `docs/adr/0009` for the at-rest layout change).

## Alternatives considered

* **Patch the VFS so `--vfs-links` uses `symlink` metadata instead of payloads.**
  Would couple the fork to one backend's private metadata, break
  `local`/`crypt`/cloud mounts, and require changing `vfs`, `cmd/mount` and
  `operations`. The backend already controls its own listings and `Put`.
* **Always proxy links (no option).** Legacy `.rclonelink` payloads and literal
  files with that suffix would change meaning for every existing remote.
* **Store the target in a separate `<name>.symlink` object.** Still two objects,
  and it adds a third file kind to hide.

## Consequences

* One encrypted object per link at rest with `proxy_links = true`, and no
  `.rclonelink` payload to hide from listings or to move separately.
* `rclone cat pm:dir/link.rclonelink` returns the target either way; `--vfs-links`
  keeps working because the backend still advertises the `.rclonelink` name.
* Reading a link costs the sidecar (cached, prefetched from listings) instead of
  the VFS cache file; `ModTime` and file hashes follow the sidecar.
* `SetModTime` on a link is a no-op; a link's mtime is its sidecar's mtime.
* Covered by `TestProxyLink*` Go tests plus the `proxy_links` cases in
  `test/suites/01-cli.sh` and `test/suites/02-mount-crypt.sh`.

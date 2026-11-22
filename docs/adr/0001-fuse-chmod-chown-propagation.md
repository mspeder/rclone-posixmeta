# ADR 0001 — FUSE chmod/chown/stat propagation via a local rclone fork

## Status

Accepted (2026-11-22)

## Context

`backend/posixmeta` stores POSIX `mode`, `uid` and `gid` in sparse
`.posixmeta` sidecars and implements `fs.Metadataer`, `fs.SetMetadataer`,
`Chmod` and `Chown`. `rclone backend setmeta` and `lsjson --metadata` therefore
work, but inside an `rclone mount`:

* `Attr` (getattr/stat) synthesized `mode`/`uid`/`gid` from the mount-wide
  `--file-perms`, `--dir-perms`, `--uid` and `--gid` flags, ignoring per-entry
  metadata.
* `Setattr` (kernel `chmod`/`chown`) only handled mtime and size; `VFS.Chmod`
  and `VFS.Chown` returned `ENOSYS` and the FUSE handlers ignored the
  requests.

The relevant code lives in the external module `github.com/rclone/rclone`
(v1.76.0). Upstream still has this gap (verified against `master`), so the
behaviour cannot be fixed from the backend package alone: `cmd/mount` and
`vfs` both need changes.

## Decision

Fork rclone into the repository and consume it with a `replace` directive:

```go
replace github.com/rclone/rclone => ./third_party/rclone
```

Patch scope (kept minimal so the fork can be refreshed from upstream):

* `vfs/file.go`, `vfs/dir.go` — add
  `Metadata`/`SetMetadata`/`Chmod`/`Chown` on `File` and `Dir`.
  * Queued (`pendingMetadata`) writes for files that are still uploading;
    applied from `setObject`.
  * `Dir.backendEntry` refreshes synthetic `fs.NewDir` entries created by
    `Mkdir`/`Rename` from the backend so the first `chmod` after `mkdir`
    works.
* `vfs/vfs.go` — `VFS.Chmod`/`Chown`/`Metadata` dispatch to nodes, returning
  `ENOSYS` when the backend has no metadata support.
* `cmd/mount/file.go`, `cmd/mount/dir.go` — overlay `mode`/`uid`/`gid` from
  `Metadata` in `Attr`; forward `Mode`/`Uid`/`Gid` from `Setattr`, ignoring
  `ENOSYS` so non-metadata backends keep the previous no-op behaviour.

## Alternatives considered

* **Overlay file (`go build -overlay`) instead of a fork** — no tree copy, but
  requires every build invocation (including `go test`, `go run` and editor
  tooling) to pass generated, GOMODCACHE-dependent paths. Rejected as fragile.
* **`go mod vendor` and patch `vendor/`** — vendors every transitive
  dependency (hundreds of MB) instead of just the 49 MB rclone module.
* **Wait for an upstream fix** — blocks the feature; patches are small and
  isolated, so carrying them is cheap.

## Consequences

* `rclone` upgrades require re-applying (or upstreaming) the patch and
  re-running the `02-mount-crypt` suite.
* `cmount`/`mount2` (`cmd/cmount`) is intentionally out of scope; `rclone
  mount` (bazil FUSE, Linux) is the supported path.
* Chown of the mount root is not propagated (the VFS root is a synthetic
  directory with no backend entry).
* Cross-process metadata edits remain invisible in a running mount until
  `cache_ttl` expires (backend cache semantics, not a VFS issue).

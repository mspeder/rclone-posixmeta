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

A naive fix is not acceptable as a default, though: `Attr` would consult any
`fs.Metadataer`, and `local` (as well as `crypt` over `local`) reports real
`mode`/`uid`/`gid`. Mounting `local:` would then stop honouring
`--file-perms`/`--dir-perms`, and `chmod`/`chown` inside the mount would mutate
the backing files — a breaking change for every existing `rclone mount` user.
The VFS/FUSE behaviour therefore has to be explicitly opt-in.

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
  `ENOSYS` so non-metadata backends keep the previous no-op behaviour. Both
  paths are gated on the new `--metadata-push` mount option (default off).
* `cmd/mountlib/mount.go` — `--metadata-push` option, plus a preflight: a
  backend implementing `RequiresMetadataPush() bool` is rejected with a clear
  error when the mount lacks the flag. `posixmeta` returns true, so it cannot
  be mounted in a silently degraded state (stat shows mount defaults, chmod
  dropped). Backends without the method (`local`, `crypt`, S3, ...) are
  unaffected and keep stock behaviour by default; passing `--metadata-push`
  opts them in explicitly.

## Alternatives considered

* **Overlay file (`go build -overlay`) instead of a fork** — no tree copy, but
  requires every build invocation (including `go test`, `go run` and editor
  tooling) to pass generated, GOMODCACHE-dependent paths. Rejected as fragile.
* **`go mod vendor` and patch `vendor/`** — vendors every transitive
  dependency (hundreds of MB) instead of just the 49 MB rclone module.
* **Wait for an upstream fix** — blocks the feature; patches are small and
  isolated, so carrying them is cheap.

## Consequences

* Default `rclone mount` behaviour is unchanged: no metadata reads in getattr
  and no write-through unless `--metadata-push` is given, so mounting `local:`
  or `crypt(local):` still honours `--file-perms`/`--dir-perms` and drops
  `chmod`/`chown` as before.
* `posixmeta` mounts require `--metadata-push`; without it the mount fails fast
  with an actionable error instead of silently ignoring the stored metadata.
* `rclone` upgrades require re-applying (or upstreaming) the patch and
  re-running the `02-mount-crypt` suite.
* `cmount`/`mount2` (`cmd/cmount`) is intentionally out of scope; `rclone
  mount` (bazil FUSE, Linux) is the supported path. `cmount` accepts
  `--metadata-push` but does not implement the overlay.
* Chown of the mount root is not propagated (the VFS root is a synthetic
  directory with no backend entry).
* Cross-process metadata edits remain invisible in a running mount until
  `cache_ttl` expires (backend cache semantics, not a VFS issue).

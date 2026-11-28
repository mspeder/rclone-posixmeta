# ADR 0005 — New entries belong to the creating process

## Status

Accepted (2026-11-22). The "mode is not stamped" part of this decision is
superseded by `docs/adr/0011-create-mode-from-fuse-request.md`, which stamps the
kernel-provided mode (and inherits `S_ISGID` onto new subdirectories) so the
mount behaves like ext4. Ownership stamping as described here is unchanged.

## Context

On a regular filesystem the owner of a newly created inode is the effective uid
of the process performing the create; the group is its effective gid, unless the
parent directory has `S_ISGID`, in which case the parent's group is used. The
mount user and the parent directory owner play no part.

`posixmeta` stores `uid`/`gid` in sparse `.posixmeta` sidecars and, with
`inherit_parent_owner = true` (the default), resolves the ownership of any entry
without its own sidecar from the nearest ancestor directory sidecar. New entries
created through `rclone mount` had no sidecar *and* no creator credentials, so:

```bash
mkdir test && chown hello:hello test && cd test && touch newfile
stat -c '%U:%G' newfile      # hello:hello, not the creating user
```

The FUSE kernel request already carries the creator's credentials
(`bazil.org/fuse`: `Header.Uid`/`Header.Gid` on `CreateRequest`,
`MkdirRequest`, `MknodRequest` and `SymlinkRequest`), but `cmd/mount/dir.go`
discarded them: `Create`/`Mkdir`/`Symlink` never recorded ownership and `Mknod`
even rebuilt a `CreateRequest` without `Header`. The backend therefore could not
know who created the entry.

## Decision

Patch `cmd/mount/dir.go` so that, when `--metadata-push` is set, the new entry is
stamped with the creating process's `uid`/`gid` from the request header:

* `Create` → `file.SetMetadata` after `Dir.Create`/`Open` (the VFS queues it in
  `pendingMetadata` while the upload is in flight and applies it from
  `setObject`, so it lands on the backend object).
* `Mkdir` → `dir.SetMetadata` (the synthetic VFS entry is refreshed from the
  backend first, see `Dir.backendEntry`).
* `Mknod` → forwards `req.Header` into the synthetic `CreateRequest`.
* `Symlink` → ownership only; the mode still comes from `--link-perms`
  (`S_IFLNK`), because Linux ignores `chmod` on links.

`posixCreateMetadata` (in `cmd/mount/posixmeta.go`) builds the metadata:

* `uid` = creator;
* `gid` = creator, unless the parent directory's resolved mode has
  `os.ModeSetgid`, in which case the parent's `gid` is used.

Failures are logged and ignored (`ENOSYS` silently) so backends without metadata
support keep their previous behaviour and a transient remote error never fails a
create that already produced the payload.

Deliberately out of scope (see however
`docs/adr/0011-create-mode-from-fuse-request.md`, which now stamps the mode and
propagates `S_ISGID` to new subdirectories):

* **Mode stamping was originally left out.** This ADR only recorded ownership;
  ADR 0011 later added `mode` from `req.Mode` so `mkdir -m`/umask and setgid
  inheritance work as on ext4.

`inherit_parent_owner` keeps its meaning for entries that have no recorded
creator — `rclone copy`/`sync`, objects written before this change, and any
backend write that carries no POSIX metadata.

## Alternatives considered

* **Documentation-only (`inherit_parent_owner = false`).** New entries would
  report the mount process's uid/gid, but every entry would, including those
  created by another local user through `--allow-other`, and chowning a
  directory would no longer cover its subtree.
* **Content-only alternative: inherit only for setgid directories** (a new
  `inherit_parent_owner` mode). Fixes the symptom without touching the fork, but
  still cannot express "the creator owns it", cannot see the real caller, and
  changes the meaning of an existing option.
* **Stamp the mode too.** Rejected at the time (it makes the reported
  permissions depend on the caller's umask and conflicts with the existing
  `--file-perms`/`default_file_mode` policy); revisited and adopted in
  `docs/adr/0011-create-mode-from-fuse-request.md` for ext4 fidelity.

## Consequences

* `touch`/`cp`/`mkdir`/`ln -s` inside a directory owned by another user now
  report the creating process, matching a regular filesystem.
* When the creator differs from the inherited owner, a sparse sidecar is
  written (`uid`/`gid` only), exactly like any other override. In the common
  single-user case the stamp equals the default and stays sparse.
* A setgid directory still forces its group on new entries, so service
  directories behave as expected.
* Still opt-in: without `--metadata-push` the fork keeps stock rclone behaviour
  and creates nothing.
* One extra cached metadata lookup per create (to read the parent mode for the
  setgid rule); `List` priming and the sidecar cache keep it a cache hit in
  practice.
* Residual: a racing create (file appearing between the kernel's lookup and the
  create) can stamp the loser's credentials on the winner's object. Two
  concurrent creators of the same name are already unsupported.
* Covered by `test_mount_create_records_creating_process`,
  `test_mount_create_in_setgid_dir_inherits_group`,
  `test_mount_roundtrip_metadata_inheritance_and_encryption` (creator vs copied
  entry) and `test_mount_symlinks_via_vfs_links`.

# ADR 0011 — New entries get the mode the kernel asked for (ext4 semantics)

## Status

Accepted (2026-11-28). Supersedes the "mode is not stamped" scope decision of
`docs/adr/0005-create-ownership-from-fuse-request.md`.

## Context

ADR 0005 made `--metadata-push` record the creating process's `uid`/`gid` on new
entries but deliberately left `mode` alone: a new entry kept
`--file-perms`/`--dir-perms` (i.e. `default_file_mode`/`default_dir_mode` once
resolved) and the parent's `S_ISGID` was only used to pick the group.

That produces two visible deviations from a regular filesystem:

* `mkdir -m 0700` / a caller umask other than the mount's default are silently
  ignored — `stat` reports `default_dir_mode` instead of what the caller asked
  for.
* A subdirectory created in a setgid directory does **not** get `S_ISGID`
  (Linux propagates the bit to subdirectories only), so a file created inside it
  falls back to the creator's primary group instead of the setgid directory's
  group:

  ```bash
  mkdir dir1 && chgrp cms dir1 && chmod g+s dir1   # dir1 = 2775 matthieu:cms
  touch dir1/file1                                 # matthieu:cms, as expected
  mkdir dir1/dir2                                  # dir2 was 0755 => not setgid
  touch dir1/dir2/file2                            # matthieu:matthieu, wrong
  ```

Ext4 gets this right in `inode_init_owner()`: the new mode is the `mkdir(2)`/
`open(2)` mode masked by the caller's umask, the group comes from the parent when
it is setgid, and `S_ISGID` is copied to new **directories** only (never to
regular files).

## Decision

Stamp the mode too, straight from the FUSE request:

* `Create`/`Mknod` → `mode` = `req.Mode`;
* `Mkdir` → `mode` = `req.Mode`, plus `S_ISGID` when the parent's resolved mode
  has it;
* `Symlink` → ownership only, as before (`--link-perms`/`S_IFLNK` wins, Linux
  ignores `chmod` on links).

`posixCreateMetadata` (`cmd/mount/posixmeta.go`) keeps building a single
`fs.Metadata` (creator uid, creator/parent gid, mode) so one `SetMetadata` — one
sparse sidecar — is written per create.

No umask handling is needed in the daemon: bazil/fuse never negotiates
`FUSE_DONT_MASK`, so the kernel has already applied the caller's umask to
`req.Mode` before sending the request (that is also why `req.Umask` is
informational here). `modeToMetadata` keeps setuid/setgid/sticky.

## Why `default_file_mode`/`default_dir_mode` stay

They are not made redundant by this change; they answer a different question:

* they are the **resolved mode of entries that carry no metadata of their own** —
  `rclone copy`/`sync` writes, objects created by other clients, and trees that
  predate this backend (with `--metadata-push`, `posixmeta` always returns a
  mode, so they are also what `stat` shows for those entries; the mount's
  `--file-perms`/`--dir-perms` are overridden by that overlay);
* they are the **sparseness baseline**: `buildSparseSidecar` drops `mode` when it
  equals the default, so a default-umask create writes no sidecar at all. With
  `default_file_mode = 0644`/`default_dir_mode = 0755` and `umask 022`, `touch`
  and `mkdir` therefore cost exactly what they did before this change — the
  fidelity is free in the common case.

## Alternatives considered

* **Propagate `S_ISGID` only, keep modes from the config** (the minimal fix).
  Keeps `mkdir -m` and a non-default umask silently ineffective, i.e. the mount
  is still not ext4-like.
* **Copy the parent's permission bits onto new subdirectories (2775).** Not what
  Linux does: only the `S_ISGID` bit is inherited; the permission bits come from
  the requested mode and umask.
* **Rewrite the umask locally (`req.Mode &^ req.Umask`).** Wrong when the kernel
  already masked the mode, and it would double-apply the umask on kernels that
  negotiated `FUSE_DONT_MASK`.

## Consequences

* `mkdir -m 0700`, `umask 077` and nested setgid directories now behave as on
  ext4; the reported mode is what the caller asked for (see the tests below).
* Any create whose umask/default combination differs from
  `default_file_mode`/`default_dir_mode` writes a sparse sidecar (one extra
  object per entry on a cloud remote). With the default configuration
  (`0644`/`0755`, `umask 022`) nothing changes.
* Files created through the mount no longer follow `--file-perms`; that flag —
  like `--dir-perms` — only remains as a `getattr` fallback for entries whose
  backend reports no metadata (non-posixmeta remotes).
* `rclone copy`/`sync` and other clients still resolve to
  `default_file_mode`/`default_dir_mode`, because they carry no creator and no
  mode. A mixed tree is expected.
* Entries created under a setgid parent keep `S_ISGID` even if the parent's bit
  is removed later: like ext4, the bit is copied at creation, not inherited
  dynamically.
* A `CREATE` that reopens an existing object (the kernel can send it for a name
  whose cached dentry was negative) does **not** restamp it: `stampCreate`
  skips files that already report metadata, so a re-opener's umask/mode can
  never overwrite a recorded `chmod`/`chown`. Files written again through an
  open handle go through `overwrite_metadata = preserve` as before.
* `--metadata-permissions` (the kernel enforcing the reported mode) applies to
  the stamped mode, so a `umask 077` file created by another user is not
  readable by the mount user — same as a local filesystem.
* Test harness: `test/run.sh` pins `umask 022` so the expected modes do not
  depend on the developer's shell.
* Covered by `02-mount-crypt` tests
  `test_mount_create_honours_requested_mode` (umask, `mkdir -m`, and
  sparseness), `test_mount_create_in_setgid_dir_inherits_group` (S_ISGID on the
  subdirectory, one level down file group, `mkdir -p` levels) and the
  `sidecar_layout = dir` block of `test_mount_dir_layout_roundtrip`.

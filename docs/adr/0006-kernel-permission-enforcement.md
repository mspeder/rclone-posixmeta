# ADR 0006 — The kernel enforces the POSIX metadata we report

## Status

Accepted (2026-11-22)

## Context

With `--metadata-push`, `stat` inside an `rclone mount` reports the per-entry
`mode`, `uid` and `gid` stored by `posixmeta` (for example a directory with
`mode=0700 uid=1050`). Those attributes were purely cosmetic:

```bash
ls -ld /mnt/pm/private        # drwx------ 1050 1060 private
cd /mnt/pm/private            # succeeds as an unrelated user
cat /mnt/pm/private/secret    # succeeds even with mode=0600
```

`cmd/mount` does not implement `fusefs.NodeAccesser`, so go-fuse answers
`FUSE_ACCESS` with `ENOSYS`; the Linux kernel then stops sending access
requests and lets every path lookup, open and read through. `default_permissions`
was only passed when the user added the upstream `--default-permissions` flag,
which is off by default and was never implied by `--metadata-push`.

The result was a silent security gap specific to this fork's use case: the whole
point of `posixmeta` is that the metadata is real, yet `chmod 0700` did not keep
anybody out of the directory.

## Decision

`cmd/mount` passes `fuse.DefaultPermissions()` when either:

* `--default-permissions` is set (unchanged upstream behaviour), or
* `--metadata-push` is set **and** the new `--metadata-permissions` mount option
  is true (the default).

The kernel then checks the attributes produced by `Attr`, which are exactly the
ones the mount reports, using the caller's credentials, supplementary groups and
capabilities (including the root bypass). No `NodeAccesser` implementation is
needed, no permission logic is duplicated in Go, and `access(2)`, path traversal,
`open(2)`, `unlink`/`rename` all behave like a local filesystem.

`--metadata-permissions=false` restores the previous permissive behaviour for
users who deliberately want a mount that ignores the recorded modes (for example
a backup service that must read every entry regardless of its recorded owner).

Without `--metadata-push` nothing changes: stock rclone behaviour is preserved
for `local`, `crypt` and cloud mounts, which continue to rely on the
`--file-perms`/`--dir-perms`/`--uid`/`--gid` defaults.

## Alternatives considered

* **Implement `NodeAccesser` on `Dir`/`File`.** Covers `access(2)` and path
  lookup, but `open(2)`/`unlink(2)` never send `FUSE_ACCESS`; enforcing those too
  would mean re-implementing the kernel's permission model (supplementary groups,
  capabilities, setgid inheritance, `MAY_*` masks) in every operation. More code,
  easy to get wrong, and it still would not cover other users' group membership
  correctly. Rejected.
* **Require `--default-permissions` in the mount preflight.** Correct but breaks
  every existing `--metadata-push` mount at upgrade time for what is a security
  fix, and the flag name gives no hint that it is about *stored* metadata.
  Rejected in favour of enabling it and offering an explicit opt-out.
* **Leave the default permissive and only document the flag.** This is what the
  code did; a mount that silently ignores `0700` is a vulnerability, not a
  preference. Rejected.
* **Enforce in the `posixmeta` backend.** The backend never sees the caller's
  credentials; only the FUSE request header carries them. Not possible.

## Consequences

* `cd`, `open` and read access through the mount now honour the recorded
  `mode`/`uid`/`gid`, including entries created on another machine or by
  `rclone copy`/`backend setmeta` with a foreign owner.
* The common single-user case is unaffected: `posixmeta`'s `default_uid`/
  `default_gid` default to the mounting process's ids, and entries created
  through the mount are stamped with their creator, so the mount user keeps
  full access to its own files. `--uid`/`--gid` still provide the attributes for
  the mount root, whose sidecar cannot be written through the mount.
* Writing inside a directory that is owned by somebody else now requires the
  corresponding `w`/`x` bits — after `backend setmeta pm:dir -o uid=1050`,
  `touch pm-dir/file` as the mount user fails unless the directory is
  group/world writable (e.g. `mode=0777`) or the caller's group matches.
  `chmod`/`chown`/`rm` on a foreign-owned file likewise follow normal POSIX
  rules (`rm` may warn that a `0600` file is write protected; `rm -f` unlinks
  it because only the parent directory's write bit matters).
* Metadata changed by another process stays invisible until `cache_ttl` expires
  in the mount process, so a `backend setmeta` chown is enforced only once the
  mount refreshes its cached attributes (same residual as for `stat`).
* `--metadata-permissions=false` is the escape hatch; it does not disable the
  metadata overlay, only its kernel enforcement. It does **not** make a
  foreign-owned entry writable by `chmod`/`chown`: the VFS still requires
  ownership or `CAP_FOWNER` for `setattr`.
* Admin access needs `--allow-other`. FUSE refuses every request whose
  uid/gid does not match the mount owner (`user_id=`/`group_id=` in
  `/proc/self/mounts`) unless the mount was created with `allow_other`;
  `fuse_setattr()` returns `-EACCES` from `fuse_allow_current_process()`
  before the daemon runs. Consequently `sudo chmod`/`sudo chown` on a mount
  created by a normal user fail with `Operation not permitted`/`Permission
  denied`, even though root holds `CAP_FOWNER`. `--allow-other` plus
  `user_allow_other` in `/etc/fuse.conf` restores the normal root bypass (the
  kernel's `default_permissions` check honours `CAP_DAC_OVERRIDE`/`CAP_FOWNER`).
  The alternative is out-of-band repair with `backend setmeta` from another
  process (visible once the mount's `cache_ttl` expires), or mounting as root.
* The `cmount`/`mount2` path (`cmd/cmount/`) is unaffected: it implements
  `Access` as a no-op and is not part of the supported topology.
* Covered by `test_mount_enforces_recorded_permissions` (traversal, listing and
  read denial, the permissive escape hatch, and the presence of
  `default_permissions` in the mount options). Existing mount tests were
  adjusted to create entries before handing their directory to another user, or
  to make that directory world-writable.

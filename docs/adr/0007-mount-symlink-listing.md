# ADR 0007 — A symlink created through the mount is listed immediately

* Status: Accepted
* Date: 2026-11-22
* Supersedes: none
* Related: `0002-fuse-symlinks-via-vfs-links.md`,
  `0005-create-ownership-from-fuse-request.md`

## Context

With `--vfs-links`, `ln -s` inside an `rclone mount` succeeded and
`readlink`/`stat` worked, but the new link stayed invisible to `ls`:

```bash
mount_pm --dir-cache-time 500ms &
ls /mnt/pm/dir                 # (empty)
ln -s target /mnt/pm/dir/link  # returns 0
ls /mnt/pm/dir                 # still empty
sleep 0.6 && ls /mnt/pm/dir    # link appears
```

Regular files created with `Create` were visible in a few milliseconds, so the
backend, the VFS writeback and the FUSE node were all fine; only symlinks were
affected. `cmd/mount.Dir.Symlink` built the full path with
`path.Join(d.Path(), req.NewName)` and called `d.VFS().CreateSymlink(target,
newName)`, which re-resolved the parent (`vfs.StatParent`) and returned a
`*vfs.Dir` from the VFS tree — not necessarily the receiver `d.Dir`.

When the parent's cached children are dropped — `Dir.ForgetAll` runs on
`--dir-cache-time` expiry (`cacheCleanup`) and replaces `d.items` with a fresh
map whenever the directory has no virtual entries — a later `VFS.StatParent`
builds a *new* `*vfs.Dir` instance for the parent. `Dir.Create` then registers
the new file in that instance's `virtual` map, while the `Dir` the kernel holds
as a FUSE node (and whose `ReadDirAll` answers `ls`) is the old one. The entry
therefore cannot be listed until *that* instance's listing is refreshed again —
i.e. after another full `--dir-cache-time`. `Create`, `Mkdir`, `Mknod`,
`Remove` and `Rename` do not have the problem because they already act on the
receiver.

This is an upstream bug, confirmed against rclone v1.76.0 and current master,
and it is independent of `posixmeta`: it reproduces with `local` and `crypt`.

## Decision

Move the link-creation logic into the VFS as an exported method on the
directory that owns the entry:

* `vfs/dir.go`: new `(*Dir).CreateSymlink(oldname, leaf string) (Node, error)`
  containing what `VFS.CreateSymlink` used to do after resolving the parent
  (`--links` check, `EEXIST` check on the cached listing, `Dir.Create` with
  `o_SYMLINK`, `setSymlink`, then writing the target). It is intentionally
  named after the existing `(*Dir).Create`, `Mkdir`, `Remove`, `Rename`
  family.
* `vfs/vfs.go`: `VFS.CreateSymlink` keeps its signature and behaviour but is
  now `StatParent` + `dir.CreateSymlink(oldname, leaf)`. Its other callers
  (`cmd/mount2`, `cmd/cmount`, `cmd/serve/nfs`) are unaffected by design: they
  do not cache FUSE nodes.
* `cmd/mount/dir.go`: `Dir.Symlink` calls `d.Dir.CreateSymlink(req.Target,
  req.NewName)`, so the virtual entry is always registered on the node the
  kernel is reading. The now-unused `path` import is dropped.

The VFS method keeps upstream's observable semantics (including the
`EEXIST` decision being made against the cached listing, exactly like
`Dir.Create`). The *write* error of the target payload is still discarded as
upstream did (only `Close` is checked via the deferred `fs.CheckClose`), because
changing that would alter error reporting beyond the scope of this fix.

The fix ships in its own patch, `third_party/patches/0001-mount-symlink-listing.patch`,
which contains no `posixmeta`-specific code: it is meant to be submitted
upstream as-is. `0002-posixmeta-metadata.patch` is generated on top of it (both
touch `Dir.Symlink`), so the order in `third_party/patches/` is significant.
Once the fix is released in an upstream rclone version, `0001` is deleted and
only `0002` is rebased.

## Alternatives considered

* **Have `cmd/mount` create the entry with `Dir.AddVirtual`.** `AddVirtual` is
  designed for entries that exist only in the VFS cache (e.g. `.rclonelink`
  staging during uploads) and does not create a backend object. Reusing it would
  mean reimplementing `CreateSymlink`'s upload path in `cmd/mount` and would
  leave `VFS.CreateSymlink` broken for every other caller. Rejected.
* **Call `d.VFS().CreateSymlink` and then fix up the returned node.** The
  returned `*vfs.File` does not expose its parent directory, so `cmd/mount`
  cannot re-register it on `d.Dir`, and the VFS methods that would be needed
  (`addObject`, `addVirtual`) are unexported. That would require more VFS
  surface than the new method. Rejected.
* **Invalidate the parent listing after `CreateSymlink`.** Forcing a
  `ForgetAll` on the receiver would make `ls` reload from the backend, which
  does not contain the new link yet (`--vfs-cache-mode` uploads are deferred
  and `Dir.Create` is virtual), so the link would still be missing and the
  directory would lose its whole cache on every `ln -s`. Rejected.
* **Fix it in `posixmeta`/the backend.** The backend never sees directory
  cache state; the entry is lost inside the VFS. Not possible.
* **Keep one combined patch.** Rejected: the upstream fix must be reviewable
  and upstreamable on its own, and removable without touching the `posixmeta`
  overlay.

## Consequences

* `ln -s` is visible in `ls` immediately (measured ~4 ms instead of one full
  `--dir-cache-time`, 10/10 runs with `--dir-cache-time 500ms`).
* No behaviour change for `mount2`, `cmount`, `serve nfs`, or for mounts
  without `--vfs-links`: the argument validation, `EEXIST` and `ENOSYS`
  semantics are the same, only the parent resolution moved.
* The `EEXIST` check still uses the parent's cached listing, like `Dir.Create`;
  a link created out-of-band can therefore be overwritten if the listing is
  stale and the backend allows it. This is pre-existing upstream behaviour for
  every create operation through the mount, not introduced here.
* Covered by `test_mount_symlink_listed_immediately` (populate the directory,
  drop the whole VFS directory cache with `vfs/forget` over `--rc --rc-no-auth`
  on a unix socket, create the link, require it in `ls` with no polling) and
  `test_mount_symlink_listed_after_dir_cache_expiry` (the same scenario without
  `--rc`, in ten fresh subdirectories, using a real `--dir-cache-time` expiry).
  Both fail on the unfixed tree and pass with patch `0001`.

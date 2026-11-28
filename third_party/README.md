# third_party

## `rclone/` — local rclone fork (not versioned)

`rclone-posixmeta` needs a handful of small VFS/FUSE changes that are not
upstream in rclone v1.76.0 (see `docs/adr/0001-fuse-chmod-chown-propagation.md`).
They live in a local fork wired up by:

```go
// go.mod
replace github.com/rclone/rclone => ./third_party/rclone
```

The fork is a complete 49 MB copy of the upstream release, so it is **not
committed**. Only the unified diffs are versioned, in `third_party/patches/`,
and they are applied in lexical order:

* `0001-mount-symlink-listing.patch` — the upstream-facing fix: a symlink
  created through `rclone mount --links` is missing from `ls` until
  `--dir-cache-time` expires. `cmd/mount` re-resolved the parent through
  `VFS.CreateSymlink`, which can hand back a different `*vfs.Dir` instance
  after a directory cache forget, so the new virtual entry was added to a node
  the kernel never reads. The patch adds `(*vfs.Dir).CreateSymlink`,
  makes `VFS.CreateSymlink` a thin wrapper over it, and has `Dir.Symlink` use
  the receiver (see `docs/adr/0007-mount-symlink-listing.md`). It touches only
  `vfs/dir.go`, `vfs/vfs.go` and `cmd/mount/dir.go` and contains nothing
  posixmeta-specific, so it can be submitted upstream as-is and deleted once
  released.
* `0002-posixmeta-metadata.patch` — the posixmeta-specific metadata overlay:
  `vfs/file.go`, `vfs/dir.go`, `cmd/mount/mount.go`, `cmd/mount/file.go`,
  `cmd/mount/dir.go`, `cmd/mountlib/mount.go`, plus new
  `cmd/mount/posixmeta.go`. It is generated on top of `0001` (both patches
  touch `Dir.Symlink`), hence the ordering.

The `cmd/mount` overlay skips the backend `mode` for symlink nodes (upstream
`--link-perms`/`S_IFLNK` must win; see
`docs/adr/0002-fuse-symlinks-via-vfs-links.md`) and still applies `uid`/`gid`.
`cmd/mount/posixmeta.go` also builds the metadata stamped on newly created entries
(`Create`/`Mkdir`/`Mknod`/`Symlink`) from the FUSE request: the caller's
`uid`/`gid` (setgid-directory group inheritance) and the kernel-provided `mode`,
which is already masked by the caller's umask, plus `S_ISGID` inherited onto new
subdirectories only — see `docs/adr/0005-create-ownership-from-fuse-request.md`
and `docs/adr/0011-create-mode-from-fuse-request.md`. Modes equal to
`default_file_mode`/`default_dir_mode` stay sparse (no sidecar), so a default
umask still writes nothing extra.

Both patches stay deliberately small: the only new VFS surface in `0002` is
`File`/`Dir`.`Metadata` and `SetMetadata`. `VFS.Chmod`/`VFS.Chown` are untouched
(they still return `ENOSYS` exactly like upstream), so other VFS users
(`rclone serve nfs`, `mount2`, `cmount`) keep their stock behaviour even for
backends such as `local` which implement `fs.SetMetadataer`. Everything else is
gated on the mount's `--metadata-push` option.

### Materialise / refresh the fork

```bash
./scripts/setup-fork.sh          # no-op if already present
./scripts/setup-fork.sh --force  # rebuild from scratch
make fork                        # same, via the Makefile
```

The script reads the rclone version from the single-line `require` in
`go.mod`, downloads that exact module version with `go mod download`, copies it
to `third_party/rclone/`, and applies every `third_party/patches/*.patch` with
`patch -p1` in lexical order (the order is significant: `0002` is a diff
against the tree with `0001` applied).

`make build`, `make test` and `test/run.sh` invoke it automatically, so a fresh
clone only needs a Go toolchain and `patch`. The stamp
(`third_party/rclone/.posixmeta-fork`) records the applied patches and the
pinned version; the script is a no-op only while the stamp is newer than every
patch and `go.mod`, so editing a patch or bumping rclone rebuilds the fork.

### Regenerating the patches

After editing the fork (e.g. when bumping rclone), regenerate both diffs from a
pristine copy:

```bash
SRC="$(go env GOMODCACHE)/github.com/rclone/rclone@v1.76.0"

# 0001: only the upstream symlink-listing fix
{ for f in vfs/dir.go vfs/vfs.go cmd/mount/dir.go; do
    diff -u --label "a/$f" --label "b/$f" "$SRC/$f" "third_party/rclone/$f" || true
  done
} > third_party/patches/0001-mount-symlink-listing.patch

# 0002: everything else, diffed against pristine + 0001
rm -rf /tmp/pm-step1 && cp -a "$SRC" /tmp/pm-step1 && chmod -R u+w /tmp/pm-step1
patch -p1 -d /tmp/pm-step1 < third_party/patches/0001-mount-symlink-listing.patch
{ for f in vfs/file.go vfs/dir.go cmd/mount/mount.go cmd/mount/file.go cmd/mount/dir.go cmd/mountlib/mount.go; do
    diff -u --label "a/$f" --label "b/$f" "/tmp/pm-step1/$f" "third_party/rclone/$f" || true
  done
  diff -uN --label /dev/null --label b/cmd/mount/posixmeta.go /dev/null \
    third_party/rclone/cmd/mount/posixmeta.go || true
} > third_party/patches/0002-posixmeta-metadata.patch
```

Then rebuild the fork from the patches (`./scripts/setup-fork.sh --force`),
confirm that applying both to a pristine copy reproduces `third_party/rclone/`
(`diff -r`), and run `./test/run.sh`.

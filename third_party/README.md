# third_party

## `rclone/` — local rclone fork (not versioned)

`rclone-posixmeta` needs four small VFS/FUSE changes that are not upstream in
rclone v1.76.0 (see `docs/adr/0001-fuse-chmod-chown-propagation.md`). They live
in a local fork wired up by:

```go
// go.mod
replace github.com/rclone/rclone => ./third_party/rclone
```

The fork is a complete 49 MB copy of the upstream release, so it is **not
committed**. Only the unified diff is versioned:

* `rclone-posixmeta.patch` — diff of the modified files against pristine
  `github.com/rclone/rclone@v1.76.0`
  (`vfs/vfs.go`, `vfs/file.go`, `vfs/dir.go`, `cmd/mount/file.go`,
  `cmd/mount/dir.go`, `cmd/mountlib/mount.go`, plus new
  `cmd/mount/posixmeta.go`).

### Materialise / refresh the fork

```bash
./scripts/setup-fork.sh          # no-op if already present
./scripts/setup-fork.sh --force  # rebuild from scratch
make fork                        # same, via the Makefile
```

The script reads the rclone version from the single-line `require` in
`go.mod`, downloads that exact module version with `go mod download`, copies it
to `third_party/rclone/`, and applies the patch with `patch -p1`.

`make build`, `make test` and `test/run.sh` invoke it automatically, so a fresh
clone only needs a Go toolchain and `patch`.

### Regenerating the patch

After editing the fork (e.g. when bumping rclone), regenerate the diff from a
pristine copy:

```bash
SRC="$(go env GOMODCACHE)/github.com/rclone/rclone@v1.76.0"
{ for f in vfs/vfs.go vfs/file.go vfs/dir.go cmd/mount/file.go cmd/mount/dir.go cmd/mountlib/mount.go; do
    diff -u --label "a/$f" --label "b/$f" "$SRC/$f" "third_party/rclone/$f" || true
  done
  diff -uN --label /dev/null --label b/cmd/mount/posixmeta.go /dev/null \
    third_party/rclone/cmd/mount/posixmeta.go || true
} > third_party/rclone-posixmeta.patch
```

Then rebuild the fork from the patch (`./scripts/setup-fork.sh --force`) and
run `./test/run.sh` to confirm it still applies cleanly and passes.

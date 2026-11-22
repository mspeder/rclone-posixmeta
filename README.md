# rclone-posixmeta

A custom [`rclone`](https://rclone.org) build with a `posixmeta` wrapper backend
that persists live POSIX metadata (`mode`, `uid`, `gid`, `symlink`, `btime`) in
sparse per-entry JSON sidecars (`<entry>.posixmeta`) while delegating payloads
and `mtime` to any wrapped remote.

Typical encrypted-cloud topology:

```text
local/pcloud/S3  ->  crypt  ->  posixmeta  ->  rclone mount (FUSE)
```

`posixmeta` sits above `crypt` so it sees plaintext entry names, hides
`.posixmeta` sidecars, inherits owners from parent directories, and coalesces
sidecar writeback. The bundled rclone fork teaches `rclone mount` to forward
FUSE `chmod`/`chown` and report per-entry metadata from `stat` when the
`--metadata-push` mount option is given (off by default, so stock rclone
behaviour is unchanged for other backends):

```bash
rclone-posixmeta mount pm: /mnt/pm --metadata-push --vfs-links --vfs-cache-mode writes
```

Symlinks use upstream rclone's `.rclonelink` model: add `--vfs-links` to store
a link as one object holding the target (no `.posixmeta` sidecar), with
`readlink`/`stat`/rename/delete handled by the VFS. The fork avoids overlaying
the posixmeta default file mode on link nodes, so `--link-perms` applies while
`uid`/`gid` inheritance still does. Without `--vfs-links`, `ln -s` fails
loudly instead of silently degrading.

`posixmeta` refuses to mount without `--metadata-push` rather than silently
ignoring the stored `mode`/`uid`/`gid`.

* Architecture, invariants and test procedures: [`AGENTS.md`](AGENTS.md)
* FUSE chmod/chown design decision:
  [`docs/adr/0001-fuse-chmod-chown-propagation.md`](docs/adr/0001-fuse-chmod-chown-propagation.md)
* FUSE symlink design decision:
  [`docs/adr/0002-fuse-symlinks-via-vfs-links.md`](docs/adr/0002-fuse-symlinks-via-vfs-links.md)
* Test harness: [`test/README.md`](test/README.md)

## Build

Requirements: Go (matching `go.mod`), a C compiler, and `patch`.

```bash
make build        # bootstraps third_party/rclone, then CGO-enabled go build
# or
./scripts/setup-fork.sh
CGO_ENABLED=1 go build -o ./rclone-posixmeta .
```

The local rclone fork is **not** versioned: only
`third_party/rclone-posixmeta.patch` is committed and
`scripts/setup-fork.sh` materialises `third_party/rclone/` from the pinned
upstream release. See [`third_party/README.md`](third_party/README.md).

## Test

```bash
make test                 # Go unit tests + CLI + mount-over-crypt
./test/run.sh --list      # enumerate tests
./test/run.sh 01-cli      # a single suite
```

Tests isolate `HOME`, rclone config, cache and scratch directories, and unmount
FUSE filesystems via traps.

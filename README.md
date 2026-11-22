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

Its in-memory sidecar metadata cache is a read-through LRU bounded by
`cache_max_entries` (default 65536) and `cache_max_size` (default 64 MiB), with
`cache_ttl` (default 5m) controlling staleness, so long-lived mounts over large
remotes cannot grow without limit. Inspect it with
`rclone backend cachestats pm:` (or `rclone rc backend/command
command=cachestats fs=pm:` against a mount running with `--rc` for live
counters).

Metadata updates are debounced by `writeback_delay` and written by
`writeback_workers` goroutines with per-path ordering and exponential-backoff
retries (`writeback_retry_max`, queue bounded by `pending_max_entries`), so a
slow or briefly failing remote does not drop a `chmod`/`chown`. Queued updates
are flushed by `Shutdown` and by an `lib/atexit` handler, which also covers
`fusermount -u`. `overwrite_metadata` (`preserve` by default) controls whether
an overwrite keeps or drops the previous object's sidecar. See
[`docs/durability-and-scale.md`](docs/durability-and-scale.md) for the full list
of fixes and the accepted residual limitations.

* Architecture, invariants and test procedures: [`AGENTS.md`](AGENTS.md)
* FUSE chmod/chown design decision:
  [`docs/adr/0001-fuse-chmod-chown-propagation.md`](docs/adr/0001-fuse-chmod-chown-propagation.md)
* FUSE symlink design decision:
  [`docs/adr/0002-fuse-symlinks-via-vfs-links.md`](docs/adr/0002-fuse-symlinks-via-vfs-links.md)
* Bounded sidecar cache design decision:
  [`docs/adr/0003-bounded-sidecar-cache.md`](docs/adr/0003-bounded-sidecar-cache.md)
* Durability, writeback ordering and scale — what is fixed and what is residual:
  [`docs/durability-and-scale.md`](docs/durability-and-scale.md)
* Durable writeback design decision:
  [`docs/adr/0004-durable-writeback-and-delegation.md`](docs/adr/0004-durable-writeback-and-delegation.md)
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

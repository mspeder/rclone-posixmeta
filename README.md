# rclone-posixmeta

A custom [`rclone`](https://rclone.org) build with a `posixmeta` wrapper backend
that persists live POSIX metadata (`mode`, `uid`, `gid`, `symlink`, `btime`) in
sparse per-entry JSON sidecars (`<dir>/.posixmeta.<entry>`) while delegating
payloads and `mtime` to any wrapped remote. Set `sidecar_layout = dir` to store
one aggregate metadata map per directory (`<dir>/.posixmeta`) instead — one
object and one read for a whole directory of overridden entries. The layouts are
never converted automatically: opening a remote with the wrong one fails with an
explicit error. See
[`docs/adr/0010-per-directory-aggregate-sidecars.md`](docs/adr/0010-per-directory-aggregate-sidecars.md).

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
a link, with `readlink`/`stat`/rename/delete handled by the VFS. By default the
link is one object holding the target (plus a sparse sidecar only when its
creator differs from the owner it would inherit), and the fork avoids overlaying
the posixmeta default file mode on link nodes, so `--link-perms` applies while
`uid`/`gid` are still recorded and shown. Set `proxy_links = true` on the remote
to store the target in the entry's sidecar instead: a link then costs one object
at rest in total (the sidecar), and the backend emulates the
`<name>.rclonelink` object from its `symlink` attribute. Without `--vfs-links`,
`ln -s` fails loudly instead of silently degrading.

Entries created through the mount (`touch`, `mkdir`, `cp`, `ln -s`) are recorded
with the creating process's `uid`/`gid` and `mode`, like a regular filesystem — a
file created inside a directory owned by another user belongs to its creator, not
to the directory owner, and the mode is the one the kernel asked for (already
masked by the caller's umask), so `mkdir -m 0700` and `umask 077` behave as on
ext4. A `setgid` directory forces its group and copies `S_ISGID` onto new
subdirectories (never onto files), so a setgid tree keeps its group all the way
down. Modes equal to `default_file_mode`/`default_dir_mode` still write no
sidecar. See
[`docs/adr/0005-create-ownership-from-fuse-request.md`](docs/adr/0005-create-ownership-from-fuse-request.md)
and
[`docs/adr/0011-create-mode-from-fuse-request.md`](docs/adr/0011-create-mode-from-fuse-request.md).

With `--metadata-push` the kernel also *enforces* the reported ownership: path
traversal, reads and writes use the `mode`/`uid`/`gid` from the sidecars, so
`cd` into a `0700` directory or `cat` a `0600` file owned by somebody else is
refused even with `--allow-other`. Pass `--metadata-permissions=false` to keep
the old permissive behaviour. See
[`docs/adr/0006-kernel-permission-enforcement.md`](docs/adr/0006-kernel-permission-enforcement.md).

`sudo` is **not** an exception to FUSE's per-mount access rule: without
`--allow-other`, the kernel rejects every request whose uid/gid differs from the
mount owner's (`user_id=`/`group_id=` in `/proc/self/mounts`) with `EACCES`,
before rclone sees it. So if a directory's sidecar records a foreign owner,
`sudo chmod`/`sudo chown` fail with `Operation not permitted`/`Permission denied`
until the mount is created with `--allow-other` (and `user_allow_other` in
`/etc/fuse.conf`). `--metadata-permissions=false` does not help: `chmod`/`chown`
still require being the owner or holding `CAP_FOWNER`.

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
* Symlinks stored only in the sidecar (`proxy_links`):
  [`docs/adr/0008-proxy-link-files.md`](docs/adr/0008-proxy-link-files.md)
* `<dir>/.posixmeta.<entry>` sidecar naming (breaking on-disk layout):
  [`docs/adr/0009-sidecar-prefix-naming.md`](docs/adr/0009-sidecar-prefix-naming.md)
* Per-directory aggregate map (`sidecar_layout = dir`, `rclone backend repair`):
  [`docs/adr/0010-per-directory-aggregate-sidecars.md`](docs/adr/0010-per-directory-aggregate-sidecars.md)
* Bounded sidecar cache design decision:
  [`docs/adr/0003-bounded-sidecar-cache.md`](docs/adr/0003-bounded-sidecar-cache.md)
* Durability, writeback ordering and scale — what is fixed and what is residual:
  [`docs/durability-and-scale.md`](docs/durability-and-scale.md)
* Durable writeback design decision:
  [`docs/adr/0004-durable-writeback-and-delegation.md`](docs/adr/0004-durable-writeback-and-delegation.md)
* Step-by-step pCloud setup and mount guide (`crypt` + `posixmeta` on
  `/mnt/pcloud`):
  [`docs/mount-pcloud-crypt-posixmeta.md`](docs/mount-pcloud-crypt-posixmeta.md)
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
`third_party/patches/*.patch` are committed and `scripts/setup-fork.sh`
materialises `third_party/rclone/` from the pinned upstream release. See
[`third_party/README.md`](third_party/README.md).

## Test

```bash
make test                 # Go unit tests + CLI + mount-over-crypt
./test/run.sh --list      # enumerate tests
./test/run.sh 01-cli      # a single suite
```

Tests isolate `HOME`, rclone config, cache and scratch directories, and unmount
FUSE filesystems via traps.

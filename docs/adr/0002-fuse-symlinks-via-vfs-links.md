# 0002 — FUSE symlinks via upstream `--vfs-links`

* Status: Accepted
* Date: 2026-11-22
* Supersedes: none
* Related: `0001-fuse-chmod-chown-propagation.md`

## Context

The `posixmeta` backend already persists a `"symlink"` attribute in its sparse
`.posixmeta` sidecars, but that is a metadata attribute set with
`rclone backend setmeta`; it is not connected to the FUSE mount. The mount must
support `ln -s`, `readlink`, `stat` on links, rename and delete for the
production `local -> crypt -> posixmeta -> FUSE` stack.

Upstream rclone v1.76.0 already contains a complete symlink implementation in
the VFS, gated on `--vfs-links` (or the global `--links`). When enabled, a
symlink is stored as a single object whose name gets the `.rclonelink` suffix
and whose payload is the link target; the VFS strips the suffix when listing
and reading names.

Three designs were considered:

* **A — upstream `.rclonelink` convention** (selected): one ordinary object per
  link, no backend changes, works over any remote (including `crypt` and
  `pcloud`).
* **B — backend-created symlink sidecars**: add a VFS `CreateSymlink` path that
  stores the target in `"symlink"` metadata instead of a payload. This needs a
  new object per link plus sidecar, backend-specific VFS code, and a second
  representation of the same concept.
* **C — native filesystem symlinks**: not portable to cloud/crypt remotes.

## Decision

Use design A. No new rclone feature code is required for creation or reading;
symlinks are enabled by mounting with `--vfs-links`.

* **Storage**: `<name>.rclonelink` is the only object created for a link. Its
  payload is the target string. No `.posixmeta` sidecar is written unless
  metadata is explicitly pushed (`chmod`/`chown`/`backend setmeta`), matching
  the sparse-sidecar invariant.
* **Metadata**: the posixmeta `"symlink"` attribute remains an independent,
  explicitly set metadata field (`backend setmeta`, `lsjson --metadata`); the
  FUSE path does not consume it. The two representations are deliberately not
  mixed.
* **Mode**: `cmd/mount` overlays backend `mode`/`uid`/`gid` when
  `--metadata-push` is set, but for symlink nodes it must skip the mode
  overlay. `File.Mode()` already returns `--link-perms` (default 0666, masked
  by the umask) plus `S_IFLNK`, which is what the kernel expects. `uid`/`gid`
  are still overlaid, so parent-owner inheritance is visible on links.
* **Opt-in, no mount preflight**: `--vfs-links` stays optional. Unlike a missing
  `--metadata-push` (which silently hides metadata and drops chmod/chown), a
  missing `--vfs-links` makes `ln -s`/`readlink` fail loudly with `ENOSYS`.
  Forcing the flag would reinterpret any existing literal `*.rclonelink`
  objects as symlinks and refuse to mount existing posixmeta setups until the
  flag is added. The absence is covered by
  `test_mount_without_vfs_links_rejects_symlink` instead.
* **No backend change**: posixmeta and crypt treat `.rclonelink` as an ordinary
  payload/filename, so they need no special case.

## Consequences

Positive:

* Symlinks work end-to-end over `crypt`/cloud with exactly one object per link;
  encrypted storage never contains plaintext link targets or names.
* The fork patch only gains a mode guard, keeping it minimal and
  upstream-compatible.
* Rename/delete/remount require no extra backend work: they are ordinary object
  operations, already covered by the sidecar lifecycle rules.

Trade-offs:

* `rclone lsf`/`lsjson` at the backend level show `*.rclonelink`; the suffix is
  translated only inside the FUSE mount.
* `chmod` on a symlink is ignored by the kernel, hence the mode guard; users
  should not expect `chmod -h` to persist a link mode.
* A literal object named `*.rclonelink` is reinterpreted when `--vfs-links` is
  enabled. This is upstream behaviour and part of why the flag is opt-in.
* Mounts that do not pass `--vfs-links` retain stock symlink-less behaviour.

## Verification

* `test/suites/02-mount-crypt.sh`:
  `test_mount_symlinks_via_vfs_links` (relative/absolute/dangling links,
  `stat` type and mode, inherited uid/gid, backend payload and lack of sidecar,
  rename, delete, remount) and
  `test_mount_without_vfs_links_rejects_symlink` (loud opt-in failure).
* `01-cli/test_cli_btime_and_symlink_roundtrip` covers the independent
  `"symlink"` metadata attribute.

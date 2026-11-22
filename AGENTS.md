# AGENTS.md — `rclone-posixmeta` Development & Testing Guide

This file provides architectural context, build rules, design invariants, and repeatable verification procedures (including `rclone mount` over `crypt`) so autonomous coding agents (such as Cline) can safely maintain and extend `rclone-posixmeta`.

## 1. Project Overview

`rclone-posixmeta` is a custom `rclone` build featuring a wrapper backend (`backend/posixmeta`) that persists live POSIX metadata (`mode`, `uid`, `gid`, `symlink`, and `btime`) in **sparse per-entry JSON sidecar files** (`<entry>.posixmeta`) while delegating file payloads and `mtime` to any underlying `rclone` remote (e.g., `local`, `pcloud`, or `crypt`).

### Core Architecture & Layering

When used for encrypted cloud FUSE mounts, the stack is ordered from bottom to top:

1. **Backing Storage Remote** (`local`, `pcloud`, S3, etc.)
2. **`crypt` Remote** — Encrypts both file payloads and filenames (including `<entry>.posixmeta` sidecar files and their filenames) at rest.
3. **`posixmeta` Remote** — Sits **above** `crypt` so it can inspect plaintext entry names, hide `.posixmeta` sidecars during `List`/`NewObject`, resolve sparse metadata with parent directory owner inheritance, and maintain an in-memory TTL cache + debounced async writeback queue.
4. **`rclone mount` (VFS / FUSE)** — Exposes the `posixmeta` remote to the OS kernel.

## 2. Repository & Build Conventions

* **Primary Backend Implementation:** `backend/posixmeta/posixmeta.go`
* **Backend Registration:** Imported in `backend/all/all.go` (`_ "github.com/rclone/rclone/backend/posixmeta"`).
* **Build Command:**
  ```bash
  make build        # bootstraps the rclone fork if needed, then builds
  # or
  ./scripts/setup-fork.sh
  CGO_ENABLED=1 go build -o ./rclone-posixmeta .
  ```
  > **Note:** `CGO_ENABLED=1` is required on Linux for `rclone mount` FUSE support.
  > **Note:** `third_party/rclone/` is **not versioned**. Only
  > `third_party/rclone-posixmeta.patch` is committed; `scripts/setup-fork.sh`
  > (also run automatically by `make build`/`make test` and `test/run.sh`)
  > materialises the fork from the `go.mod`-pinned upstream release and applies
  > the patch. A fresh clone therefore needs `go` and `patch`, nothing else.
* **Local rclone fork (`third_party/rclone`):** `go.mod` has
  `replace github.com/rclone/rclone => ./third_party/rclone` (rclone v1.76.0).
  The fork carries the VFS/FUSE patch which is not upstream yet:
  * `vfs/`: `File`/`Dir` gained `Metadata`/`SetMetadata`/`Chmod`/`Chown`;
    `VFS.Chmod`/`Chown`/`Metadata` dispatch to them; pending metadata is
    queued while a file is still being written and applied in `setObject`;
    freshly `Mkdir`'d (synthetic) directory entries are refreshed from the
    backend before metadata is read or written.
  * `cmd/mount/`: `Attr` overlays per-entry `mode`/`uid`/`gid` from
    `fs.Metadataer` onto the mount-wide defaults, and `Setattr` forwards
    kernel `chmod`/`chown` to the node (silently ignored with ENOSYS when the
    backend doesn't support metadata). Both paths are gated on the
    `--metadata-push` mount option (default off) to preserve stock rclone
    behaviour for `local`/`crypt`/cloud mounts. Symlink nodes keep their mode
    from `--link-perms` (`S_IFLNK` plus the link permissions) while `uid`/`gid`
    are still overlaid.
  * `cmd/mountlib/`: `--metadata-push` option and a preflight that rejects a
    backend implementing `RequiresMetadataPush() bool` when the flag is
    absent; `posixmeta` returns true, so it cannot be mounted in a silently
    degraded state.
  Keep patches minimal and upstream-compatible so the fork can be refreshed
  (regeneration instructions: `third_party/README.md`); re-run
  `test/suites/02-mount-crypt.sh` after any rclone version bump. A
  unified diff against pristine v1.76.0 is kept in
  `third_party/rclone-posixmeta.patch` (see also
  `docs/adr/0001-fuse-chmod-chown-propagation.md` and
  `docs/adr/0002-fuse-symlinks-via-vfs-links.md`).

### Known Benign Log Message

When running `./rclone-posixmeta`, rclone may emit:

```text
ERROR internal error: no overview data found for "posixmeta"
```

This occurs because rclone's embedded `docs/data` overview lookup table is generated at upstream release time from built-in backends. It is non-fatal and does not affect backend execution, VFS mounts, or metadata operations. Do **not** treat this message as a test failure unless accompanied by a non-zero exit code or functional error.

## 3. Design Invariants & Critical Rules

Agents modifying `backend/posixmeta/posixmeta.go` **must** preserve the following invariants:

1. **Sparse Sidecars Only:**
   * A `<entry>.posixmeta` file must **only** be written when at least one attribute (`mode`, `uid`, `gid`, `symlink`, `btime`) differs from the effective default (`default_file_mode` / `default_dir_mode`, `default_uid` / `default_gid`, or inherited ancestor `uid`/`gid` when `inherit_parent_owner = true`).
   * If an update reverts all attributes back to defaults/inherited values, `buildSparseSidecar` returns `nil` and the sidecar must be deleted from the wrapped remote.
2. **Zero `atime` / `ctime` Write Churn:**
   * Never persist `atime` or `ctime` into `.posixmeta` sidecars; `SetMetadata` explicitly ignores them so file reads do not trigger remote sidecar uploads.
3. **Sidecar Hiding & Lifecycle Coupling:**
   * When `hide_sidecars = true` (default), `List` and `NewObject` must filter out any path ending with `sidecar_suffix` (`.posixmeta`), and `Put` must reject direct writes to sidecar paths.
   * `Move`, `DirMove`, `Remove`, `Rmdir`, and `Purge` must keep sidecars synchronized (moving or deleting `<entry>.posixmeta` alongside `<entry>`).
4. **Coalesced Writeback & Flush Safety:**
   * Rapid metadata updates are coalesced in memory via `writeback_delay` and flushed asynchronously.
   * `CleanUp` and `backend setmeta` must flush pending writes (`flushPending` / `flushAllPending`) so short-lived CLI commands and unmounts never lose sidecar updates.
5. **Crypt Password Obscuring:**
   * Any `rclone.conf` created for automated tests using `type = crypt` **must** obscure `password` and `password2` via `./rclone-posixmeta obscure <plaintext>` before writing the config file. Plaintext passwords cause `input too short when revealing password - is it obscured?`.
6. **Root-Boundary Sidecar Resolution:**
   * `NewFs` must mirror wrapped backends that support file roots: when the wrapped remote returns `fs.ErrorIsFile`, return an `Fs` rooted at the file's parent and propagate `fs.ErrorIsFile` (so `rclone moveto pm:a.txt pm:b.txt` works).
   * Sidecar paths are identity-mapped through `absRemote` (`f.root + "/" + remote`). All sidecar reads/writes/moves/deletes and ancestor-owner lookups use `f.base` (the wrapped remote opened at the `remote` option root) so sidecars for the root entry and above (`<root>.posixmeta`, ancestor owner sidecars) remain reachable from subpath filesystems. Payload I/O keeps using `f.wrapped` (rooted for performance).
   * `Rmdir`, `Purge`, and `DirMove` of a filesystem root must remove/move the root's sidecar in the parent directory.
7. **Opt-in FUSE metadata:**
   * `rclone mount` must not read or write POSIX metadata unless
     `--metadata-push` is set. Without it, `stat`/`getattr` must keep using
     `--file-perms`/`--dir-perms`/`--uid`/`--gid` and `chmod`/`chown` must remain
     no-ops for **all** backends (stock rclone behaviour).
   * `posixmeta` implements `RequiresMetadataPush() bool` returning true, so a
     mount without the flag fails fast with an actionable error instead of
     silently ignoring sidecars.
8. **Symlinks via upstream `--vfs-links`:**
   * A FUSE symlink is one `<name>.rclonelink` object whose payload is the
     target (see `docs/adr/0002-fuse-symlinks-via-vfs-links.md`). No
     `.posixmeta` sidecar is created for a plain link.
   * For symlink nodes the mount must not overlay the backend `mode`
     (`--link-perms`/`S_IFLNK` wins because Linux ignores `chmod` on links),
     but must still overlay `uid`/`gid` so inheritance works.
   * `--vfs-links` is deliberately opt-in and has no mount preflight: without
     it `ln -s`/`readlink` fail loudly, and forcing it would reinterpret
     literal `*.rclonelink` objects and break existing mounts.

## 4. Automated Verification Procedures

Always run tests inside isolated temporary directories (`/tmp/pm-*`) with an explicit `--config` and `--cache-dir` so user state (`~/.config/rclone` and `~/.cache/rclone`) is never touched. Always unmount FUSE mounts and delete `/tmp/pm-*` after each test.

### Test Harness (preferred entry point)

The shell procedures below are implemented as the repository harness; prefer it
over running the snippets by hand. It builds the binary, isolates every test
(own process, `HOME`, config, cache and scratch tree), unmounts FUSE filesystems
via traps, and reports `PASS`/`FAIL`/`SKIP`/`XFAIL`/`XPASS`.

```bash
./test/run.sh                    # go unit tests + CLI + mount/crypt
./test/run.sh 01-cli             # suite by name
./test/run.sh --list             # enumerate tests
./test/run.sh --filter 'move'    # substring/regex selection
./test/run.sh --tap              # TAP-13 output for CI
./test/run.sh --strict-xpass     # fail if a known gap is fixed
```

* Runner: `test/run.sh`
* Framework: `test/lib/harness.sh`
* Suites: `test/suites/00-go-unit.sh`, `01-cli.sh` (Procedure A), `02-mount-crypt.sh` (Procedure B)
* Go unit tests: `backend/posixmeta/posixmeta_test.go`
* Documentation: `test/README.md`

No tests are currently `XFAIL`. `XPASS` means a test marked with `xfail` now
passes: make it fatal in CI with `--strict-xpass`, then remove the `xfail` call.

### Procedure A: Non-Mount Unit/CLI Verification (Sidecars, Inheritance, Moves, Deletes)

Verifies sparse sidecar creation, hiding, `uid`/`gid` inheritance from parent directories, move propagation, and cleanup.

```bash
set -euo pipefail
rm -rf /tmp/pm-cli-test
mkdir -p /tmp/pm-cli-test/src /tmp/pm-cli-test/in

cat <<'EOF' > /tmp/pm-cli-test/rclone.conf
[raw_local]
type = local

[pm_local]
type = posixmeta
remote = raw_local:/tmp/pm-cli-test/src
default_file_mode = 0644
default_dir_mode = 0755
inherit_parent_owner = true
sidecar_suffix = .posixmeta
hide_sidecars = true
cache_ttl = 5m
writeback_delay = 50ms
EOF

# 1. Upload default file -> no sidecar should be created
printf "hello\n" > /tmp/pm-cli-test/in/default.txt
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf copyto /tmp/pm-cli-test/in/default.txt pm_local:default.txt
test ! -e /tmp/pm-cli-test/src/default.txt.posixmeta

# 2. Set directory owner -> child inherits uid/gid without needing its own sidecar
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf mkdir pm_local:proj
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf backend setmeta pm_local: proj -o uid=2001 -o gid=2002
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf copyto /tmp/pm-cli-test/in/default.txt pm_local:proj/inherited.txt
test ! -e /tmp/pm-cli-test/src/proj/inherited.txt.posixmeta
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf lsjson --metadata pm_local:proj/inherited.txt | grep -q '"uid":"2001"'

# 3. Override mode on child -> sparse sidecar created, hidden in ls, moved with file
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf backend setmeta pm_local: proj/inherited.txt -o mode=0750
test -e /tmp/pm-cli-test/src/proj/inherited.txt.posixmeta
./rclone-posixmeta --config /tmp/pm-cli-test/rclone.conf moveto pm_local:proj/inherited.txt pm_local:proj/renamed.txt
test ! -e /tmp/pm-cli-test/src/proj/inherited.txt.posixmeta
test -e /tmp/pm-cli-test/src/proj/renamed.txt.posixmeta

rm -rf /tmp/pm-cli-test
echo "PASS: Procedure A"
```

### Procedure B: End-to-End `rclone mount` over `crypt` Verification

This is the primary integration test for the target production topology (`local` -> `crypt` -> `posixmeta` -> `FUSE mount`).

#### Critical VFS / Mount Testing Notes:

* Always pass `--cache-dir /tmp/pm-crypt-mount/vfs-cache` so VFS writeback state is isolated from `~/.cache/rclone`.
* Always pass `--vfs-cache-mode writes --vfs-write-back 200ms --dir-cache-time 500ms` (or wait ≥ 1s after closing a file) so VFS flushes dirty files to the wrapped `crypt` remote before asserting on backing files or `lsjson --metadata`.
* Always register an `EXIT` trap to unmount `/tmp/pm-crypt-mount/mnt` (`fusermount -u` or `umount`) so failed assertions never leave stale FUSE mounts behind.

```bash
set -euo pipefail

TMPDIR="/tmp/pm-crypt-mount"
MNT="$TMPDIR/mnt"
RAW="$TMPDIR/raw"
VFS="$TMPDIR/vfs-cache"
CONF="$TMPDIR/rclone.conf"

cleanup() {
  fusermount -u "$MNT" 2>/dev/null || umount "$MNT" 2>/dev/null || true
  rm -rf "$TMPDIR"
}
trap cleanup EXIT

rm -rf "$TMPDIR"
mkdir -p "$RAW" "$MNT" "$VFS"

OBS1=$(./rclone-posixmeta obscure "test-crypt-pass-1")
OBS2=$(./rclone-posixmeta obscure "test-crypt-salt-2")

cat <<EOF > "$CONF"
[raw_store]
type = local

[enc_store]
type = crypt
remote = raw_store:$RAW
password = $OBS1
password2 = $OBS2

[pm_crypt]
type = posixmeta
remote = enc_store:
default_file_mode = 0644
default_dir_mode = 0755
inherit_parent_owner = true
sidecar_suffix = .posixmeta
hide_sidecars = true
cache_ttl = 5m
writeback_delay = 50ms
EOF

# 1. Mount posixmeta(crypt(local)) in background
./rclone-posixmeta --config "$CONF" mount pm_crypt: "$MNT" \
  --cache-dir "$VFS" \
  --metadata-push \
  --vfs-links \
  --vfs-cache-mode writes \
  --vfs-write-back 200ms \
  --dir-cache-time 500ms \
  --daemon

sleep 1

# 2. Create directory and file through FUSE mount
mkdir -p "$MNT/secret_dir"
printf "encrypted payload\n" > "$MNT/secret_dir/notes.txt"
sleep 1

# 3. Apply custom POSIX metadata (mode, uid, gid, btime)
./rclone-posixmeta --config "$CONF" backend setmeta pm_crypt: secret_dir \
  -o uid=1050 \
  -o gid=1060

./rclone-posixmeta --config "$CONF" backend setmeta pm_crypt: secret_dir/notes.txt \
  -o mode=0600 \
  -o btime="2026-11-22T08:00:00Z"

sleep 1

# 4. Verify mount visibility (no .posixmeta sidecar leaked into mount directory)
test -f "$MNT/secret_dir/notes.txt"
test ! -e "$MNT/secret_dir/notes.txt.posixmeta"
test "$(cat "$MNT/secret_dir/notes.txt")" = "encrypted payload"

# 5. Verify metadata resolution & parent owner inheritance via lsjson --metadata
META_JSON=$(./rclone-posixmeta --config "$CONF" lsjson --metadata pm_crypt:secret_dir)
echo "$META_JSON" | grep -q '"Name":"notes.txt"'
echo "$META_JSON" | grep -q '"mode":"0600"'
echo "$META_JSON" | grep -q '"uid":"1050"'
echo "$META_JSON" | grep -q '"gid":"1060"'
echo "$META_JSON" | grep -q '"btime":"2026-11-22T08:00:00Z"'

# 6. Verify at-rest encryption on raw storage (no plaintext filenames or sidecar suffixes)
if find "$RAW" -name '*notes*' -o -name '*.posixmeta' | grep -q .; then
  echo "FAIL: Plaintext filename or unencrypted .posixmeta leaked into raw storage"
  exit 1
fi

# 7. Verify file rename inside FUSE mount preserves metadata & moves encrypted sidecar
mv "$MNT/secret_dir/notes.txt" "$MNT/secret_dir/notes_renamed.txt"
sleep 1

META_RENAMED=$(./rclone-posixmeta --config "$CONF" lsjson --metadata pm_crypt:secret_dir)
echo "$META_RENAMED" | grep -q '"Name":"notes_renamed.txt"'
echo "$META_RENAMED" | grep -q '"mode":"0600"'
echo "$META_RENAMED" | grep -q '"uid":"1050"'

# 8. Verify deletion inside FUSE mount removes encrypted sidecar as well
rm "$MNT/secret_dir/notes_renamed.txt"
sleep 1
test -z "$(./rclone-posixmeta --config "$CONF" ls pm_crypt:secret_dir)"

# 9. Verify FUSE chmod/chown propagation and getattr reporting.  Changing only
#    the group needs a group the caller belongs to (CAP_CHOWN is not required).
printf "mode payload\n" > "$MNT/secret_dir/mode.txt"
sleep 1
chmod 0700 "$MNT/secret_dir/mode.txt"
CHGID=$(id -G | tr ' ' '\n' | awk -v cur="$(id -g)" '$1 != cur { print; exit }')
if [[ -n $CHGID ]]; then
  chgrp "$CHGID" "$MNT/secret_dir/mode.txt"
fi
sleep 1
test "$(stat -c '%a' "$MNT/secret_dir/mode.txt")" = "700"
if [[ -n $CHGID ]]; then
  test "$(stat -c '%g' "$MNT/secret_dir/mode.txt")" = "$CHGID"
fi
META_MODE=$(./rclone-posixmeta --config "$CONF" lsjson --metadata pm_crypt:secret_dir)
echo "$META_MODE" | grep -q '"Name":"mode.txt"'
echo "$META_MODE" | grep -q '"mode":"0700"'
if [[ -n $CHGID ]]; then
  echo "$META_MODE" | grep -q "\"gid\":\"$CHGID\""
fi

# 10. Verify symlinks via --vfs-links: one backend object holding the target,
#     no sidecar, symlink type and inherited owner visible through getattr.
RAW_BEFORE=$(find "$RAW" -type f | wc -l | tr -d ' ')
ln -s mode.txt "$MNT/secret_dir/link.txt"
sleep 1
test -L "$MNT/secret_dir/link.txt"
test "$(readlink "$MNT/secret_dir/link.txt")" = "mode.txt"
test "$(stat -c '%F' "$MNT/secret_dir/link.txt")" = "symbolic link"
test "$(stat -c '%u' "$MNT/secret_dir/link.txt")" = "1050"
./rclone-posixmeta --config "$CONF" lsjson pm_crypt:secret_dir |
  grep -q '"Name":"link.txt.rclonelink"'
test "$(find "$RAW" -type f | wc -l | tr -d ' ')" = "$((RAW_BEFORE + 1))"

echo "PASS: Procedure B (mount + crypt)"
```

## 5. Roadmap / Next Areas for Improvement

### 1. Native Kernel FUSE `chmod` / `chown` / `stat` Propagation — Fixed

Implemented in the local rclone fork (`third_party/rclone`, see section 2):

* **`vfs/file.go` & `vfs/dir.go`:** `File`/`Dir` expose `Metadata(ctx)`,
  `SetMetadata(ctx, fs.Metadata)`, `Chmod(ctx, os.FileMode)` and
  `Chown(ctx, uid, gid)`.
  * `File.SetMetadata` queues metadata in `pendingMetadata` while the file is
    still being written (`o == nil` or open writers) and applies it from
    `setObject` once the upload has produced the real object. `Metadata`
    overlays pending values so getattr is correct immediately.
  * `Dir.backendEntry` re-reads the `fs.Directory` from the backend when the
    VFS holds a synthetic `fs.NewDir` (freshly `Mkdir`'d or renamed
    directories); without this, `chmod` right after `mkdir` silently no-ops.
* **`vfs/vfs.go`:** `VFS.Chmod`, `VFS.Chown` and `VFS.Metadata` dispatch to
  the node methods, returning `ENOSYS` for backends without metadata support.
* **`cmd/mount/file.go` & `cmd/mount/dir.go`:** `Attr` reads `Metadata` and
  overlays `mode` (including setuid/setgid/sticky), `uid` and `gid` onto the
  mount-wide `--file-perms`/`--dir-perms`/`--uid`/`--gid` defaults. `Setattr`
  forwards `req.Valid.Mode()`, `req.Valid.Uid()` and `req.Valid.Gid()` as
  `SetMetadata`, swallowing `ENOSYS` to preserve the legacy no-op behaviour
  for backends without metadata. Both paths require `--metadata-push`.
* **`cmd/mountlib/mount.go`:** `--metadata-push` (default off) plus a
  preflight rejecting backends implementing `RequiresMetadataPush() bool`
  (`posixmeta`) when the flag is missing.

Covered by `02-mount-crypt` tests
`test_mount_chmod_via_fuse_is_forwarded`,
`test_mount_chown_via_fuse_is_forwarded`,
`test_mount_getattr_reflects_sidecar_metadata`,
`test_mount_requires_metadata_push`,
`test_mount_without_metadata_push_keeps_stock_behaviour` and
`test_mount_metadata_push_is_opt_in`, plus Procedure B step 9.

Known limitations:

* The `cmount`/`mount2` path (`cmd/cmount/`) is not patched; use `rclone mount`.
* `chown` of the mount root is not propagated: the VFS root holds a synthetic
  directory entry with no backend counterpart.
* Metadata changed by another process is invisible until `cache_ttl` expires
  in the mount process (same as any backend cache).

### 2. Symlink Support in FUSE Mount — Fixed

Uses the upstream `.rclonelink` model (design A in
`docs/adr/0002-fuse-symlinks-via-vfs-links.md`):

* Mount with `--vfs-links` (or the global `--links`). `ln -s` stores one
  `<name>.rclonelink` object whose payload is the target; `readlink`, `stat`,
  rename, delete and remount work through the VFS. No backend or rclone VFS
  change was needed.
* `posixmeta` over `crypt` keeps that as a single encrypted object: no
  plaintext link name/target at rest and no `.posixmeta` sidecar for a plain
  link.
* The fork's `Attr` overlay skips `mode` for symlink nodes so `--link-perms`
  (plus `S_IFLNK`) wins, while `uid`/`gid` (including parent-owner inheritance)
  are still applied.
* `--vfs-links` is opt-in with **no** mount preflight: without it `ln -s`
  fails with `ENOSYS` rather than silently misbehaving. Covered by
  `test_mount_without_vfs_links_rejects_symlink` and
  `test_mount_symlinks_via_vfs_links`.
* The separate `"symlink"` sidecar attribute remains available via
  `backend setmeta`/`lsjson --metadata`; it is not consumed by the FUSE path.

### 3. Production `pcloud` + `crypt` + `posixmeta` Stack

* Once local `mount + crypt` tests pass completely, configure the production stack in `~/.config/rclone/rclone.conf`:
  ```ini
  [pcloud_crypt]
  type = crypt
  remote = pcloud:<encrypted-folder>
  password = <obscured-password>
  password2 = <obscured-salt>

  [pcloud_posix]
  type = posixmeta
  remote = pcloud_crypt:
  default_file_mode = 0644
  default_dir_mode = 0755
  inherit_parent_owner = true
  sidecar_suffix = .posixmeta
  hide_sidecars = true
  cache_ttl = 5m
  writeback_delay = 500ms
  ```
* Mount `pcloud_posix:` with
  `--metadata-push --vfs-links --vfs-cache-mode writes` (or `full`) and verify
  that `chmod`, `chown`, `stat`, symlinks, file creation, renames, and deletes
  persist across remounts.

### 4. CLI Subpath Re-rooting (`pm:<path>`) — Fixed

* `rclone moveto pm:a.txt pm:b.txt`, `rclone moveto pm:dir pm:dir2`, `rclone rmdir pm:dir` and `rclone purge pm:dir` open the wrapped remote re-rooted at the requested subpath. Originally, sidecars at or above that root (`<dir>.posixmeta`, ancestor owner sidecars) were invisible, so moves/deletes leaked them and `lsjson --metadata pm:dir/file` lost parent-owner inheritance.
* Implemented in `backend/posixmeta/posixmeta.go`: `NewFs` now mirrors `fs.ErrorIsFile` by rooting at the file's parent, and keeps `f.base` (wrapped base root) for identity-mapped sidecar I/O (`absRemote`). `Move`, `DirMove`, `Rmdir`, `Purge`, `getSidecar`, writeback, cache invalidation, and ancestor resolution all use base-relative paths.
* Covered by `01-cli` tests `test_cli_move_propagates_sidecar`, `test_cli_rmdir_removes_directory_sidecar`, `test_cli_dir_move_propagates_directory_sidecar`, `test_cli_purge_removes_directory_sidecar`, `test_cli_subpath_metadata_inherits_ancestor_owner`, `02-mount-crypt` tests `test_crypt_subpath_dir_move_preserves_metadata`, `test_crypt_subpath_rmdir_removes_encrypted_sidecar`, plus Go unit tests `TestNewFsFileRootReturnsParent`, `TestSubpathInheritsOwnerFromSidecarOutsideRoot`, `TestRmdirAtRootRemovesSidecarOutsideRoot`.

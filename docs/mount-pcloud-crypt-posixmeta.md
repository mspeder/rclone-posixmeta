# Mounting pCloud as a POSIX-permission filesystem on `/mnt/pcloud`

This is the end-to-end recipe for the production topology this project exists
for:

```text
pcloud (encrypted at rest)
  └── crypt            encrypts payloads *and* filenames
        └── posixmeta  owns mode/uid/gid/symlink/btime sidecars, hides them
              └── rclone mount (FUSE)  ->  /mnt/pcloud
```

`posixmeta` must sit **above** `crypt`: it needs the plaintext entry names to
hide `<dir>/.posixmeta.<entry>` sidecars and to inherit owner ids from parent
directories. `crypt` then encrypts the sidecars (payload and filename) like any
other object.

Result after following this guide:

* `chmod`, `chown`, `stat` and symlinks work inside `/mnt/pcloud` and survive
  remounts.
* New files and directories inside `/mnt/pcloud` belong to the process that
  created them (like a regular filesystem), even under a directory owned by
  another user.
* pCloud holds only encrypted blobs: no plaintext filenames, no visible
  `.posixmeta.*` sidecar names, and `proxy_links = true` keeps a symlink in a
  single sidecar instead of a payload plus metadata.
* Sidecar updates are debounced, retried on failure and flushed on unmount.
* Nothing is mounted unless `--metadata-push` is given, so metadata is never
  silently ignored.

All commands below assume the binary is `./rclone-posixmeta` in the repository.
Step 7 installs it to `/usr/local/bin`.

---

## 1. Prerequisites

```bash
# FUSE + tooling
sudo apt install -y fuse3
ls -l /dev/fuse              # must exist
command -v fusermount3       # or fusermount

# for building only: Go (see go.mod), a C compiler, patch
go version && cc --version | head -1
```

Build (CGO is required for `rclone mount`):

```bash
cd /path/to/rclone-posixmeta
make build                   # bootstraps third_party/rclone, then builds
./rclone-posixmeta version
```

> `make build` materialises the pinned rclone fork from
> `third_party/patches/*.patch` on first run, so the very first build needs
> network access.

---

## 2. Create the raw pCloud remote

Interactive, on a machine with a browser:

```bash
./rclone-posixmeta config
#   n) New remote
#   name>     pcloud
#   Storage>  pcloud
#   client_id / client_secret>   (leave blank)
#   Use web browser to automatically authenticate?  y
#   ... log in and authorise in the browser ...
#   y) Yes this is OK
#   q) Quit config
```

Headless box: run `rclone authorize "pcloud"` on the machine that *has* a
browser, then paste the resulting token JSON at the `config_token>` prompt of
`./rclone-posixmeta config` on the server. If you paste a token by hand, also
set the region hostname — rclone normally derives it during the OAuth flow:

| Account region      | `hostname`        |
| ------------------- | ----------------- |
| US / "Original"     | `api.pcloud.com`  |
| EU (Luxembourg)     | `eapi.pcloud.com` |

Sanity-check the raw remote and create the encrypted-at-rest folder:

```bash
./rclone-posixmeta ls pcloud:
./rclone-posixmeta about pcloud:          # quota
./rclone-posixmeta mkdir pcloud:encrypted
```

`pcloud` has no checksum support, so `rclone check` will report "no hashes" on
this remote; use `--size-only` or compare through `cryptcheck` against the
crypt remote instead.

---

## 3. Create the `crypt` remote on top of it

Passwords **must** be obscured before they reach `rclone.conf`; plaintext makes
rclone fail with `input too short when revealing password - is it obscured?`.

```bash
./rclone-posixmeta obscure    # type the crypt password  -> PASS_OBSCURED
./rclone-posixmeta obscure    # type the salt/password2  -> SALT_OBSCURED
```

Append to `~/.config/rclone/rclone.conf`:

```ini
[pcloud_enc]
type = crypt
remote = pcloud:encrypted
password = PASS_OBSCURED
password2 = SALT_OBSCURED
filename_encryption = standard
directory_name_encryption = true
```

* `filename_encryption` / `directory_name_encryption` — encrypt names, not just
  contents. Do not disable these: the sidecar suffix would otherwise leak.
* Optional `filename_encoding = base32768` if you have long non-ASCII names
  (crypt expands names, and pCloud paths are length-limited).

Verify that pCloud itself only sees ciphertext:

```bash
printf 'secret payload\n' > /tmp/pm-probe.txt
./rclone-posixmeta copyto /tmp/pm-probe.txt pcloud_enc:probe.txt
./rclone-posixmeta ls pcloud:encrypted      # random-looking names, no "probe.txt"
./rclone-posixmeta cat pcloud_enc:probe.txt # back to plaintext
./rclone-posixmeta deletefile pcloud_enc:probe.txt
```

> Writing your crypt password down is unavoidable — losing it means losing the
> data. Changing `password2` makes existing data unreadable.

---

## 4. Create the `posixmeta` remote on top of the crypt remote

Append to the same `rclone.conf`:

```ini
[pcloud_posix]
type = posixmeta
remote = pcloud_enc:
default_file_mode = 0644
default_dir_mode = 0755
default_uid = -1
default_gid = -1
inherit_parent_owner = true
sidecar_prefix = .posixmeta
hide_sidecars = true
cache_ttl = 5m
cache_max_entries = 65536
cache_max_size = 64M
writeback_delay = 500ms
writeback_workers = 4
writeback_retry_max = 5m
pending_max_entries = 65536
prefetch_workers = 8
overwrite_metadata = preserve
proxy_links = true
```

Why these values:

| Option | Effect |
| --- | --- |
| `default_file_mode` / `default_dir_mode` | Attributes reported when an entry has no sidecar. No sidecar is written while an entry matches these. |
| `default_uid` / `default_gid` = `-1` | Fall back to the uid/gid of the `rclone mount` process (you). |
| `inherit_parent_owner` | An entry **without recorded credentials** (e.g. written by `rclone copy`) adopts the nearest ancestor directory's owner, so one `chown` on a directory covers such a subtree without per-file sidecars. Entries created through the mount store the creating process's uid/gid instead (see below). |
| `sidecar_prefix` | Sidecars are named `<dir>/.posixmeta.<entry>` next to their entry. The remote's own root has no name, so its attributes live in the bare `.posixmeta` **inside** it (mounting `pcloud_posix:` with `remote = pcloud_crypt:` → `<encrypted-folder>/.posixmeta`), while a subpath root keeps `<parent>/.posixmeta.<root>` (`pcloud_posix:dir`). Rename old `<entry>.posixmeta` files manually; they are not migrated (`docs/adr/0009-sidecar-prefix-naming.md`). |
| `hide_sidecars` | `List`/`NewObject` conceal `.posixmeta` and `.posixmeta.*`, `Put` refuses to write one. |
| `cache_ttl` | How long a resolved sidecar may be reused in memory. Shorter = fresher, more pCloud reads. |
| `cache_max_entries` / `cache_max_size` | Bound the read-through LRU so a long-lived mount cannot grow without limit. |
| `writeback_delay` | Coalesces `chmod` storms. `0` makes every change synchronous (safest against `SIGKILL`). |
| `writeback_workers` | Parallel sidecar uploads. `0` = synchronous in the caller. |
| `writeback_retry_max` | Cap of the exponential backoff; failed writes are retried, never dropped. |
| `pending_max_entries` | Queue cap; overflow is written synchronously instead of lost. |
| `prefetch_workers` | Concurrent sidecar prefetches after a directory listing (absence is cached for free). |
| `overwrite_metadata` | `preserve` keeps `chmod`/`chown` when the mount re-uploads a file; `reset` drops stale metadata on overwrite. Keep `preserve` for a mount. |
| `proxy_links` | `true` stores a symlink target in the entry sidecar only (no `.rclonelink` payload): one encrypted object per link. `false` (default) keeps the upstream payload model. `docs/adr/0008-proxy-link-files.md`. |
| `sidecar_layout` | `sparse` (default) writes `<dir>/.posixmeta.<entry>` per overridden entry. Set `dir` to store one aggregate `<dir>/.posixmeta` map per directory: one pCloud object and one read for a whole directory of `chmod`/`chown` entries. The layout is recorded in the remote's root map and detected on open; opening with the wrong one fails with `refusing to mix layouts` instead of ignoring metadata (`docs/adr/0010-per-directory-aggregate-sidecars.md`). |
| `on_corrupt` | `dir` layout only. `error` (default) fails reads and mutations of a directory whose map is corrupt; `warn` logs it and serves defaults while still refusing to overwrite the map. |
| `map_max_entries` / `map_max_bytes` | `dir` layout only. Caps of one directory map (default `100000` entries / `8M`); a larger map is treated as corrupt, so it can never be silently truncated. |

### Who owns a new file

Entries created through the mount (`touch`, `cp`, `mkdir`, `ln -s`) are recorded
with the **creating process's** `uid`/`gid` and `mode`, like a regular
filesystem: the FUSE request carries them and `posixmeta` stores a sparse sidecar
only when they differ from the value the entry would otherwise inherit (a default
umask with `0644`/`0755` therefore still writes no sidecar). The mode is the
kernel's, i.e. already masked by the caller's umask, so `mkdir -m 0700` and
`umask 077` work. A `setgid` directory forces its own group on new entries and
copies `S_ISGID` onto new subdirectories (never onto files), so a setgid tree
keeps its group all the way down. So

```bash
mkdir /mnt/pcloud/test
sudo chown hello:cms /mnt/pcloud/test && sudo chmod g+s /mnt/pcloud/test
cd /mnt/pcloud/test && touch newfile    # owned by you, group cms
mkdir sub && touch sub/f                # sub is 2755, f is you:cms
```

Entries that have no recorded creator (`rclone copy`/`sync` writes, objects
created before this behaviour existed) keep using `inherit_parent_owner` and
`default_file_mode`/`default_dir_mode`, so a directory `chown` still covers them.
See `docs/adr/0005-create-ownership-from-fuse-request.md` and
`docs/adr/0011-create-mode-from-fuse-request.md`.

Note: `--file-perms`/`--dir-perms` no longer affect entries created through a
posixmeta mount (the backend always reports a mode); they remain the `getattr`
fallback for entries whose backend gives no metadata.

Verify through the CLI before touching FUSE (nothing is mounted yet):

```bash
./rclone-posixmeta mkdir pcloud_posix:docs
./rclone-posixmeta backend setmeta pcloud_posix: docs -o uid=1050 -o gid=1060
printf 'hello\n' > /tmp/pm-hello.txt
./rclone-posixmeta copyto /tmp/pm-hello.txt pcloud_posix:docs/hello.txt

# inherited owner, no per-file sidecar needed
./rclone-posixmeta lsjson --metadata pcloud_posix:docs/hello.txt
#   -> "uid":"1050", "gid":"1060", "mode":"0644"

# sparse sidecar only when an attribute differs from the default
./rclone-posixmeta backend setmeta pcloud_posix: docs/hello.txt -o mode=0600
./rclone-posixmeta lsjson --metadata pcloud_posix:docs | grep mode

# sidecars are invisible on the wrapper and encrypted on pcloud
./rclone-posixmeta ls pcloud_posix:docs
./rclone-posixmeta ls pcloud:encrypted

./rclone-posixmeta backend cachestats pcloud_posix:
```

---

## 5. Prepare the mount point

```bash
sudo install -d -o "$USER" -g "$(id -gn)" -m 0755 /mnt/pcloud
```

For a single-user mount nothing else is needed. If other accounts (a media
server, `nginx`, a container) must read the mount, enable FUSE's
`user_allow_other` **and** pass `--allow-other`:

```bash
sudoedit /etc/fuse.conf      # uncomment/add:  user_allow_other
```

Those accounts are then subject to the recorded POSIX permissions (see
`--metadata-permissions` below): they can only traverse/read entries whose
`mode`/`uid`/`gid` grant it, so give the shared directory an appropriate group
and mode instead of relying on a permissive mount.

`--allow-other` is also what makes `sudo` work. Without it a FUSE mount only
accepts requests whose uid **and** gid match `user_id=`/`group_id=` in
`/proc/self/mounts`, so the kernel answers `EACCES` for root before rclone sees
the request: `sudo chmod`/`sudo chown` on the mount fail with `Operation not
permitted`/`Permission denied` even though root holds `CAP_FOWNER`. If a
directory's sidecar records a foreign owner and you need to take it back:

```bash
# 1. preferred: mount with --allow-other (user_allow_other in /etc/fuse.conf),
#    then root can chmod/chown like on a local filesystem
sudo chown -R "$(id -u):$(id -g)" /mnt/pcloud/locked_dir

# 2. without --allow-other: repair the sidecar out of band (no FUSE checks).
#    A running mount keeps its cached attributes for cache_ttl (5m by default),
#    so wait it out or remount before stat shows the new owner.
./rclone-posixmeta backend setmeta pcloud_posix: locked_dir \
  -o uid="$(id -u)" -o gid="$(id -g)" -o mode=0755
```

`--metadata-permissions=false` is **not** a workaround for this: it only skips
the traversal/read checks, while `chmod`/`chown` still require being the owner
or holding `CAP_FOWNER`.

---

## 6. First mount (foreground, so you see errors)

```bash
./rclone-posixmeta mount pcloud_posix: /mnt/pcloud \
  --metadata-push \
  --vfs-links \
  --vfs-cache-mode writes \
  --vfs-write-back 0 \
  --uid "$(id -u)" --gid "$(id -g)" \
  --dir-cache-time 1m \
  --poll-interval 1m \
  --cache-dir ~/.cache/rclone/vfs \
  --tpslimit 4 --tpslimit-burst 8 \
  --log-file ~/pcloud-mount.log --log-level INFO
```

| Flag | Why |
| --- | --- |
| `--metadata-push` | **Mandatory.** Read `mode`/`uid`/`gid` from sidecars in `getattr` and forward kernel `chmod`/`chown`. `posixmeta` refuses to mount without it. |
| `--metadata-permissions` | Default **on**: the kernel enforces the reported `mode`/`uid`/`gid`, so `cd`/`read`/`write` obey the sidecars even with `--allow-other`. Pass `--metadata-permissions=false` only if a service must read and write entries regardless of the recorded owner. |
| `--vfs-links` | Symlinks as `<name>.rclonelink` objects; with `proxy_links = true` the target is served from the sidecar and nothing extra is uploaded. Without it `ln -s` fails loudly. |
| `--vfs-cache-mode writes` | Local staging while a file is written, then one upload. Use `full` for read caching too; avoid `off` (partial uploads straight to pCloud). |
| `--vfs-write-back 0` | Upload on close instead of the default 5 s deferral. **Keep this**: the VFS upload queue is cancelled, not drained, when the mount is unmounted (`fusermount -u` or `SIGTERM`), so a file written within `--vfs-write-back` of a stop would never reach pCloud. |
| `--uid` / `--gid` | Mount-wide defaults where no sidecar exists — notably the mount root, whose `chown` is not propagated. |
| `--dir-cache-time` / `--poll-interval` | pCloud supports change notifications and `posixmeta` forwards them, so listings refresh quickly; `poll-interval` must be **smaller** than `dir-cache-time`. |
| `--cache-dir` | Keeps VFS staging away from the default location — useful with several mounts. |
| `--tpslimit*` | pCloud throttles API calls; start conservative and lower `--transfers` if you see 429s. |

In a second terminal, verify from the kernel's point of view:

```bash
cd /mnt/pcloud
ls -la                                    # no .posixmeta.* entries
printf 'mounted payload\n' > mode.txt
chmod 0700 mode.txt
stat -c '%a %u %g %n' mode.txt            # 700 <you> <you> mode.txt
ln -s mode.txt link.txt && readlink link.txt && stat -c '%F' link.txt
df -h /mnt/pcloud                         # quota from pcloud about
```

Metadata applied while mounted is debounced by `writeback_delay`; confirm what
the backend actually stored:

```bash
./rclone-posixmeta lsjson --metadata pcloud_posix:mode.txt
```

`chown` to an arbitrary uid/gid needs `CAP_CHOWN` (i.e. `sudo`); changing only
the group works unprivileged when you belong to it. As a normal user prefer
`backend setmeta` to assign foreign owners:

```bash
./rclone-posixmeta backend setmeta pcloud_posix:docs -o uid=1050 -o gid=1060
```

Because `--metadata-permissions` is on by default, that directory is then owned
by `1050` and the kernel enforces its mode: `cd docs` still works with the
default `0755`, but creating, renaming or deleting inside it from this mount
fails unless the mode grants the mounting user `w`/`x` (for example `-o
mode=0775` or `0777`). `cat` on a `0600` file of another owner fails the same
way. Changes made with `backend setmeta` while the mount is running become
visible after `cache_ttl` (default 5m), like any other metadata change from
outside the mount. If you need the old permissive behaviour for a
backup-style mount, add `--metadata-permissions=false`.

Unmount cleanly with <kbd>Ctrl-C</kbd> in the foreground or `fusermount -u
/mnt/pcloud`. Two different flush windows matter here:

* **POSIX metadata (sidecars)** — queued `chmod`/`chown` updates are written by
  the `posixmeta` backend itself and are flushed on `Shutdown` plus an `atexit`
  handler, so an unmount or `SIGTERM` right after `chmod` is safe. Only
  `SIGKILL` can lose the last `writeback_delay` window (`writeback_delay = 0`
  removes even that).
* **File payloads (VFS write cache)** — uploads deferred by `--vfs-write-back`
  are *cancelled* when the mount goes away, which is why this guide sets
  `--vfs-write-back 0` (upload on close). If you prefer the default 5 s
  deferral, wait longer than that before unmounting.

---

## 7. Make it persistent (systemd user service)

Install the binary outside the build tree first:

```bash
sudo install -m 0755 ./rclone-posixmeta /usr/local/bin/rclone-posixmeta
```

`~/.config/systemd/user/pcloud-posix.service`:

```ini
[Unit]
Description=pCloud (crypt + posixmeta) mounted on /mnt/pcloud
Documentation=file:///usr/share/doc/rclone-posixmeta/mount-pcloud-crypt-posixmeta.md
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/rclone-posixmeta mount pcloud_posix: /mnt/pcloud \
  --metadata-push \
  --vfs-links \
  --vfs-cache-mode writes \
  --vfs-write-back 0 \
  --uid %U --gid %G \
  --dir-cache-time 1m \
  --poll-interval 1m \
  --cache-dir %h/.cache/rclone/vfs \
  --tpslimit 4 --tpslimit-burst 8 \
  --log-file %h/pcloud-mount.log --log-level INFO
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

Enable it — do **not** combine this with `--daemon`, systemd supervises the
foreground process. There is deliberately **no `ExecStop=fusermount -u`**:
systemd's `SIGTERM` makes rclone unmount and exit itself, and adding an
external unmount first would only shorten the window in which queued work can
be cancelled. With `--vfs-write-back 0` nothing is left queued at that point.

```bash
systemctl --user daemon-reload
systemctl --user enable --now pcloud-posix.service
systemctl --user status pcloud-posix.service
loginctl enable-linger "$USER"     # start at boot without logging in
```

`%U`/`%G` are the service user's uid/gid, so the mount is owned by you. If the
mount must be readable by other local users, add `--allow-other` (plus
`user_allow_other` in `/etc/fuse.conf`).

An fstab entry also works but is harder to keep correct because `posixmeta`
requires `--metadata-push`; prefer the unit above and keep the flags together.

---

## 8. Prove it persists across a remount

```bash
# write metadata through the mount
printf 'durable\n' > /mnt/pcloud/durable.txt
chmod 0640 /mnt/pcloud/durable.txt
sleep 2                      # > writeback_delay, so the sidecar is uploaded

systemctl --user restart pcloud-posix.service
sleep 2

stat -c '%a %n' /mnt/pcloud/durable.txt          # 640
./rclone-posixmeta lsjson --metadata pcloud_posix:durable.txt
```

The same check works after a reboot, and it is the one to run whenever you
change a `posixmeta` option.

---

## 9. Observe the live cache and writeback queue

A standalone CLI invocation has an empty cache. To see the counters of the
running mount, start it with `--rc` (add to the unit's `ExecStart`)...

```bash
./rclone-posixmeta mount pcloud_posix: /mnt/pcloud \
  --metadata-push --vfs-links --vfs-cache-mode writes \
  --rc --rc-addr 127.0.0.1:5572 \
  --log-file ~/pcloud-mount.log
```

...then query it:

```bash
./rclone-posixmeta rc backend/command command=cachestats fs=pcloud_posix:
# entries, bytes, max_entries, max_bytes, hits, misses, evictions,
# pending_writes, inflight_writes, written, failed_writes,
# retried_writes, overflow_writes, last_write_error
```

What to watch on a pCloud mount:

* `pending_writes` stuck above zero → sidecar uploads are failing; look at
  `last_write_error` and `failed_writes`.
* `evictions` climbing fast with a high `misses` → consider raising
  `cache_max_entries`/`cache_max_size`.
* `overflow_writes` > 0 → `pending_max_entries` is too small for your churn.

---

## 10. Day-2 operations

**Backups.** The `.posixmeta` sidecars *are* part of the data (they are named `<dir>/.posixmeta.<entry>`). Copy or sync
through a `posixmeta` remote, never through raw `pcloud:` / `pcloud_enc:`, so
sidecars travel with their objects:

```bash
./rclone-posixmeta sync /mnt/pcloud pcloud_posix:backup   # or: copy
./rclone-posixmeta check pcloud_posix:backup /mnt/pcloud --size-only
```

**Bulk metadata.** `chmod`/`chown` inside the mount, or from the CLI:

```bash
./rclone-posixmeta backend setmeta pcloud_posix:projects -o uid=1050 -o gid=1060
```

Directories inherit downward, so one call covers a subtree unless a file
overrides `uid`/`gid` explicitly.

**Renames and deletes.** `mv`, `rm`, `rmdir` inside the mount move/remove the
sidecar with the object. CLI `move`/`moveto`/`rmdir`/`purge` on the
`posixmeta` remote do the same, including for a subpath root (`pcloud_posix:dir`
deletes `.posixmeta.dir` in its parent).

**Changing mount or cache settings.** Edit the unit, `systemctl --user
daemon-reload`, `systemctl --user restart pcloud-posix.service`.

**Unmount.**

```bash
systemctl --user stop pcloud-posix.service   # flushes pending sidecars
# or, for a foreground/daemon mount:
fusermount -u /mnt/pcloud
```

Queued POSIX metadata is flushed on either path. Payload uploads are only
guaranteed to be complete because `--vfs-write-back 0` uploads on close; if you
raise that value, wait it out before stopping.

If the mount is wedged (`Transport endpoint is not connected`):

```bash
fusermount -uz /mnt/pcloud     # lazy unmount; prefer the clean path above
```

---

## 11. pCloud-specific caveats

* **No hashes.** pCloud exposes no checksums, so `rclone check` reports "no
  hashes"; use `--size-only` or `cryptcheck` against the crypt remote.
* **Rate limits.** pCloud throttles aggressively. Keep `--tpslimit` modest and
  reduce `--transfers`/`--checkers` if you see 429s in the log.
* **Filename restrictions.** pCloud forbids `\ / : * ? " < > |`, control
  characters and trailing dots/spaces. `crypt` with standard filename encoding
  produces safe, opaque names, which is one more reason not to disable it.
* **Change notifications.** pCloud supports them (long-poll with short
  fallback), `crypt` and `posixmeta` forward them, so `--poll-interval` keeps
  the mount in sync with other clients.
* **Server-side copy.** `posixmeta` forwards `Copy` when the wrapped backend
  provides it, and `crypt` forwards it to pCloud, so `cp` inside the mount
  avoids a download/upload round trip where pCloud allows it.
* **No multi-writer safety.** Run **one** mount per remote. Two mounts of the
  same `posixmeta` remote have no coordination for sidecar writes.
* **`mtime`** is stored by the pCloud backend; `posixmeta` only owns
  `mode`/`uid`/`gid`/`symlink`/`btime`.
* **Trash-only deletion.** `rclone cleanup` on pCloud needs `username`/
  `password` (obscured) because that API does not accept OAuth; see
  `./rclone-posixmeta help backend pcloud`.
* **Accepted residuals** (no atomic payload+sidecar pair, memory-only debounce
  queue, `preserve` overwrite semantics) are listed in
  [`docs/durability-and-scale.md`](durability-and-scale.md). Set
  `writeback_delay = 0` if you cannot tolerate losing the last debounce window
  on a power cut.

---

## 12. Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `ERROR internal error: no overview data found for "posixmeta"` | Benign: rclone's embedded docs table predates this backend. Non-fatal, ignore it. |
| `backend "posixmeta" stores POSIX mode/uid/gid; mount with --metadata-push ...` | Add `--metadata-push`. This is the fail-fast guard against a silently metadata-less mount. |
| `stat` shows `666`/`777` or ignores saved modes | You mounted the `crypt` remote instead of the `posixmeta` one, or forgot `--metadata-push`, or the sidecar is absent and `default_*_mode` applies. |
| `ln -s`: `Operation not supported` / `readlink` empty | Add `--vfs-links`. |
| `chmod` works, then reverts after a rewrite | `overwrite_metadata = reset` is dropping the sidecar on VFS re-upload; use `preserve` (default). |
| `chown`: `Operation not permitted` | Changing to another uid needs `sudo` (`CAP_CHOWN`); an unprivileged user can only change the group to one it belongs to. `sudo` itself only works if the mount was created with `--allow-other` (see the next row). Use `backend setmeta` for arbitrary owners. |
| `sudo chmod`/`sudo chown`: `Operation not permitted` or `Permission denied` | The mount was created without `--allow-other`. A FUSE mount only accepts requests whose uid *and* gid match `user_id=`/`group_id=` in `/proc/self/mounts`; `fuse_setattr()` returns `EACCES` for `sudo` (and for any other account) before rclone runs, even though root has `CAP_FOWNER`. Remount with `--allow-other` (+ `user_allow_other` in `/etc/fuse.conf`), repair with `backend setmeta`, or mount as root. `--metadata-permissions=false` does not help. |
| `.posixmeta` / `.posixmeta.*` visible in `/mnt/pcloud` | `hide_sidecars` is false or the wrong remote is mounted. Sidecars are only hidden when the mount goes through `posixmeta`. |
| Metadata written elsewhere is not visible | In-memory staleness: wait for `cache_ttl` (5m default) or lower it. |
| A file written just before stopping is missing from pCloud | Its upload was still deferred by `--vfs-write-back` (default 5 s) and the VFS cancels queued uploads when the mount goes away. Use `--vfs-write-back 0`, or wait out the delay before unmounting. |
| `Transport endpoint is not connected`, `df` hangs | The mount died. `fusermount -uz /mnt/pcloud`, then read the log (`~/pcloud-mount.log`). |
| `input too short when revealing password - is it obscured?` | A plaintext password reached `rclone.conf`; re-run `rclone-obscure` and paste the obscured value. |
| Sidecar writes keep failing; `pending_writes` grows | Look at `last_write_error` in `cachestats`; pCloud quota/rate limits and network failures are retried with backoff up to `writeback_retry_max`. |
| Very slow `ls -l` on big directories | Confirm `prefetch_workers > 0` and raise `--dir-cache-time`; check `misses`/`evictions` in `cachestats`. |
| 429 / `too many requests` in the log | Lower `--tpslimit-burst`, keep `--tpslimit 4`, reduce `--transfers`/`--checkers`. |
| `copy` from a local tree loses modes | `rclone copy` does not send metadata unless asked: add `--metadata` (or set modes afterwards with `backend setmeta`). |

---

## TL;DR

```bash
make build
./rclone-posixmeta config            # pcloud remote (US or EU hostname)
./rclone-posixmeta obscure           # twice: password, then salt
# write [pcloud_enc] (crypt) and [pcloud_posix] (posixmeta, see steps 3-4)
sudo install -d -o "$USER" -g "$(id -gn)" /mnt/pcloud
./rclone-posixmeta mount pcloud_posix: /mnt/pcloud \
  --metadata-push --vfs-links --vfs-cache-mode writes --vfs-write-back 0 \
  --uid "$(id -u)" --gid "$(id -g)" --poll-interval 1m
```

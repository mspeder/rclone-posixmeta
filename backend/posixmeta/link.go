// Proxy links: store a symlink entirely in the entry's sidecar.
//
// rclone represents a symlink as a regular object called `<name>.rclonelink`
// whose payload is the link target, and both `--vfs-links` and `rclone link`
// consume that shape. With `proxy_links = true` this backend does not upload
// that payload at all: the target lives in the `symlink` attribute of the
// single sidecar for `<name>` (the same file that already carries mode/uid/gid),
// and List/ListR/NewObject publish a virtual `<name>.rclonelink` object that
// serves the target from the sidecar. At rest the remote therefore contains one
// object per link instead of two.
package posixmeta

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
)

// sidecarLink is a proxied symlink resolved from a sidecar.
type sidecarLink struct {
	remote  string // emulated object name, e.g. "dir/link.txt.rclonelink"
	target  string
	modTime time.Time
}

// lookupMetadata returns the value of key in meta, ignoring case.
func lookupMetadata(meta fs.Metadata, key string) (string, bool) {
	if v, ok := meta[key]; ok {
		return v, true
	}
	for k, v := range meta {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// getSidecarInfo resolves the sidecar of remote together with the sidecar
// object's modification time.
func (f *Fs) getSidecarInfo(ctx context.Context, remote string) (*sidecarData, time.Time, error) {
	return f.getSidecarInfoAbs(ctx, f.absRemote(remote))
}

// resolveLinkCandidates reads the sidecars of the given entry names and returns
// the ones that represent a proxied symlink, keyed by entry name. Reads are
// bounded by prefetch_workers and served from the sidecar cache when possible.
func (f *Fs) resolveLinkCandidates(ctx context.Context, candidates map[string]struct{}) map[string]sidecarLink {
	links := make(map[string]sidecarLink, len(candidates))
	if len(candidates) == 0 {
		return links
	}

	targets := make([]string, 0, len(candidates))
	for target := range candidates {
		targets = append(targets, target)
	}

	n := f.opt.PrefetchWorkers
	if n <= 0 {
		n = 1
	}
	if n > len(targets) {
		n = len(targets)
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, n)
	)
	for _, target := range targets {
		sem <- struct{}{}
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			defer func() { <-sem }()
			sc, modTime, err := f.getSidecarInfo(ctx, target)
			if err != nil {
				fs.CountError(ctx, err)
				fs.Debugf(f, "posixmeta: cannot read sidecar of %q: %v", target, err)
				return
			}
			if sc == nil || sc.Symlink == "" {
				return
			}
			mu.Lock()
			links[target] = sidecarLink{
				remote:  target + fs.LinkSuffix,
				target:  sc.Symlink,
				modTime: modTime,
			}
			mu.Unlock()
		}(target)
	}
	wg.Wait()
	return links
}

// newLinkObject returns the virtual link object for a `.rclonelink` remote, or
// (nil, nil) when its sidecar does not describe a symlink.
func (f *Fs) newLinkObject(ctx context.Context, remote string) (fs.Object, error) {
	base := strings.TrimSuffix(remote, fs.LinkSuffix)
	if base == "" || !isLinkRemote(remote) {
		return nil, nil
	}
	sc, modTime, err := f.getSidecarInfo(ctx, base)
	if err != nil {
		return nil, err
	}
	if sc == nil || sc.Symlink == "" {
		return nil, nil
	}
	return f.newLinkObjectAt(remote, sc.Symlink, modTime), nil
}

func (f *Fs) newLinkObjectAt(remote, target string, modTime time.Time) *posixLinkObject {
	return &posixLinkObject{f: f, remote: remote, target: target, modTime: modTime}
}

func (f *Fs) newLinkObjectFromSidecar(link sidecarLink) *posixLinkObject {
	return f.newLinkObjectAt(link.remote, link.target, link.modTime)
}

// readLinkTarget reads and validates the payload of a `.rclonelink` upload.
func readLinkTarget(in io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(in, maxLinkTargetSize+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxLinkTargetSize {
		return "", fmt.Errorf("posixmeta: symlink target is larger than %d bytes", maxLinkTargetSize)
	}
	target := string(raw)
	if target == "" {
		return "", errors.New("posixmeta: empty symlink target")
	}
	if strings.IndexByte(target, 0) >= 0 {
		return "", errors.New("posixmeta: symlink target contains a NUL byte")
	}
	return target, nil
}

// writeLinkSidecar stores target in the sidecar of base, merging the POSIX
// attributes carried by meta. The write is synchronous: for a proxied link the
// sidecar is the link itself, so a failed write must fail the operation instead
// of being retried after `ln -s` already reported success.
func (f *Fs) writeLinkSidecar(ctx context.Context, base, target string, meta fs.Metadata) (*sidecarData, error) {
	if base == "" {
		return nil, errors.New("posixmeta: invalid symlink name")
	}
	cur, err := f.resolve(ctx, base, false)
	if err != nil {
		return nil, err
	}
	if _, _, err := applyPOSIXAttrs(&cur, meta, true); err != nil {
		return nil, err
	}
	cur.symlink = target

	sc := f.buildSparseSidecar(ctx, base, false, cur)
	if sc == nil {
		// Unreachable while target is non-empty, but the symlink is the whole
		// point: never let a sparse result drop it.
		sc = &sidecarData{Version: sidecarSchemaVersion, Symlink: target}
	}

	absBase := f.absRemote(base)
	f.quiesceAbs(ctx, absBase)
	if err := f.persistSidecarAbs(ctx, absBase, sc); err != nil {
		return nil, err
	}
	f.putCacheAt(absBase, sc, time.Now())
	return sc, nil
}

// putLink stores a `.rclonelink` upload as a sidecar and returns the virtual
// link object.
func (f *Fs) putLink(ctx context.Context, in io.Reader, src fs.ObjectInfo, options []fs.OpenOption, remote string) (fs.Object, error) {
	target, err := readLinkTarget(in)
	if err != nil {
		return nil, err
	}

	meta, merr := fs.GetMetadataOptions(ctx, f.wrapped, src, options)
	if merr != nil {
		fs.Debugf(f, "posixmeta: cannot read incoming metadata for %q: %v", remote, merr)
		meta = nil
	}

	base := strings.TrimSuffix(remote, fs.LinkSuffix)
	sc, err := f.writeLinkSidecar(ctx, base, target, meta)
	if err != nil {
		return nil, err
	}
	f.removeLinkPayload(ctx, remote)
	return f.newLinkObjectAt(remote, sc.Symlink, time.Now()), nil
}

// storeLink implements `backend setmeta -o symlink=...` for a link that does not
// exist yet.
func (f *Fs) storeLink(ctx context.Context, remote, target string, meta fs.Metadata) error {
	base := strings.TrimSuffix(remote, fs.LinkSuffix)
	if _, err := f.writeLinkSidecar(ctx, base, target, meta); err != nil {
		return err
	}
	f.removeLinkPayload(ctx, remote)
	return nil
}

// removeLinkPayload drops a real `<name>.rclonelink` payload (legacy link, or a
// literal file with that name) once the sidecar became the authoritative store,
// along with the metadata sidecar of that payload.
func (f *Fs) removeLinkPayload(ctx context.Context, remote string) {
	obj, err := f.wrapped.NewObject(ctx, remote)
	if err != nil {
		if !errors.Is(err, fs.ErrorObjectNotFound) {
			fs.Debugf(f, "posixmeta: cannot inspect stale link payload %q: %v", remote, err)
		}
		return
	}
	if rerr := obj.Remove(ctx); rerr != nil {
		fs.Debugf(f, "posixmeta: cannot remove stale link payload %q: %v", remote, rerr)
		return
	}
	if derr := f.deleteSidecarImmediate(ctx, remote); derr != nil {
		fs.Debugf(f, "posixmeta: cannot remove stale link payload sidecar for %q: %v", remote, derr)
	}
}

// moveLink renames a proxied symlink by moving its sidecar (and a legacy
// payload, if any). Renaming it to a non-link name materialises the target as a
// regular file instead.
func (f *Fs) moveLink(ctx context.Context, src *posixLinkObject, remote string) (fs.Object, error) {
	srcTarget := strings.TrimSuffix(src.remote, fs.LinkSuffix)
	if srcTarget == "" {
		return nil, fmt.Errorf("posixmeta: invalid symlink name %q", src.remote)
	}
	if !isLinkRemote(remote) {
		return f.materializeLink(ctx, src, remote)
	}
	dstTarget := strings.TrimSuffix(remote, fs.LinkSuffix)

	srcAbs := f.absRemote(srcTarget)
	dstAbs := f.absRemote(dstTarget)
	f.quiesceAbs(ctx, srcAbs)
	f.quiesceAbs(ctx, dstAbs)

	sc, _, err := f.getSidecarInfoAbs(ctx, srcAbs)
	if err != nil {
		return nil, err
	}
	if sc == nil || sc.Symlink == "" {
		sc = &sidecarData{Version: sidecarSchemaVersion, Symlink: src.target}
	}
	if err := f.persistSidecarAbs(ctx, dstAbs, sc); err != nil {
		return nil, err
	}
	f.putCacheAt(dstAbs, sc, time.Now())
	if derr := f.deleteSidecarAbsImmediate(ctx, srcAbs); derr != nil {
		fs.Debugf(f, "posixmeta: cannot remove moved link sidecar for %q: %v", srcTarget, derr)
	}

	// Keep a legacy payload with the link rather than deleting user data; when
	// there is none, make sure the destination has no stale payload either.
	movedPayload := false
	if doMove := f.wrapped.Features().Move; doMove != nil {
		if obj, oerr := f.wrapped.NewObject(ctx, src.remote); oerr == nil {
			if _, mvErr := doMove(ctx, obj, remote); mvErr != nil {
				fs.Debugf(f, "posixmeta: cannot move legacy link payload %q: %v", src.remote, mvErr)
			} else {
				movedPayload = true
			}
		}
	}
	if !movedPayload {
		f.removeLinkPayload(ctx, remote)
	}
	return f.newLinkObjectAt(remote, sc.Symlink, time.Now()), nil
}

// materializeLink writes the symlink target as a regular file at remote and
// drops the link sidecar. It is used when a link is renamed or copied to a name
// that is not a `.rclonelink`.
func (f *Fs) materializeLink(ctx context.Context, src *posixLinkObject, remote string) (fs.Object, error) {
	info := object.NewStaticObjectInfo(remote, time.Now().UTC(), int64(len(src.target)), true, nil, f)
	obj, err := f.Put(ctx, strings.NewReader(src.target), info)
	if err != nil {
		return nil, err
	}
	srcTarget := strings.TrimSuffix(src.remote, fs.LinkSuffix)
	f.quiesceAbs(ctx, f.absRemote(srcTarget))
	if derr := f.deleteSidecarAbsImmediate(ctx, f.absRemote(srcTarget)); derr != nil {
		fs.Debugf(f, "posixmeta: cannot remove link sidecar for %q: %v", srcTarget, derr)
	}
	f.removeLinkPayload(ctx, src.remote)
	return obj, nil
}

// copyLink copies a proxied symlink, keeping it a symlink when the destination
// is a `.rclonelink` and otherwise materialising the target.
func (f *Fs) copyLink(ctx context.Context, src *posixLinkObject, remote string) (fs.Object, error) {
	if !isLinkRemote(remote) {
		info := object.NewStaticObjectInfo(remote, time.Now().UTC(), int64(len(src.target)), true, nil, f)
		return f.Put(ctx, strings.NewReader(src.target), info)
	}

	srcTarget := strings.TrimSuffix(src.remote, fs.LinkSuffix)
	dstTarget := strings.TrimSuffix(remote, fs.LinkSuffix)
	srcAbs := f.absRemote(srcTarget)
	dstAbs := f.absRemote(dstTarget)
	f.quiesceAbs(ctx, dstAbs)

	sc, modTime, err := f.getSidecarInfoAbs(ctx, srcAbs)
	if err != nil {
		return nil, err
	}
	if sc == nil || sc.Symlink == "" {
		sc = &sidecarData{Version: sidecarSchemaVersion, Symlink: src.target}
		modTime = time.Now()
	}
	if err := f.persistSidecarAbs(ctx, dstAbs, sc); err != nil {
		return nil, fmt.Errorf("posixmeta: copy link sidecar to %q: %w", remote, err)
	}
	f.putCacheAt(dstAbs, sc, modTime)
	f.removeLinkPayload(ctx, remote)
	return f.newLinkObjectAt(remote, sc.Symlink, modTime), nil
}

// posixLinkObject is the virtual `.rclonelink` object served from a sidecar.
type posixLinkObject struct {
	f       *Fs
	remote  string
	target  string
	modTime time.Time
}

func (o *posixLinkObject) Fs() fs.Info    { return o.f }
func (o *posixLinkObject) String() string { return o.remote }
func (o *posixLinkObject) Remote() string { return o.remote }
func (o *posixLinkObject) Size() int64    { return int64(len(o.target)) }
func (o *posixLinkObject) Storable() bool { return true }

func (o *posixLinkObject) ModTime(context.Context) time.Time { return o.modTime }

// SetModTime is a no-op: the link has no payload, and writing a sidecar just to
// record an mtime would defeat the "links are one object" model. A link's mtime
// follows the sidecar object's own mtime.
func (o *posixLinkObject) SetModTime(context.Context, time.Time) error { return nil }

func (o *posixLinkObject) Hash(ctx context.Context, ht hash.Type) (string, error) {
	if !o.f.Hashes().Contains(ht) {
		return "", nil
	}
	h, err := hash.NewMultiHasherTypes(hash.NewHashSet(ht))
	if err != nil {
		return "", err
	}
	if _, err := h.Write([]byte(o.target)); err != nil {
		return "", err
	}
	return h.Sums()[ht], nil
}

func (o *posixLinkObject) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	content := []byte(o.target)
	fs.FixRangeOption(options, int64(len(content)))
	offset, limit := int64(0), int64(-1)
	for _, option := range options {
		switch x := option.(type) {
		case *fs.RangeOption:
			offset, limit = x.Decode(int64(len(content)))
		default:
			if option.Mandatory() {
				return nil, fmt.Errorf("posixmeta: unsupported open option %v", option)
			}
		}
	}
	if offset > int64(len(content)) {
		offset = int64(len(content))
	}
	content = content[offset:]
	if limit >= 0 && limit < int64(len(content)) {
		content = content[:limit]
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (o *posixLinkObject) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	_, err := o.f.putLink(ctx, in, src, options, o.remote)
	return err
}

func (o *posixLinkObject) Remove(ctx context.Context) error {
	target := strings.TrimSuffix(o.remote, fs.LinkSuffix)
	if target == "" {
		return fmt.Errorf("posixmeta: invalid symlink name %q", o.remote)
	}
	absTarget := o.f.absRemote(target)
	o.f.quiesceAbs(ctx, absTarget)
	if err := o.f.deleteSidecarAbsImmediate(ctx, absTarget); err != nil {
		return err
	}
	o.f.removeLinkPayload(ctx, o.remote)
	return nil
}

// Metadata returns the resolved POSIX attributes of the link plus its target.
func (o *posixLinkObject) Metadata(ctx context.Context) (fs.Metadata, error) {
	res, err := o.f.resolve(ctx, strings.TrimSuffix(o.remote, fs.LinkSuffix), false)
	if err != nil {
		return nil, err
	}
	meta := fs.Metadata{
		"mode":    fmt.Sprintf("0%o", res.mode),
		"uid":     fmt.Sprintf("%d", res.uid),
		"gid":     fmt.Sprintf("%d", res.gid),
		"symlink": o.target,
		"mtime":   o.modTime.UTC().Format(time.RFC3339Nano),
	}
	if res.hasBTime && !res.btime.IsZero() {
		meta["btime"] = res.btime.UTC().Format(time.RFC3339Nano)
	}
	return meta, nil
}

// SetMetadata updates the sidecar of the link. The target known to this object
// is the authoritative one unless meta carries an explicit symlink attribute, so
// chmod/chown never drop the link.
func (o *posixLinkObject) SetMetadata(ctx context.Context, meta fs.Metadata) error {
	base := strings.TrimSuffix(o.remote, fs.LinkSuffix)
	target := o.target
	if newTarget, ok := lookupMetadata(meta, "symlink"); ok {
		target = newTarget
	}
	if target != "" {
		_, err := o.f.writeLinkSidecar(ctx, base, target, meta)
		return err
	}

	// Clearing the symlink turns the sidecar into a plain metadata sidecar (or
	// removes it when nothing else is set).
	cur, err := o.f.resolve(ctx, base, false)
	if err != nil {
		return err
	}
	cur.symlink = ""
	if _, _, err := applyPOSIXAttrs(&cur, meta, true); err != nil {
		return err
	}
	sc := o.f.buildSparseSidecar(ctx, base, false, cur)
	absBase := o.f.absRemote(base)
	o.f.quiesceAbs(ctx, absBase)
	if err := o.f.persistSidecarAbs(ctx, absBase, sc); err != nil {
		return err
	}
	if sc == nil {
		o.f.invalidateCacheAbs(absBase)
	} else {
		o.f.putCacheAt(absBase, sc, time.Now())
	}
	return nil
}

func (o *posixLinkObject) Chmod(ctx context.Context, mode os.FileMode) error {
	return o.SetMetadata(ctx, chmodMetadata(mode))
}

func (o *posixLinkObject) Chown(ctx context.Context, uid, gid int) error {
	if meta := chownMetadata(uid, gid); len(meta) > 0 {
		return o.SetMetadata(ctx, meta)
	}
	return nil
}

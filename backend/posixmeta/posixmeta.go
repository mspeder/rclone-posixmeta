// Package posixmeta implements an rclone wrapper backend that persists live POSIX
// metadata (mode, uid, gid, symlink target, and btime) in sparse per-entry
// sidecar files while delegating file payloads and mtime to a wrapped remote.
package posixmeta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
)

const (
	sidecarSchemaVersion = 1
	defaultSuffix        = ".posixmeta"
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "posixmeta",
		Description: "POSIX metadata sidecar wrapper remote",
		CommandHelp: []fs.CommandHelp{
			{
				Name:  "setmeta",
				Short: "Set POSIX metadata on an object or directory",
				Long:  "Updates POSIX metadata attributes (mode, uid, gid, symlink, btime) for a target path and flushes the sidecar.",
			},
		},
		NewFs: NewFs,
		MetadataInfo: &fs.MetadataInfo{
			System: map[string]fs.MetadataHelp{
				"mode": {
					Help:     "POSIX file mode and permission bits (octal, e.g. 0644 or 0755)",
					Type:     "octal",
					Example:  "0755",
					ReadOnly: false,
				},
				"uid": {
					Help:     "POSIX user ID (owner)",
					Type:     "decimal",
					Example:  "1000",
					ReadOnly: false,
				},
				"gid": {
					Help:     "POSIX group ID",
					Type:     "decimal",
					Example:  "1000",
					ReadOnly: false,
				},
				"btime": {
					Help:     "Birth / creation timestamp (RFC3339Nano)",
					Type:     "RFC 3339",
					Example:  "2026-11-22T08:00:00Z",
					ReadOnly: false,
				},
			},
			Help: "Stores sparse POSIX metadata (mode, uid, gid, symlink, btime) in per-entry sidecars.",
		},
		Options: []fs.Option{
			{
				Name:     "remote",
				Help:     "Remote to wrap (e.g., pcloud_crypt: or pcloud:folder).",
				Required: true,
			},
			{
				Name:    "default_file_mode",
				Help:    "Default POSIX mode for regular files when no sidecar override exists (octal).",
				Default: "0644",
			},
			{
				Name:    "default_dir_mode",
				Help:    "Default POSIX mode for directories when no sidecar override exists (octal).",
				Default: "0755",
			},
			{
				Name:    "default_uid",
				Help:    "Default UID when no sidecar or inherited parent UID applies (-1 uses current process UID).",
				Default: -1,
			},
			{
				Name:    "default_gid",
				Help:    "Default GID when no sidecar or inherited parent GID applies (-1 uses current process GID).",
				Default: -1,
			},
			{
				Name:    "inherit_parent_owner",
				Help:    "Inherit uid/gid from the nearest ancestor directory with an explicit override before falling back to defaults.",
				Default: true,
			},
			{
				Name:    "sidecar_suffix",
				Help:    "Suffix appended to entry names for metadata sidecars.",
				Default: defaultSuffix,
			},
			{
				Name:    "hide_sidecars",
				Help:    "Hide sidecar files from directory listings.",
				Default: true,
			},
			{
				Name:    "cache_ttl",
				Help:    "In-memory TTL for cached sidecar metadata entries.",
				Default: fs.Duration(5 * time.Minute),
			},
			{
				Name:    "writeback_delay",
				Help:    "Debounce delay before flushing coalesced sidecar metadata changes to the wrapped remote.",
				Default: fs.Duration(500 * time.Millisecond),
			},
		},
	})
}

// Options defines the configuration fields for the posixmeta backend.
type Options struct {
	Remote             string      `config:"remote"`
	DefaultFileMode    string      `config:"default_file_mode"`
	DefaultDirMode     string      `config:"default_dir_mode"`
	DefaultUID         int         `config:"default_uid"`
	DefaultGID         int         `config:"default_gid"`
	InheritParentOwner bool        `config:"inherit_parent_owner"`
	SidecarSuffix      string      `config:"sidecar_suffix"`
	HideSidecars       bool        `config:"hide_sidecars"`
	CacheTTL           fs.Duration `config:"cache_ttl"`
	WritebackDelay     fs.Duration `config:"writeback_delay"`
}

// sidecarData represents the sparse JSON payload stored in <entry>.posixmeta.
// Only fields differing from resolved defaults/inherited values are serialized.
type sidecarData struct {
	Version int     `json:"v"`
	Mode    string  `json:"mode,omitempty"`
	UID     *uint32 `json:"uid,omitempty"`
	GID     *uint32 `json:"gid,omitempty"`
	Symlink string  `json:"symlink,omitempty"`
	BTime   string  `json:"btime,omitempty"`
}

// resolvedMeta holds effective POSIX attributes for an entry.
type resolvedMeta struct {
	mode        os.FileMode
	hasMode     bool
	uid         uint32
	hasUID      bool
	gid         uint32
	hasGID      bool
	symlink     string
	btime       time.Time
	hasBTime    bool
	sidecarSeen bool
}

type cacheEntry struct {
	data      *sidecarData // nil means confirmed absent on wrapped remote
	expiresAt time.Time
}

type pendingWrite struct {
	data   *sidecarData // nil means delete sidecar
	isDir  bool
	timer  *time.Timer
	genSeq uint64
}

// Fs wraps an underlying fs.Fs and overlays sparse POSIX metadata sidecars.
type Fs struct {
	name            string
	root            string
	wrapped         fs.Fs
	base            fs.Fs // wrapped remote opened at its base root for sidecar access
	opt             Options
	features        *fs.Features
	defaultFileMode os.FileMode
	defaultDirMode  os.FileMode
	defaultUID      uint32
	defaultGID      uint32

	mu       sync.Mutex
	cache    map[string]cacheEntry
	pending  map[string]*pendingWrite
	writeGen uint64
}

// NewFs constructs a new posixmeta wrapper filesystem.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}
	if opt.Remote == "" {
		return nil, errors.New("posixmeta: 'remote' option is required")
	}
	if opt.SidecarSuffix == "" {
		opt.SidecarSuffix = defaultSuffix
	}
	if strings.Contains(opt.SidecarSuffix, "/") {
		return nil, errors.New("posixmeta: 'sidecar_suffix' must not contain '/'")
	}

	fileMode, err := parseOctalMode(opt.DefaultFileMode, 0o644)
	if err != nil {
		return nil, fmt.Errorf("posixmeta: invalid default_file_mode %q: %w", opt.DefaultFileMode, err)
	}
	dirMode, err := parseOctalMode(opt.DefaultDirMode, 0o755)
	if err != nil {
		return nil, fmt.Errorf("posixmeta: invalid default_dir_mode %q: %w", opt.DefaultDirMode, err)
	}

	defUID := uint32(os.Getuid())
	if opt.DefaultUID >= 0 {
		defUID = uint32(opt.DefaultUID)
	}
	defGID := uint32(os.Getgid())
	if opt.DefaultGID >= 0 {
		defGID = uint32(opt.DefaultGID)
	}

	wrappedRoot := opt.Remote
	if root != "" {
		if strings.HasSuffix(wrappedRoot, ":") || strings.HasSuffix(wrappedRoot, "/") {
			wrappedRoot += root
		} else {
			wrappedRoot += "/" + root
		}
	}

	wrappedFs, err := fs.NewFs(ctx, wrappedRoot)
	if err != nil && !errors.Is(err, fs.ErrorIsFile) {
		return nil, fmt.Errorf("posixmeta: failed to open wrapped remote %q: %w", wrappedRoot, err)
	}

	// Backends that support file roots (e.g. local) return ErrorIsFile and
	// re-root themselves at the file's parent directory. Mirror that so rclone
	// commands such as `moveto pm:file pm:file2` operate on the parent
	// directory with the file name as the source remote.
	actualRoot := root
	if errors.Is(err, fs.ErrorIsFile) {
		actualRoot = parentRemote(root)
	}

	f := &Fs{
		name:            name,
		root:            actualRoot,
		wrapped:         wrappedFs,
		base:            wrappedFs,
		opt:             *opt,
		defaultFileMode: fileMode,
		defaultDirMode:  dirMode,
		defaultUID:      defUID,
		defaultGID:      defGID,
		cache:           make(map[string]cacheEntry),
		pending:         make(map[string]*pendingWrite),
	}

	// When rooted at a subdirectory, sidecars for the root entry itself and for
	// its ancestors live outside the wrapped root. Open the wrapped remote at
	// its base so those sidecars can still be read, written, moved and removed.
	if actualRoot != "" && opt.Remote != wrappedRoot {
		if baseFs, baseErr := fs.NewFs(ctx, opt.Remote); baseErr != nil {
			fs.Debugf(f, "failed to open sidecar base remote %q: %v", opt.Remote, baseErr)
		} else {
			f.base = baseFs
		}
	}

	f.features = (&fs.Features{
		CaseInsensitive:          wrappedFs.Features().CaseInsensitive,
		DuplicateFiles:           wrappedFs.Features().DuplicateFiles,
		ReadMimeType:             wrappedFs.Features().ReadMimeType,
		WriteMimeType:            wrappedFs.Features().WriteMimeType,
		CanHaveEmptyDirectories:  wrappedFs.Features().CanHaveEmptyDirectories,
		BucketBased:              wrappedFs.Features().BucketBased,
		BucketBasedRootOK:        wrappedFs.Features().BucketBasedRootOK,
		SetTier:                  wrappedFs.Features().SetTier,
		GetTier:                  wrappedFs.Features().GetTier,
		ReadMetadata:             true,
		WriteMetadata:            true,
		UserMetadata:             true,
		DirModTimeUpdatesOnWrite: wrappedFs.Features().DirModTimeUpdatesOnWrite,
	}).Fill(ctx, f)

	// Disable ServerSideAcrossConfigs unless wrapped supports it cleanly with sidecar awareness.
	f.features.ServerSideAcrossConfigs = false

	return f, err
}

// Name returns the configured remote name.
func (f *Fs) Name() string { return f.name }

// Root returns the root path within the remote.
func (f *Fs) Root() string { return f.root }

// String returns a human-readable description of the remote.
func (f *Fs) String() string {
	return fmt.Sprintf("posixmeta(%s)", f.wrapped.String())
}

// Precision returns the timestamp precision of the wrapped filesystem.
func (f *Fs) Precision() time.Duration { return f.wrapped.Precision() }

// Hashes returns the supported hash types of the wrapped filesystem.
func (f *Fs) Hashes() hash.Set { return f.wrapped.Hashes() }

// Features returns the optional features supported by this wrapper.
func (f *Fs) Features() *fs.Features { return f.features }

// UnWrap returns the underlying wrapped fs.Fs.
func (f *Fs) UnWrap() fs.Fs { return f.wrapped }

// WrapFs returns the filesystem wrapping this one, if any.
func (f *Fs) WrapFs() fs.Fs { return nil }

// SetWrapper satisfies fs.Wrapper if embedded in another wrapper.
func (f *Fs) SetWrapper(wrapper fs.Fs) {}

// absRemote maps a remote relative to this Fs root to a path relative to the
// wrapped remote's base root (the value of the `remote` option).
func (f *Fs) absRemote(remote string) string {
	return joinRootRemote(f.root, remote)
}

// joinRootRemote joins a wrapper root and a wrapper-relative remote, treating
// "" as the base root.
func joinRootRemote(root, remote string) string {
	root = strings.Trim(root, "/")
	remote = strings.Trim(remote, "/")
	switch {
	case root == "":
		return remote
	case remote == "":
		return root
	default:
		return root + "/" + remote
	}
}

func (f *Fs) sidecarRemote(remote string) string {
	clean := strings.TrimSuffix(remote, "/")
	return clean + f.opt.SidecarSuffix
}

func (f *Fs) isSidecar(remote string) bool {
	return strings.HasSuffix(remote, f.opt.SidecarSuffix)
}

// List lists objects and directories, hiding sidecar files and wrapping returned entries.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := f.wrapped.List(ctx, dir)
	if err != nil {
		return nil, err
	}

	out := make(fs.DirEntries, 0, len(entries))
	for _, entry := range entries {
		remote := entry.Remote()
		if f.isSidecar(remote) {
			if f.opt.HideSidecars {
				continue
			}
		}
		switch x := entry.(type) {
		case fs.Object:
			out = append(out, f.wrapObject(x))
		case fs.Directory:
			out = append(out, f.wrapDirectory(x))
		default:
			out = append(out, entry)
		}
	}
	return out, nil
}

// NewObject finds the object at remote and wraps it with POSIX metadata support.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if f.opt.HideSidecars && f.isSidecar(remote) {
		return nil, fs.ErrorObjectNotFound
	}
	obj, err := f.wrapped.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	return f.wrapObject(obj), nil
}

// Put uploads a new file and writes a sparse sidecar only if incoming metadata differs from defaults.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	if f.opt.HideSidecars && f.isSidecar(remote) {
		return nil, fmt.Errorf("posixmeta: cannot write directly to sidecar path %q", remote)
	}

	wrappedObj, err := f.wrapped.Put(ctx, in, src, options...)
	if err != nil {
		return nil, err
	}

	po := &posixObject{Object: wrappedObj, f: f}
	if poInner, ok := wrappedObj.(*posixObject); ok {
		po = poInner
	}

	// Extract any incoming metadata (e.g., from --metadata or VFS) and persist if non-default.
	if meta, err := fs.GetMetadataOptions(ctx, f.wrapped, src, options); err == nil && len(meta) > 0 {
		_ = po.SetMetadata(ctx, meta)
	}

	return po, nil
}

// Mkdir creates a directory on the wrapped remote.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	return f.wrapped.Mkdir(ctx, dir)
}

// Rmdir removes an empty directory and cleans up its directory sidecar if present.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if err := f.wrapped.Rmdir(ctx, dir); err != nil {
		return err
	}
	_ = f.deleteSidecarImmediate(ctx, dir)
	return nil
}

// Purge deletes a directory and all its contents (including sidecars).
func (f *Fs) Purge(ctx context.Context, dir string) error {
	if do := f.wrapped.Features().Purge; do != nil {
		if err := do(ctx, dir); err != nil {
			return err
		}
		f.invalidatePrefix(dir)
		_ = f.deleteSidecarImmediate(ctx, dir)
		return nil
	}
	return fs.ErrorCantPurge
}

// Move moves/renames an object along with its sidecar file if present.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	doMove := f.wrapped.Features().Move
	if doMove == nil {
		return nil, fs.ErrorCantMove
	}

	srcRemote := src.Remote()
	rawSrc := unwrapObject(src)

	movedRaw, err := doMove(ctx, rawSrc, remote)
	if err != nil {
		return nil, err
	}

	// Flush pending write for srcRemote before moving its sidecar.
	_ = f.flushPending(ctx, srcRemote)

	srcAbs := f.absRemote(srcRemote)
	dstAbs := f.absRemote(remote)
	srcSidecar := f.sidecarRemote(srcAbs)
	dstSidecar := f.sidecarRemote(dstAbs)
	// Sidecars are addressed relative to the wrapped base root so entries at or
	// above this Fs root (e.g. a root-level file's own sidecar) are handled.
	if doSidecarMove := f.base.Features().Move; doSidecarMove != nil {
		if scObj, err := f.base.NewObject(ctx, srcSidecar); err == nil {
			if _, mvErr := doSidecarMove(ctx, scObj, dstSidecar); mvErr != nil && !errors.Is(mvErr, fs.ErrorObjectNotFound) {
				fs.Debugf(f, "sidecar move %q -> %q skipped: %v", srcSidecar, dstSidecar, mvErr)
			}
		}
	}

	f.invalidateCache(srcRemote)
	f.invalidateCache(remote)
	return f.wrapObject(movedRaw), nil
}

// DirMove moves/renames a directory along with its directory-level sidecar.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	doDirMove := f.wrapped.Features().DirMove
	if doDirMove == nil {
		return fs.ErrorCantDirMove
	}

	wrappedSrc := src
	srcRoot := ""
	srcBase := f.base
	if pmSrc, ok := src.(*Fs); ok {
		wrappedSrc = pmSrc.wrapped
		srcRoot = pmSrc.root
		srcBase = pmSrc.base
	}

	if err := doDirMove(ctx, wrappedSrc, srcRemote, dstRemote); err != nil {
		return err
	}

	// Move directory-level sidecar (<dir>.posixmeta) if one exists. Paths are
	// base-relative so a moved root directory keeps its sidecar, which lives
	// just outside the wrapped root.
	if doMove := srcBase.Features().Move; doMove != nil {
		srcSidecar := f.sidecarRemote(joinRootRemote(srcRoot, srcRemote))
		dstSidecar := f.sidecarRemote(f.absRemote(dstRemote))
		if scObj, err := srcBase.NewObject(ctx, srcSidecar); err == nil {
			if _, mvErr := doMove(ctx, scObj, dstSidecar); mvErr != nil {
				fs.Errorf(f, "failed to move dir sidecar %q -> %q: %v", srcSidecar, dstSidecar, mvErr)
			}
		}
	}

	f.invalidatePrefix(srcRemote)
	f.invalidatePrefix(dstRemote)
	return nil
}

// DirSetModTime sets directory mtime on the wrapped remote if supported.
func (f *Fs) DirSetModTime(ctx context.Context, dir string, modTime time.Time) error {
	if do := f.wrapped.Features().DirSetModTime; do != nil {
		return do(ctx, dir, modTime)
	}
	return fs.ErrorNotImplemented
}

// CleanUp flushes any pending debounced sidecar writes and delegates CleanUp to wrapped remote.
func (f *Fs) CleanUp(ctx context.Context) error {
	_ = f.flushAllPending(ctx)
	if do := f.wrapped.Features().CleanUp; do != nil {
		return do(ctx)
	}
	return nil
}

// Command allows calling backend-specific helpers such as `setmeta` for testing and tooling.
// Example: rclone backend setmeta pm_local: hello.txt -o mode=0750 -o uid=1001 -o gid=2002
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (any, error) {
	switch name {
	case "setmeta":
		if len(arg) < 1 {
			return nil, errors.New("posixmeta: setmeta requires a target remote path argument")
		}
		target := strings.Trim(arg[0], "/")
		meta := make(fs.Metadata, len(opt))
		for k, v := range opt {
			meta[k] = v
		}
		if obj, err := f.NewObject(ctx, target); err == nil {
			po, ok := obj.(*posixObject)
			if !ok {
				return nil, fmt.Errorf("posixmeta: expected *posixObject for %q", target)
			}
			if err := po.SetMetadata(ctx, meta); err != nil {
				return nil, err
			}
			if flushErr := f.flushPending(ctx, target); flushErr != nil {
				return nil, flushErr
			}
			return po.Metadata(ctx)
		}
		// Fallback to directory if not an object.
		pd := f.wrapDirectory(&staticDir{remote: target, when: time.Now().UTC(), fsInfo: f})
		if err := pd.(*posixDirectory).SetMetadata(ctx, meta); err != nil {
			return nil, err
		}
		if flushErr := f.flushPending(ctx, target); flushErr != nil {
			return nil, flushErr
		}
		return pd.(*posixDirectory).Metadata(ctx)
	default:
		if do := f.wrapped.Features().Command; do != nil {
			return do(ctx, name, arg, opt)
		}
		return nil, fs.ErrorCommandNotFound
	}
}

type staticDir struct {
	remote string
	when   time.Time
	fsInfo fs.Info
}

func (d *staticDir) String() string                    { return d.remote }
func (d *staticDir) Remote() string                    { return d.remote }
func (d *staticDir) ModTime(context.Context) time.Time { return d.when }
func (d *staticDir) Size() int64                       { return 0 }
func (d *staticDir) Items() int64                      { return -1 }
func (d *staticDir) ID() string                        { return "" }
func (d *staticDir) Fs() fs.Info                       { return d.fsInfo }
func (d *staticDir) SetModTime(context.Context, time.Time) error {
	return fs.ErrorNotImplemented
}

// ---------------------------------------------------------------------------
// Sidecar Resolution, Inheritance, Caching, and Coalesced Writeback
// ---------------------------------------------------------------------------

func (f *Fs) getSidecar(ctx context.Context, remote string) (*sidecarData, error) {
	return f.getSidecarAbs(ctx, f.absRemote(remote))
}

// getSidecarAbs loads the sidecar for a path relative to the wrapped remote's
// base root, caching and pending writes keyed by that absolute path.
func (f *Fs) getSidecarAbs(ctx context.Context, absRemote string) (*sidecarData, error) {
	now := time.Now()

	f.mu.Lock()
	if pw, ok := f.pending[absRemote]; ok {
		cp := cloneSidecar(pw.data)
		f.mu.Unlock()
		return cp, nil
	}
	if ce, ok := f.cache[absRemote]; ok && now.Before(ce.expiresAt) {
		cp := cloneSidecar(ce.data)
		f.mu.Unlock()
		return cp, nil
	}
	f.mu.Unlock()

	scPath := f.sidecarRemote(absRemote)
	scObj, err := f.base.NewObject(ctx, scPath)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			f.putCache(absRemote, nil)
			return nil, nil
		}
		return nil, err
	}

	rc, err := scObj.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	var parsed sidecarData
	if err := json.Unmarshal(raw, &parsed); err != nil {
		fs.Errorf(f, "corrupt sidecar JSON at %q: %v", scPath, err)
		f.putCache(absRemote, nil)
		return nil, nil
	}

	f.putCache(absRemote, &parsed)
	return cloneSidecar(&parsed), nil
}

func (f *Fs) resolve(ctx context.Context, remote string, isDir bool) (resolvedMeta, error) {
	var res resolvedMeta
	if isDir {
		res.mode = f.defaultDirMode
	} else {
		res.mode = f.defaultFileMode
	}
	res.uid = f.defaultUID
	res.gid = f.defaultGID

	// Inherit UID/GID from nearest ancestor directory with an explicit override.
	if f.opt.InheritParentOwner {
		ancestors := ancestorDirs(f.absRemote(remote))
		var foundUID, foundGID bool
		for _, anc := range ancestors {
			sc, err := f.getSidecarAbs(ctx, anc)
			if err != nil || sc == nil {
				continue
			}
			if !foundUID && sc.UID != nil {
				res.uid = *sc.UID
				res.hasUID = true
				foundUID = true
			}
			if !foundGID && sc.GID != nil {
				res.gid = *sc.GID
				res.hasGID = true
				foundGID = true
			}
			if foundUID && foundGID {
				break
			}
		}
	}

	sc, err := f.getSidecar(ctx, remote)
	if err != nil {
		return res, err
	}
	if sc == nil {
		return res, nil
	}

	res.sidecarSeen = true
	if sc.Mode != "" {
		if parsed, err := parseOctalMode(sc.Mode, res.mode); err == nil {
			res.mode = parsed
			res.hasMode = true
		}
	}
	if sc.UID != nil {
		res.uid = *sc.UID
		res.hasUID = true
	}
	if sc.GID != nil {
		res.gid = *sc.GID
		res.hasGID = true
	}
	if sc.Symlink != "" {
		res.symlink = sc.Symlink
	}
	if sc.BTime != "" {
		if t, err := time.Parse(time.RFC3339Nano, sc.BTime); err == nil {
			res.btime = t
			res.hasBTime = true
		}
	}
	return res, nil
}

// buildSparseSidecar returns nil if all fields match defaults / inherited parent owner.
func (f *Fs) buildSparseSidecar(ctx context.Context, remote string, isDir bool, desired resolvedMeta) *sidecarData {
	defaultMode := f.defaultFileMode
	if isDir {
		defaultMode = f.defaultDirMode
	}

	inheritedUID := f.defaultUID
	inheritedGID := f.defaultGID
	if f.opt.InheritParentOwner {
		var foundUID, foundGID bool
		for _, anc := range ancestorDirs(f.absRemote(remote)) {
			sc, err := f.getSidecarAbs(ctx, anc)
			if err != nil || sc == nil {
				continue
			}
			if !foundUID && sc.UID != nil {
				inheritedUID = *sc.UID
				foundUID = true
			}
			if !foundGID && sc.GID != nil {
				inheritedGID = *sc.GID
				foundGID = true
			}
			if foundUID && foundGID {
				break
			}
		}
	}

	sc := &sidecarData{
		Version: sidecarSchemaVersion,
	}
	hasAny := false

	if desired.hasMode && desired.mode != defaultMode {
		sc.Mode = fmt.Sprintf("0%o", desired.mode)
		hasAny = true
	}
	if desired.hasUID && desired.uid != inheritedUID {
		u := desired.uid
		sc.UID = &u
		hasAny = true
	}
	if desired.hasGID && desired.gid != inheritedGID {
		g := desired.gid
		sc.GID = &g
		hasAny = true
	}
	if desired.symlink != "" {
		sc.Symlink = desired.symlink
		hasAny = true
	}
	if desired.hasBTime && !desired.btime.IsZero() {
		sc.BTime = desired.btime.UTC().Format(time.RFC3339Nano)
		hasAny = true
	}

	if !hasAny {
		return nil
	}
	return sc
}

func (f *Fs) scheduleSidecarUpdate(ctx context.Context, remote string, isDir bool, sc *sidecarData) error {
	absRemote := f.absRemote(remote)
	delay := time.Duration(f.opt.WritebackDelay)
	f.putCache(absRemote, sc)

	if delay <= 0 {
		return f.persistSidecarAbs(ctx, absRemote, sc)
	}

	f.mu.Lock()
	f.writeGen++
	gen := f.writeGen
	if existing, ok := f.pending[absRemote]; ok {
		existing.timer.Stop()
		existing.data = cloneSidecar(sc)
		existing.isDir = isDir
		existing.genSeq = gen
		existing.timer = time.AfterFunc(delay, func() {
			f.runScheduledWrite(absRemote, gen)
		})
		f.mu.Unlock()
		return nil
	}

	pw := &pendingWrite{
		data:   cloneSidecar(sc),
		isDir:  isDir,
		genSeq: gen,
	}
	pw.timer = time.AfterFunc(delay, func() {
		f.runScheduledWrite(absRemote, gen)
	})
	f.pending[absRemote] = pw
	f.mu.Unlock()
	return nil
}

func (f *Fs) runScheduledWrite(absRemote string, gen uint64) {
	f.mu.Lock()
	pw, ok := f.pending[absRemote]
	if !ok || pw.genSeq != gen {
		f.mu.Unlock()
		return
	}
	data := cloneSidecar(pw.data)
	delete(f.pending, absRemote)
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := f.persistSidecarAbs(ctx, absRemote, data); err != nil {
		fs.Errorf(f, "async sidecar writeback failed for %q: %v", absRemote, err)
	}
}

func (f *Fs) flushPending(ctx context.Context, remote string) error {
	return f.flushPendingAbs(ctx, f.absRemote(remote))
}

func (f *Fs) flushPendingAbs(ctx context.Context, absRemote string) error {
	f.mu.Lock()
	pw, ok := f.pending[absRemote]
	if !ok {
		f.mu.Unlock()
		return nil
	}
	pw.timer.Stop()
	data := cloneSidecar(pw.data)
	delete(f.pending, absRemote)
	f.mu.Unlock()

	return f.persistSidecarAbs(ctx, absRemote, data)
}

func (f *Fs) flushAllPending(ctx context.Context) error {
	f.mu.Lock()
	remotes := make([]string, 0, len(f.pending))
	for r := range f.pending {
		remotes = append(remotes, r)
	}
	f.mu.Unlock()

	var firstErr error
	for _, r := range remotes {
		if err := f.flushPendingAbs(ctx, r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// persistSidecarAbs writes (or deletes) the sidecar for an absolute path.
func (f *Fs) persistSidecarAbs(ctx context.Context, absRemote string, sc *sidecarData) error {
	if sc == nil {
		return f.deleteSidecarAbsImmediate(ctx, absRemote)
	}
	scPath := f.sidecarRemote(absRemote)

	payload, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	info := object.NewStaticObjectInfo(
		scPath,
		time.Now().UTC(),
		int64(len(payload)),
		true,
		nil,
		f.base,
	)
	_, err = f.base.Put(ctx, bytes.NewReader(payload), info)
	return err
}

func (f *Fs) deleteSidecarImmediate(ctx context.Context, remote string) error {
	return f.deleteSidecarAbsImmediate(ctx, f.absRemote(remote))
}

func (f *Fs) deleteSidecarAbsImmediate(ctx context.Context, absRemote string) error {
	f.invalidateCacheAbs(absRemote)
	scPath := f.sidecarRemote(absRemote)
	scObj, err := f.base.NewObject(ctx, scPath)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return nil
		}
		return err
	}
	return scObj.Remove(ctx)
}

func (f *Fs) putCache(absRemote string, sc *sidecarData) {
	ttl := time.Duration(f.opt.CacheTTL)
	if ttl <= 0 {
		return
	}
	f.mu.Lock()
	f.cache[absRemote] = cacheEntry{
		data:      cloneSidecar(sc),
		expiresAt: time.Now().Add(ttl),
	}
	f.mu.Unlock()
}

func (f *Fs) invalidateCache(remote string) {
	f.invalidateCacheAbs(f.absRemote(remote))
}

func (f *Fs) invalidateCacheAbs(absRemote string) {
	f.mu.Lock()
	delete(f.cache, absRemote)
	f.mu.Unlock()
}

func (f *Fs) invalidatePrefix(dir string) {
	prefix := f.absRemote(strings.TrimSuffix(dir, "/"))
	if prefix == "" {
		f.mu.Lock()
		clear(f.cache)
		f.mu.Unlock()
		return
	}
	f.mu.Lock()
	for k := range f.cache {
		if k == prefix || strings.HasPrefix(k, prefix+"/") {
			delete(f.cache, k)
		}
	}
	f.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Wrapped Object Implementation
// ---------------------------------------------------------------------------

type posixObject struct {
	fs.Object
	f *Fs
}

func (f *Fs) wrapObject(o fs.Object) fs.Object {
	if po, ok := o.(*posixObject); ok {
		return po
	}
	return &posixObject{Object: o, f: f}
}

func unwrapObject(o fs.Object) fs.Object {
	if po, ok := o.(*posixObject); ok {
		return po.Object
	}
	return o
}

func (o *posixObject) Fs() fs.Info { return o.f }

func (o *posixObject) UnWrap() fs.Object { return o.Object }

func (o *posixObject) SetModTime(ctx context.Context, t time.Time) error {
	return o.Object.SetModTime(ctx, t)
}

func (o *posixObject) Hash(ctx context.Context, ht hash.Type) (string, error) {
	return o.Object.Hash(ctx, ht)
}

func (o *posixObject) MimeType(ctx context.Context) string {
	if do, ok := o.Object.(interface {
		MimeType(context.Context) string
	}); ok {
		return do.MimeType(ctx)
	}
	return fs.MimeType(ctx, o.Object)
}

func (o *posixObject) Remove(ctx context.Context) error {
	remote := o.Remote()
	if err := o.Object.Remove(ctx); err != nil {
		return err
	}
	_ = o.f.deleteSidecarImmediate(ctx, remote)
	return nil
}

// Chmod updates the POSIX permission bits on the object via its sparse sidecar.
func (o *posixObject) Chmod(ctx context.Context, mode os.FileMode) error {
	return o.SetMetadata(ctx, fs.Metadata{
		"mode": fmt.Sprintf("0%o", mode.Perm()),
	})
}

// Chown updates the POSIX uid and gid on the object via its sparse sidecar.
func (o *posixObject) Chown(ctx context.Context, uid, gid int) error {
	meta := make(fs.Metadata, 2)
	if uid >= 0 {
		meta["uid"] = strconv.Itoa(uid)
	}
	if gid >= 0 {
		meta["gid"] = strconv.Itoa(gid)
	}
	if len(meta) == 0 {
		return nil
	}
	return o.SetMetadata(ctx, meta)
}

// Metadata merges underlying object metadata with resolved POSIX sidecar attributes.
func (o *posixObject) Metadata(ctx context.Context) (fs.Metadata, error) {
	meta := make(fs.Metadata)
	if do, ok := o.Object.(fs.Metadataer); ok {
		if m, err := do.Metadata(ctx); err == nil && m != nil {
			for k, v := range m {
				meta[k] = v
			}
		}
	}

	res, err := o.f.resolve(ctx, o.Remote(), false)
	if err != nil {
		return meta, err
	}

	meta["mode"] = fmt.Sprintf("0%o", res.mode)
	meta["uid"] = strconv.FormatUint(uint64(res.uid), 10)
	meta["gid"] = strconv.FormatUint(uint64(res.gid), 10)
	if res.symlink != "" {
		meta["symlink"] = res.symlink
	}
	if res.hasBTime && !res.btime.IsZero() {
		meta["btime"] = res.btime.UTC().Format(time.RFC3339Nano)
	}
	if _, exists := meta["mtime"]; !exists {
		meta["mtime"] = o.ModTime(ctx).UTC().Format(time.RFC3339Nano)
	}
	return meta, nil
}

// SetMetadata updates POSIX attributes (mode, uid, gid, symlink, btime) in the sparse sidecar
// and forwards any remaining metadata/mtime changes to the wrapped object.
func (o *posixObject) SetMetadata(ctx context.Context, meta fs.Metadata) error {
	remote := o.Remote()
	cur, err := o.f.resolve(ctx, remote, false)
	if err != nil {
		return err
	}

	passthrough := make(fs.Metadata)
	posixTouched := false

	for k, v := range meta {
		switch strings.ToLower(k) {
		case "mode":
			m, err := parseOctalMode(v, cur.mode)
			if err != nil {
				return fmt.Errorf("posixmeta: invalid mode %q: %w", v, err)
			}
			cur.mode = m
			cur.hasMode = true
			posixTouched = true
		case "uid":
			u, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return fmt.Errorf("posixmeta: invalid uid %q: %w", v, err)
			}
			cur.uid = uint32(u)
			cur.hasUID = true
			posixTouched = true
		case "gid":
			g, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return fmt.Errorf("posixmeta: invalid gid %q: %w", v, err)
			}
			cur.gid = uint32(g)
			cur.hasGID = true
			posixTouched = true
		case "symlink":
			cur.symlink = v
			posixTouched = true
		case "btime":
			if v == "" {
				cur.btime = time.Time{}
				cur.hasBTime = false
			} else {
				bt, err := time.Parse(time.RFC3339Nano, v)
				if err != nil {
					return fmt.Errorf("posixmeta: invalid btime %q: %w", v, err)
				}
				cur.btime = bt
				cur.hasBTime = true
			}
			posixTouched = true
		case "atime", "ctime":
			// Intentionally ignored to prevent read/mutation write churn.
		default:
			passthrough[k] = v
		}
	}

	if posixTouched {
		sc := o.f.buildSparseSidecar(ctx, remote, false, cur)
		if err := o.f.scheduleSidecarUpdate(ctx, remote, false, sc); err != nil {
			return err
		}
	}

	if len(passthrough) > 0 {
		if do, ok := o.Object.(fs.SetMetadataer); ok {
			if err := do.SetMetadata(ctx, passthrough); err != nil && !errors.Is(err, fs.ErrorNotImplemented) {
				return err
			}
		} else if mtimeStr, ok := passthrough["mtime"]; ok {
			if mt, err := time.Parse(time.RFC3339Nano, mtimeStr); err == nil {
				_ = o.Object.SetModTime(ctx, mt)
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Wrapped Directory Implementation
// ---------------------------------------------------------------------------

type posixDirectory struct {
	fs.Directory
	f *Fs
}

func (f *Fs) wrapDirectory(d fs.Directory) fs.Directory {
	if pd, ok := d.(*posixDirectory); ok {
		return pd
	}
	return &posixDirectory{Directory: d, f: f}
}

func (d *posixDirectory) Metadata(ctx context.Context) (fs.Metadata, error) {
	meta := make(fs.Metadata)
	if do, ok := d.Directory.(fs.Metadataer); ok {
		if m, err := do.Metadata(ctx); err == nil && m != nil {
			for k, v := range m {
				meta[k] = v
			}
		}
	}

	res, err := d.f.resolve(ctx, d.Remote(), true)
	if err != nil {
		return meta, err
	}

	meta["mode"] = fmt.Sprintf("0%o", res.mode)
	meta["uid"] = strconv.FormatUint(uint64(res.uid), 10)
	meta["gid"] = strconv.FormatUint(uint64(res.gid), 10)
	if res.hasBTime && !res.btime.IsZero() {
		meta["btime"] = res.btime.UTC().Format(time.RFC3339Nano)
	}
	if _, exists := meta["mtime"]; !exists {
		meta["mtime"] = d.ModTime(ctx).UTC().Format(time.RFC3339Nano)
	}
	return meta, nil
}

func (d *posixDirectory) SetMetadata(ctx context.Context, meta fs.Metadata) error {
	remote := d.Remote()
	cur, err := d.f.resolve(ctx, remote, true)
	if err != nil {
		return err
	}

	passthrough := make(fs.Metadata)
	posixTouched := false

	for k, v := range meta {
		switch strings.ToLower(k) {
		case "mode":
			m, err := parseOctalMode(v, cur.mode)
			if err != nil {
				return fmt.Errorf("posixmeta: invalid dir mode %q: %w", v, err)
			}
			cur.mode = m
			cur.hasMode = true
			posixTouched = true
		case "uid":
			u, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return fmt.Errorf("posixmeta: invalid dir uid %q: %w", v, err)
			}
			cur.uid = uint32(u)
			cur.hasUID = true
			posixTouched = true
		case "gid":
			g, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return fmt.Errorf("posixmeta: invalid dir gid %q: %w", v, err)
			}
			cur.gid = uint32(g)
			cur.hasGID = true
			posixTouched = true
		case "btime":
			if v == "" {
				cur.btime = time.Time{}
				cur.hasBTime = false
			} else {
				bt, err := time.Parse(time.RFC3339Nano, v)
				if err != nil {
					return fmt.Errorf("posixmeta: invalid dir btime %q: %w", v, err)
				}
				cur.btime = bt
				cur.hasBTime = true
			}
			posixTouched = true
		case "atime", "ctime":
			// Intentionally ignored.
		default:
			passthrough[k] = v
		}
	}

	if posixTouched {
		sc := d.f.buildSparseSidecar(ctx, remote, true, cur)
		if err := d.f.scheduleSidecarUpdate(ctx, remote, true, sc); err != nil {
			return err
		}
		// Invalidate cached resolutions under this directory when owner changes.
		d.f.invalidatePrefix(remote)
	}

	if len(passthrough) > 0 {
		if do, ok := d.Directory.(fs.SetMetadataer); ok {
			if err := do.SetMetadata(ctx, passthrough); err != nil && !errors.Is(err, fs.ErrorNotImplemented) {
				return err
			}
		} else if mtimeStr, ok := passthrough["mtime"]; ok {
			if mt, err := time.Parse(time.RFC3339Nano, mtimeStr); err == nil {
				_ = d.f.DirSetModTime(ctx, remote, mt)
			}
		}
	}

	return nil
}

// Chmod updates the POSIX permission bits on the directory via its sparse sidecar.
func (d *posixDirectory) Chmod(ctx context.Context, mode os.FileMode) error {
	return d.SetMetadata(ctx, fs.Metadata{
		"mode": fmt.Sprintf("0%o", mode.Perm()),
	})
}

// Chown updates the POSIX uid and gid on the directory via its sparse sidecar.
func (d *posixDirectory) Chown(ctx context.Context, uid, gid int) error {
	meta := make(fs.Metadata, 2)
	if uid >= 0 {
		meta["uid"] = strconv.Itoa(uid)
	}
	if gid >= 0 {
		meta["gid"] = strconv.Itoa(gid)
	}
	if len(meta) == 0 {
		return nil
	}
	return d.SetMetadata(ctx, meta)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseOctalMode(s string, fallback os.FileMode) (os.FileMode, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return fallback, nil
	}
	trimmed = strings.TrimPrefix(trimmed, "0o")
	trimmed = strings.TrimPrefix(trimmed, "0O")
	val, err := strconv.ParseUint(trimmed, 8, 32)
	if err != nil {
		return 0, err
	}
	return os.FileMode(val), nil
}

// parentRemote returns the parent of a wrapper-relative path ("" for top-level).
func parentRemote(remote string) string {
	clean := strings.Trim(remote, "/")
	parent := path.Dir(clean)
	if parent == "." || parent == "/" {
		return ""
	}
	return parent
}

// ancestorDirs returns parent directory paths ordered nearest-first (e.g., "a/b/c" -> ["a/b", "a"]).
func ancestorDirs(remote string) []string {
	clean := strings.Trim(remote, "/")
	if clean == "" || !strings.Contains(clean, "/") {
		return nil
	}
	var out []string
	cur := path.Dir(clean)
	for cur != "." && cur != "/" && cur != "" {
		out = append(out, cur)
		next := path.Dir(cur)
		if next == cur {
			break
		}
		cur = next
	}
	return out
}

func cloneSidecar(sc *sidecarData) *sidecarData {
	if sc == nil {
		return nil
	}
	cp := *sc
	if sc.UID != nil {
		u := *sc.UID
		cp.UID = &u
	}
	if sc.GID != nil {
		g := *sc.GID
		cp.GID = &g
	}
	return &cp
}

var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Wrapper         = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.CleanUpper      = (*Fs)(nil)
	_ fs.DirSetModTimer  = (*Fs)(nil)
	_ fs.Commander       = (*Fs)(nil)
	_ fs.Object          = (*posixObject)(nil)
	_ fs.ObjectUnWrapper = (*posixObject)(nil)
	_ fs.Metadataer      = (*posixObject)(nil)
	_ fs.SetMetadataer   = (*posixObject)(nil)
	_ fs.Directory       = (*posixDirectory)(nil)
	_ fs.Metadataer      = (*posixDirectory)(nil)
	_ fs.SetMetadataer   = (*posixDirectory)(nil)
	_ fserrors.RetryAfter
)

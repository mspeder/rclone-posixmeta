// Package posixmeta implements an rclone wrapper backend that persists live POSIX
// metadata (mode, uid, gid, symlink target, and btime) in sparse per-entry
// sidecar files while delegating file payloads and mtime to a wrapped remote.
package posixmeta

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/atexit"
)

const (
	sidecarSchemaVersion = 1
	defaultSidecarPrefix = ".posixmeta"

	// maxLinkTargetSize bounds the target read when a symlink is proxied into
	// its sidecar. A real target is far below PATH_MAX, so anything larger is
	// rejected instead of being stored.
	maxLinkTargetSize = 64 << 10

	// overwriteMetadataPreserve keeps an existing sidecar when an object is
	// overwritten without POSIX metadata (the safe default for VFS writeback).
	overwriteMetadataPreserve = "preserve"
	// overwriteMetadataReset drops the sidecar unless the upload carries POSIX
	// metadata, so stale mode/owner never leaks onto new content.
	overwriteMetadataReset = "reset"
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
			{
				Name:  "cachestats",
				Short: "Show sidecar cache occupancy and effectiveness",
				Long:  "Reports cached entry count, approximate memory usage, configured caps, hit/miss/eviction counters and the pending writeback count.",
			},
			{
				Name:  "repair",
				Short: "Check and repair directory metadata maps",
				Long:  "Validates every directory map of a sidecar_layout=dir remote, optionally quarantining and salvaging corrupt maps, dropping invalid entries and orphan keys, and removing stale staging objects. Dry run unless -o dry_run=false -o confirm=REPAIR is given.",
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
				Name:    "sidecar_prefix",
				Help:    "Prefix used to name sidecars in the same directory as their entry: `<prefix>.<name>`. The base root entry, which has no name, uses the bare prefix.",
				Default: defaultSidecarPrefix,
			},
			{
				Name:    "sidecar_layout",
				Help:    "On-disk metadata layout: 'sparse' stores one `<prefix>.<name>` sidecar per entry that has overrides, 'dir' stores one `<prefix>` map per directory holding all of its entries. The layout is recorded in the remote's root map and must match, since the two are never converted automatically.",
				Default: layoutSparse,
				Examples: []fs.OptionExample{
					{Value: layoutSparse, Help: "One sidecar per entry with overrides (schema v1)."},
					{Value: layoutDir, Help: "One aggregate metadata map per directory (schema v2)."},
				},
			},
			{
				Name:    "on_corrupt",
				Help:    "How to treat an unreadable directory map: 'error' fails reads and mutations for that directory (fail closed), 'warn' logs it and serves default attributes while still refusing to overwrite it.",
				Default: onCorruptError,
				Examples: []fs.OptionExample{
					{Value: onCorruptError, Help: "Fail reads and mutations for the affected directory."},
					{Value: onCorruptWarn, Help: "Log and use default attributes, never overwriting the map."},
				},
			},
			{
				Name:    "map_max_entries",
				Help:    "Maximum number of entries accepted in one directory map (sidecar_layout=dir; 0 = unlimited). A larger map is treated as corrupt.",
				Default: defaultMapMaxEntries,
			},
			{
				Name:    "map_max_bytes",
				Help:    "Maximum accepted size of one directory map (sidecar_layout=dir; 0 = unlimited). A larger map is treated as corrupt.",
				Default: fs.SizeSuffix(defaultMapMaxBytes),
			},
			{
				Name:    "hide_sidecars",
				Help:    "Hide sidecar files from directory listings.",
				Default: true,
			},
			{
				Name:    "proxy_links",
				Help:    "Store `<name>.rclonelink` symlinks in the `symlink` attribute of the entry's single sidecar instead of uploading a payload object, and emulate the `.rclonelink` entry from that sidecar (use together with --vfs-links).",
				Default: false,
			},
			{
				Name:    "cache_ttl",
				Help:    "In-memory TTL for cached sidecar metadata entries.",
				Default: fs.Duration(5 * time.Minute),
			},
			{
				Name:    "cache_max_entries",
				Help:    "Maximum number of cached sidecar entries, evicting least recently used first (0 = unlimited).",
				Default: 65536,
			},
			{
				Name:    "cache_max_size",
				Help:    "Approximate maximum memory used by the sidecar cache, evicting least recently used first (0 = unlimited).",
				Default: fs.SizeSuffix(64 << 20),
			},
			{
				Name:    "writeback_delay",
				Help:    "Debounce delay before flushing coalesced sidecar metadata changes to the wrapped remote.",
				Default: fs.Duration(500 * time.Millisecond),
			},
			{
				Name:    "writeback_workers",
				Help:    "Number of concurrent sidecar writeback workers (0 persists synchronously in the calling goroutine).",
				Default: 4,
			},
			{
				Name:    "writeback_retry_max",
				Help:    "Maximum backoff between retries of a failed sidecar write; updates are retried for the lifetime of the process.",
				Default: fs.Duration(5 * time.Minute),
			},
			{
				Name:    "pending_max_entries",
				Help:    "Maximum number of debounced sidecar updates kept in memory; beyond this updates are persisted synchronously so nothing is lost (0 = unlimited).",
				Default: 65536,
			},
			{
				Name:    "prefetch_workers",
				Help:    "Number of concurrent sidecar prefetches issued while listing a directory (0 disables prefetching; sidecar absence is always cached for free).",
				Default: 8,
			},
			{
				Name:    "overwrite_metadata",
				Help:    "How to treat existing sidecar metadata when an object is overwritten: 'preserve' keeps the mode/uid/gid of the previous object (safe for VFS writeback), 'reset' drops it unless the upload carries POSIX metadata.",
				Default: overwriteMetadataPreserve,
				Examples: []fs.OptionExample{
					{Value: overwriteMetadataPreserve, Help: "Keep the previous object's metadata across overwrites."},
					{Value: overwriteMetadataReset, Help: "Drop stale metadata unless the upload carries its own."},
				},
			},
		},
	})
}

// Options defines the configuration fields for the posixmeta backend.
type Options struct {
	Remote             string        `config:"remote"`
	DefaultFileMode    string        `config:"default_file_mode"`
	DefaultDirMode     string        `config:"default_dir_mode"`
	DefaultUID         int           `config:"default_uid"`
	DefaultGID         int           `config:"default_gid"`
	InheritParentOwner bool          `config:"inherit_parent_owner"`
	SidecarPrefix      string        `config:"sidecar_prefix"`
	SidecarLayout      string        `config:"sidecar_layout"`
	OnCorrupt          string        `config:"on_corrupt"`
	MapMaxEntries      int           `config:"map_max_entries"`
	MapMaxBytes        fs.SizeSuffix `config:"map_max_bytes"`
	HideSidecars       bool          `config:"hide_sidecars"`
	ProxyLinks         bool          `config:"proxy_links"`
	CacheTTL           fs.Duration   `config:"cache_ttl"`
	CacheMaxEntries    int           `config:"cache_max_entries"`
	CacheMaxSize       fs.SizeSuffix `config:"cache_max_size"`
	WritebackDelay     fs.Duration   `config:"writeback_delay"`
	WritebackWorkers   int           `config:"writeback_workers"`
	WritebackRetryMax  fs.Duration   `config:"writeback_retry_max"`
	PendingMaxEntries  int           `config:"pending_max_entries"`
	PrefetchWorkers    int           `config:"prefetch_workers"`
	OverwriteMetadata  string        `config:"overwrite_metadata"`
}

// maxSidecarSize bounds how much of a sidecar object is read. A real sidecar is
// well under 1 KiB, so anything larger is treated as corrupt instead of being
// loaded into memory.
const maxSidecarSize = 1 << 20

// writebackTimeout bounds a single sidecar writeback attempt.
const writebackTimeout = 60 * time.Second

// sidecarData represents the sparse JSON payload stored in the entry's
// `<dir>/.posixmeta.<entry>` sidecar.
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
	mode     os.FileMode
	hasMode  bool
	uid      uint32
	hasUID   bool
	gid      uint32
	hasGID   bool
	symlink  string
	btime    time.Time
	hasBTime bool
}

// cacheEntry is one cached sidecar resolution. Entries are kept in LRU order by
// Fs.cacheLRU so the cache can be bounded without periodic scans.
type cacheEntry struct {
	data      *sidecarData // nil means confirmed absent on wrapped remote
	modTime   time.Time    // modification time of the sidecar object itself
	expiresAt time.Time
	size      int64         // approximate memory cost used for cache_max_size
	elem      *list.Element // list item value is the absRemote cache key
}

// cacheStats counts cache effectiveness for the `cachestats` backend command.
type cacheStats struct {
	hits      uint64
	misses    uint64
	evictions uint64
}

// cacheEntryOverhead approximates the fixed per-entry cost (struct, map bucket,
// list element and string headers) that cache_max_size must account for.
const cacheEntryOverhead = 96

// pendingWrite is one debounced sidecar update. The timer only enqueues the
// path for the worker pool; the entry itself is the source of truth and is kept
// until a write succeeds, so transient remote failures are retried instead of
// silently dropped.
type pendingWrite struct {
	data  *sidecarData // nil means delete sidecar
	gen   uint64
	due   time.Time
	fails int
	timer *time.Timer
}

// inflightRead deduplicates concurrent sidecar reads of the same path.
type inflightRead struct {
	done    chan struct{}
	sc      *sidecarData
	modTime time.Time
	err     error
}

// writebackStats counts sidecar writeback activity for the cachestats command.
type writebackStats struct {
	written  uint64
	failed   uint64
	retried  uint64
	overflow uint64 // debounced updates persisted synchronously because pending was full
	lastErr  error
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

	mu         sync.Mutex
	cache      map[string]*cacheEntry
	cacheLRU   *list.List // least recently used at the front
	cacheBytes int64      // sum of cacheEntry.size over cache
	cacheStat  cacheStats
	reads      map[string]*inflightRead

	// dir layout: cache and writeback state of the per-directory maps
	dirMode        bool
	maps           map[string]*mapState
	mapsLRU        *list.List // least recently used at the front
	mapBytes       int64
	mapLoads       map[string]*mapLoad
	rootMapEnsured bool

	pending   map[string]*pendingWrite // absRemote -> latest debounced update
	inflight  map[string]uint64        // absRemote -> generation being persisted
	writeGen  uint64                   // global update sequence
	writeStat writebackStats
	ready     chan string // scheduler -> worker handoff
	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	stopped   bool
	workers   sync.WaitGroup
	notify    chan struct{} // closed and replaced whenever a writeback finishes
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
	if opt.SidecarPrefix == "" {
		opt.SidecarPrefix = defaultSidecarPrefix
	}
	if strings.Contains(opt.SidecarPrefix, "/") {
		return nil, errors.New("posixmeta: 'sidecar_prefix' must not contain '/'")
	}
	if legacySuffix, ok := m.Get("sidecar_suffix"); ok && legacySuffix != "" {
		fs.Logf(nil, "posixmeta: the sidecar_suffix option was removed; sidecars are now '<prefix>.<name>' (sidecar_prefix=%q) and existing '<name>.posixmeta' files are no longer recognised", opt.SidecarPrefix)
	}
	switch opt.OverwriteMetadata {
	case "":
		opt.OverwriteMetadata = overwriteMetadataPreserve
	case overwriteMetadataPreserve, overwriteMetadataReset:
	default:
		return nil, fmt.Errorf("posixmeta: invalid overwrite_metadata %q (want %q or %q)", opt.OverwriteMetadata, overwriteMetadataPreserve, overwriteMetadataReset)
	}
	switch opt.SidecarLayout {
	case "":
		opt.SidecarLayout = layoutSparse
	case layoutSparse, layoutDir:
	default:
		return nil, fmt.Errorf("posixmeta: invalid sidecar_layout %q (want %q or %q)", opt.SidecarLayout, layoutSparse, layoutDir)
	}
	switch opt.OnCorrupt {
	case "":
		opt.OnCorrupt = onCorruptError
	case onCorruptError, onCorruptWarn:
	default:
		return nil, fmt.Errorf("posixmeta: invalid on_corrupt %q (want %q or %q)", opt.OnCorrupt, onCorruptError, onCorruptWarn)
	}
	if opt.MapMaxEntries <= 0 {
		opt.MapMaxEntries = defaultMapMaxEntries
	}
	if opt.MapMaxBytes <= 0 {
		opt.MapMaxBytes = defaultMapMaxBytes
	}
	if opt.SidecarLayout != layoutDir {
		if _, ok := m.Get("map_max_entries"); ok {
			fs.Debugf(nil, "posixmeta: map_max_entries/map_max_bytes only apply to sidecar_layout=dir")
		}
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
	// A proxied symlink has no payload, so opening its exact path normally
	// fails with ErrorObjectNotFound; the probe below decides whether it is a
	// link instead.
	probeLinkRoot := err == nil || errors.Is(err, fs.ErrorObjectNotFound)
	if err != nil && !errors.Is(err, fs.ErrorIsFile) && !probeLinkRoot {
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

	// Mirror the same file-root behaviour for proxied symlinks: they have no
	// payload object, so the wrapped backend cannot report fs.ErrorIsFile for
	// them. Probe the sidecar and re-root the wrapped remote at the parent.
	linkFile := false
	if !errors.Is(err, fs.ErrorIsFile) && probeLinkRoot && opt.ProxyLinks && root != "" && isLinkRemote(root) {
		probeFs, probeErr := fs.NewFs(ctx, opt.Remote)
		if probeErr != nil {
			return nil, fmt.Errorf("posixmeta: failed to open sidecar base remote %q: %w", opt.Remote, probeErr)
		}
		probeTarget := strings.TrimSuffix(root, fs.LinkSuffix)
		var sc *sidecarData
		var scErr error
		if opt.SidecarLayout == layoutDir {
			sc, _, scErr = readMapEntryRaw(ctx, probeFs, opt.SidecarPrefix, probeTarget)
		} else {
			sc, _, scErr = readSidecarObject(ctx, probeFs, sidecarPathFor(opt.SidecarPrefix, probeTarget))
		}
		if scErr == nil && sc != nil && sc.Symlink != "" {
			parent := parentRemote(root)
			parentRoot := opt.Remote
			if parent != "" {
				parentRoot = joinRootRemote(opt.Remote, parent)
			}
			wrappedFs, err = fs.NewFs(ctx, parentRoot)
			if err != nil && !errors.Is(err, fs.ErrorIsFile) {
				return nil, fmt.Errorf("posixmeta: failed to open wrapped remote %q: %w", parentRoot, err)
			}
			actualRoot = parent
			linkFile = true
		}
	}
	if !linkFile && !errors.Is(err, fs.ErrorIsFile) && err != nil {
		return nil, fmt.Errorf("posixmeta: failed to open wrapped remote %q: %w", wrappedRoot, err)
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
		cache:           make(map[string]*cacheEntry),
		cacheLRU:        list.New(),
		reads:           make(map[string]*inflightRead),
		pending:         make(map[string]*pendingWrite),
		inflight:        make(map[string]uint64),
		ready:           make(chan string, max(4, 4*opt.WritebackWorkers)),
		stopCh:          make(chan struct{}),
		notify:          make(chan struct{}),
		dirMode:         opt.SidecarLayout == layoutDir,
		maps:            make(map[string]*mapState),
		mapsLRU:         list.New(),
		mapLoads:        make(map[string]*mapLoad),
	}

	// When rooted at a subdirectory, sidecars for the root entry itself and for
	// its ancestors live outside the wrapped root. Open the wrapped remote at
	// its base so those sidecars can still be read, written, moved and removed.
	if actualRoot != "" && opt.Remote != wrappedRoot {
		baseFs, baseErr := fs.NewFs(ctx, opt.Remote)
		if baseErr != nil {
			return nil, fmt.Errorf("posixmeta: failed to open sidecar base remote %q: %w", opt.Remote, baseErr)
		}
		f.base = baseFs
	}

	// Refuse to operate on a remote written by the other layout: their metadata
	// is not interoperable and silently ignoring it would show wrong ownership.
	if layoutErr := f.detectLayout(ctx); layoutErr != nil {
		return nil, layoutErr
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
		ReadDirMetadata:          true,
		WriteDirMetadata:         true,
		UserDirMetadata:          true,
		SlowModTime:              wrappedFs.Features().SlowModTime,
		NoMultiThreading:         wrappedFs.Features().NoMultiThreading,
		DirModTimeUpdatesOnWrite: wrappedFs.Features().DirModTimeUpdatesOnWrite,
	}).Fill(ctx, f)

	// Disable ServerSideAcrossConfigs unless wrapped supports it cleanly with sidecar awareness.
	f.features.ServerSideAcrossConfigs = false

	// Only advertise optional capabilities that the wrapped backend actually
	// has, so callers fall back to their generic implementations instead of
	// discovering at call time that we cannot help.
	wrappedFeatures := wrappedFs.Features()
	if wrappedFeatures.Copy == nil {
		f.features.Copy = nil
	}
	if wrappedFeatures.ChangeNotify == nil {
		f.features.ChangeNotify = nil
	}
	if wrappedFeatures.About == nil {
		f.features.About = nil
	}

	// Misconfiguration that silently degrades metadata ownership or unbounded
	// memory use is worth telling the operator about up front.
	if opt.CacheTTL <= 0 {
		fs.Debugf(f, "cache_ttl is <= 0: sidecar metadata will not be cached, so every stat costs a remote round-trip per entry and ancestor")
	}
	if opt.CacheMaxEntries <= 0 || opt.CacheMaxSize <= 0 {
		fs.Logf(f, "cache_max_entries=%d cache_max_size=%d: the sidecar cache is unbounded", opt.CacheMaxEntries, int64(opt.CacheMaxSize))
	}
	if opt.WritebackWorkers <= 0 {
		fs.Debugf(f, "writeback_workers=0: sidecar updates are persisted synchronously")
	}

	f.registerExitFlush()

	if linkFile {
		return f, fs.ErrorIsFile
	}
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

// RequiresMetadataPush reports that mounting this backend without
// --metadata-push would silently hide the stored mode/uid/gid and drop
// chmod/chown requests, so the mount command rejects that combination.
// The flag is irrelevant for non-mount uses (copy, lsjson, setmeta, ...).
func (f *Fs) RequiresMetadataPush() bool { return true }

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

// sidecarPathFor returns the sidecar path for a remote relative to the wrapped
// base root. Sidecars live in the same directory as their entry and carry the
// entry name after the prefix, e.g. "dir/name" -> "dir/.posixmeta.name".
//
// The base root entry ("") is the one exception: it is a directory with no name
// inside its own tree, so its sidecar is the bare prefix stored *inside* that
// directory ("<base>/.posixmeta"). Its parent is outside the configured remote
// (for pcloud/crypt roots it may be the account root), so writing
// "<parent>/.posixmeta.<name>" there would escape the configured remote and a
// `purge` of the root could not clean it up. Any other root keeps the normal
// form: an Fs rooted at "dir" stores dir's attributes in "<base>/.posixmeta.dir".
// The bare prefix stays unambiguous because sidecarTarget only maps
// "<prefix>.<name>" back to an entry, so listings never synthesise a visible
// entry for it.
func sidecarPathFor(prefix, absRemote string) string {
	clean := strings.Trim(strings.TrimSuffix(absRemote, "/"), "/")
	if clean == "" {
		return prefix
	}
	dir, base := path.Split(clean)
	return dir + prefix + "." + base
}

func (f *Fs) sidecarRemote(remote string) string {
	return sidecarPathFor(f.opt.SidecarPrefix, remote)
}

// isSidecar reports whether remote names a sidecar rather than a user entry.
func (f *Fs) isSidecar(remote string) bool {
	base := path.Base(strings.TrimSuffix(remote, "/"))
	return base == f.opt.SidecarPrefix || strings.HasPrefix(base, f.opt.SidecarPrefix+".")
}

// sidecarTarget returns the entry a sidecar belongs to. Sidecars and their
// entries share a directory, so this is a purely local mapping. ok is false for
// non-sidecars and for the base root's own sidecar, which has no entry name.
func (f *Fs) sidecarTarget(remote string) (target string, ok bool) {
	if f.dirMode {
		return "", false // the dir layout has no per-entry sidecars
	}
	clean := strings.Trim(strings.TrimSuffix(remote, "/"), "/")
	base := path.Base(clean)
	prefix := f.opt.SidecarPrefix + "."
	if !strings.HasPrefix(base, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(base, prefix)
	if name == "" {
		return "", false
	}
	dir := path.Dir(clean)
	if dir == "." {
		return name, true
	}
	return dir + "/" + name, true
}

// isLinkRemote reports whether remote is a rclone link object name.
func isLinkRemote(remote string) bool {
	return strings.HasSuffix(remote, fs.LinkSuffix)
}

// List lists objects and directories, hiding sidecar files and wrapping returned entries.
//
// The raw listing already contains the sibling sidecars, so it is used to prime
// the sidecar cache: entries without a sidecar are remembered as "absent" for
// free and the sidecars that do exist are fetched concurrently while the listing
// is being wrapped. That removes most of the per-entry remote round-trips that a
// stat on a large directory would otherwise cost.
//
// With proxy_links the same listing is enough to materialise symlinks: a sidecar
// whose entry has no payload but carries a `symlink` attribute is published as
// the `<name>.rclonelink` object the VFS expects.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	if f.dirMode {
		return f.listMap(ctx, dir)
	}
	entries, err := f.wrapped.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := f.checkDirEvidence(entries); err != nil {
		return nil, err
	}

	present := make(map[string]struct{}, len(entries))
	sidecarFor := make(map[string]string, len(entries))
	for _, entry := range entries {
		remote := entry.Remote()
		if f.isSidecar(remote) {
			if target, ok := f.sidecarTarget(remote); ok {
				sidecarFor[target] = remote
			}
			continue
		}
		present[remote] = struct{}{}
	}

	var links map[string]sidecarLink
	if f.opt.ProxyLinks {
		candidates := make(map[string]struct{}, len(sidecarFor))
		for target := range sidecarFor {
			if _, ok := present[target]; !ok {
				candidates[target] = struct{}{} // no payload: may be a proxied link
			} else if _, ok := present[target+fs.LinkSuffix]; ok {
				candidates[target] = struct{}{} // sidecar may shadow a real link payload
			}
		}
		links = f.resolveLinkCandidates(ctx, candidates)
	}

	out := make(fs.DirEntries, 0, len(entries)+len(links))
	var prefetch []string
	for _, entry := range entries {
		remote := entry.Remote()
		if f.isSidecar(remote) {
			if f.opt.HideSidecars {
				continue
			}
		} else {
			absRemote := f.absRemote(remote)
			if _, ok := sidecarFor[remote]; ok {
				prefetch = append(prefetch, absRemote)
			} else {
				f.primeNegative(absRemote)
			}
			if isLinkRemote(remote) {
				if _, shadowed := links[strings.TrimSuffix(remote, fs.LinkSuffix)]; shadowed {
					// A proxied symlink takes precedence over a stale payload.
					continue
				}
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
	if len(links) > 0 {
		targets := make([]string, 0, len(links))
		for target := range links {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		for _, target := range targets {
			out = append(out, f.newLinkObjectFromSidecar(links[target]))
		}
	}
	f.prefetchSidecars(ctx, prefetch)
	return out, nil
}

// ListR lists dir recursively, hiding sidecars and wrapping the returned
// entries. It delegates to the wrapped backend's ListR so recursive walks (copy,
// check, size, ncdu) keep the wrapped backend's bulk-listing performance.
//
// Recursive batches can split a directory, so sidecar presence is resolved from
// the cache (warmed by List and by the sidecars in the batch) instead of from
// "no sibling sidecar in this batch".
func (f *Fs) ListR(ctx context.Context, dir string, callback fs.ListRCallback) error {
	if f.dirMode {
		return f.listRMap(ctx, dir, callback)
	}
	seenLinks := make(map[string]struct{})
	wrapped := func(entries fs.DirEntries) error {
		if err := f.checkDirEvidence(entries); err != nil {
			return err
		}
		var links map[string]sidecarLink
		if f.opt.ProxyLinks {
			candidates := make(map[string]struct{}, len(entries))
			for _, entry := range entries {
				remote := entry.Remote()
				if f.isSidecar(remote) {
					if target, ok := f.sidecarTarget(remote); ok {
						candidates[target] = struct{}{}
					}
					continue
				}
				if isLinkRemote(remote) {
					if target := strings.TrimSuffix(remote, fs.LinkSuffix); target != "" {
						candidates[target] = struct{}{}
					}
				}
			}
			links = f.resolveLinkCandidates(ctx, candidates)
		}

		out := make(fs.DirEntries, 0, len(entries)+len(links))
		for _, entry := range entries {
			remote := entry.Remote()
			if f.isSidecar(remote) {
				if f.opt.HideSidecars {
					continue
				}
			} else if isLinkRemote(remote) {
				if _, shadowed := links[strings.TrimSuffix(remote, fs.LinkSuffix)]; shadowed {
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
		if len(links) > 0 {
			targets := make([]string, 0, len(links))
			for target := range links {
				targets = append(targets, target)
			}
			sort.Strings(targets)
			for _, target := range targets {
				link := links[target]
				if _, done := seenLinks[link.remote]; done {
					continue
				}
				seenLinks[link.remote] = struct{}{}
				out = append(out, f.newLinkObjectFromSidecar(link))
			}
		}
		if len(out) == 0 {
			return nil
		}
		return callback(out)
	}

	if do := f.wrapped.Features().ListR; do != nil {
		return do(ctx, dir, wrapped)
	}
	return f.listRecurse(ctx, dir, wrapped)
}

// listRecurse is the fallback used when the wrapped backend has no ListR.
func (f *Fs) listRecurse(ctx context.Context, dir string, callback func(fs.DirEntries) error) error {
	entries, err := f.wrapped.List(ctx, dir)
	if err != nil {
		return err
	}
	batch := make(fs.DirEntries, 0, len(entries))
	var subdirs []string
	for _, entry := range entries {
		if d, ok := entry.(fs.Directory); ok {
			subdirs = append(subdirs, d.Remote())
		}
		batch = append(batch, entry)
	}
	if len(batch) > 0 {
		if err := callback(batch); err != nil {
			return err
		}
	}
	for _, sub := range subdirs {
		if err := f.listRecurse(ctx, sub, callback); err != nil {
			return err
		}
	}
	return nil
}

// ChangeNotify forwards change notifications from the wrapped remote and
// invalidates the affected sidecar cache entries so externally modified
// metadata is re-read promptly.
func (f *Fs) ChangeNotify(ctx context.Context, notifyFunc func(string, fs.EntryType), pollIntervalChan <-chan time.Duration) {
	do := f.wrapped.Features().ChangeNotify
	if do == nil {
		fs.Infof(f, "wrapped remote does not support change notification; relying on --dir-cache-time and cache_ttl")
		return
	}
	do(ctx, func(remote string, entryType fs.EntryType) {
		absRemote := f.absRemote(strings.TrimSuffix(remote, "/"))
		f.invalidateCacheAbs(absRemote)
		// A directory sidecar change also changes the inherited owner of
		// everything below it.
		f.invalidateSubtree(f.parentAbs(absRemote))
		notifyFunc(remote, entryType)
	}, pollIntervalChan)
}

// DirCacheFlush asks the wrapped remote to flush its directory cache.
func (f *Fs) DirCacheFlush() {
	if do := f.wrapped.Features().DirCacheFlush; do != nil {
		do()
	}
}

// About returns the usage of the wrapped remote.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	if do := f.wrapped.Features().About; do != nil {
		return do(ctx)
	}
	return nil, fs.ErrorNotImplemented
}

// parentAbs returns the absolute parent path of absRemote ("" at the base root).
func (f *Fs) parentAbs(absRemote string) string {
	return parentRemote(absRemote)
}

// NewObject finds the object at remote and wraps it with POSIX metadata support.
//
// With proxy_links a `<name>.rclonelink` remote is served from the sidecar that
// stores the symlink target; a real payload of that name (legacy links, or a
// file that happens to end in .rclonelink) is only used when no such sidecar
// exists.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if f.opt.HideSidecars && f.isSidecar(remote) {
		return nil, fs.ErrorObjectNotFound
	}
	if f.opt.ProxyLinks && isLinkRemote(remote) {
		obj, err := f.newLinkObject(ctx, remote)
		if err != nil || obj != nil {
			return obj, err
		}
	}
	obj, err := f.wrapped.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	return f.wrapObject(obj), nil
}

// Put uploads a new file and writes a sparse sidecar only if incoming metadata differs from defaults.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.putObject(ctx, in, src, options, false)
}

// PutUnchecked is Put without duplicate checking, so same-remote copies keep
// using the wrapped backend's unchecked upload path.
func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.putObject(ctx, in, src, options, true)
}

func (f *Fs) putObject(ctx context.Context, in io.Reader, src fs.ObjectInfo, options []fs.OpenOption, unchecked bool) (fs.Object, error) {
	remote := src.Remote()
	if f.opt.HideSidecars && f.isSidecar(remote) {
		return nil, fmt.Errorf("posixmeta: cannot write directly to sidecar path %q", remote)
	}
	// A symlink is stored in its sidecar, not as a payload object.
	if f.opt.ProxyLinks && isLinkRemote(remote) {
		return f.putLink(ctx, in, src, options, remote)
	}

	var (
		wrappedObj fs.Object
		err        error
	)
	if do := f.wrapped.Features().PutUnchecked; unchecked && do != nil {
		wrappedObj, err = do(ctx, in, src, options...)
	} else {
		wrappedObj, err = f.wrapped.Put(ctx, in, src, options...)
	}
	if err != nil {
		return nil, err
	}

	po := f.wrapObject(wrappedObj)
	if err := f.afterPut(ctx, remote, src, options); err != nil {
		// The payload is safely uploaded; report the metadata problem without
		// failing the transfer, but make it visible.
		fs.CountError(ctx, err)
		fs.Errorf(f, "posixmeta: metadata not applied to %q: %v", remote, err)
	}
	return po, nil
}

// afterPut reconciles the sidecar with a freshly uploaded object.
//
// With overwrite_metadata=preserve (the default) the previous object's sidecar
// is kept unless the incoming write carries POSIX metadata, because the VFS
// writeback path re-uploads a file without repeating its mode/owner and losing
// them would be worse. With overwrite_metadata=reset a stale sidecar is dropped
// so that `rclone copy` cannot apply the previous object's mode/owner to new
// content.
func (f *Fs) afterPut(ctx context.Context, remote string, src fs.ObjectInfo, options []fs.OpenOption) error {
	meta, err := fs.GetMetadataOptions(ctx, f.wrapped, src, options)
	if err != nil {
		fs.Debugf(f, "posixmeta: cannot read incoming metadata for %q: %v", remote, err)
		meta = nil
	}

	hasPOSIX := hasPOSIXMetadata(meta)
	if !hasPOSIX && f.opt.OverwriteMetadata != overwriteMetadataReset {
		return nil
	}

	// A fresh upload is a new object: resolve from defaults and apply only the
	// attributes carried by the write.
	fresh := resolvedMeta{
		mode: f.defaultFileMode,
		uid:  f.defaultUID,
		gid:  f.defaultGID,
	}
	touched, _, err := applyPOSIXAttrs(&fresh, meta, true)
	if err != nil {
		return err
	}
	var sc *sidecarData
	if touched {
		sc = f.buildSparseSidecar(ctx, remote, false, fresh)
	}
	return f.scheduleSidecarUpdate(ctx, remote, sc)
}

// Mkdir creates a directory on the wrapped remote.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if err := f.wrapped.Mkdir(ctx, dir); err != nil {
		return err
	}
	// Any cached sidecar resolution for a previously existing entry is stale.
	f.quiesceAbs(ctx, f.absRemote(dir))
	f.invalidateCache(dir)
	return nil
}

// MkdirMetadata creates a directory and stores its POSIX metadata in a sidecar.
// It is what makes `rclone copy --metadata` (and sync) preserve directory
// mode/uid/gid instead of dropping them.
func (f *Fs) MkdirMetadata(ctx context.Context, dir string, metadata fs.Metadata) (fs.Directory, error) {
	posixMeta, passthrough := splitPOSIXMetadata(metadata)

	var wrappedDir fs.Directory
	if do := f.wrapped.Features().MkdirMetadata; do != nil {
		d, err := do(ctx, dir, passthrough)
		if err != nil {
			return nil, err
		}
		wrappedDir = d
	} else {
		if err := f.Mkdir(ctx, dir); err != nil {
			return nil, err
		}
		wrappedDir = &staticDir{remote: dir, when: time.Now().UTC(), fsInfo: f}
	}

	pd := &posixDirectory{Directory: wrappedDir, f: f}
	if len(posixMeta) > 0 {
		if err := pd.SetMetadata(ctx, posixMeta); err != nil {
			return nil, err
		}
	}
	return pd, nil
}

// Copy copies src to remote on the wrapped remote, keeping the sidecar in sync.
// It preserves the wrapped backend's server-side copy instead of degrading to a
// download plus re-upload.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	if lo, ok := src.(*posixLinkObject); ok && f.opt.ProxyLinks {
		return f.copyLink(ctx, lo, remote)
	}

	doCopy := f.wrapped.Features().Copy
	if doCopy == nil {
		return nil, fs.ErrorCantCopy
	}

	// Sidecars are addressed relative to the wrapped base root, so take the
	// source's base root when it belongs to another posixmeta instance.
	srcBase, srcRoot := f.base, f.root
	if po, ok := src.(*posixObject); ok {
		srcBase, srcRoot = po.f.base, po.f.root
		if po.f == f {
			if err := f.flushPending(ctx, src.Remote()); err != nil {
				return nil, err
			}
		}
	}

	copied, err := doCopy(ctx, unwrapObject(src), remote)
	if err != nil {
		return nil, err
	}

	srcAbs := joinRootRemote(srcRoot, src.Remote())
	dstAbs := f.absRemote(remote)
	// A debounced update for the destination describes the object we just
	// replaced, so it must not be written on top of the copied metadata.
	f.quiesceAbs(ctx, dstAbs)

	var sc *sidecarData
	var modTime time.Time
	if po, ok := src.(*posixObject); ok {
		sc, modTime, err = po.f.getSidecarInfoAbs(ctx, joinRootRemote(po.f.root, src.Remote()))
	} else {
		sc, modTime, err = readSidecarObject(ctx, srcBase, f.sidecarRemote(srcAbs))
	}
	if err != nil {
		return nil, err
	}
	if sc != nil {
		if err := f.persistSidecarAbs(ctx, dstAbs, sc); err != nil {
			return nil, fmt.Errorf("posixmeta: copy sidecar to %q: %w", remote, err)
		}
		f.putCacheAt(dstAbs, sc, modTime)
	} else if err := f.deleteSidecarAbsImmediate(ctx, dstAbs); err != nil {
		fs.Debugf(f, "posixmeta: cannot remove stale sidecar for %q: %v", remote, err)
	}
	return f.wrapObject(copied), nil
}

// Rmdir removes an empty directory and cleans up its directory sidecar.
//
// Sidecars are hidden from listings, so a directory whose only remaining remote
// objects are orphaned sidecars would otherwise fail with an opaque "directory
// not empty" that `rclone ls` cannot explain. Those leftovers are removed and
// the rmdir is retried once.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if f.dirMode {
		return f.rmdirMap(ctx, dir)
	}
	f.quiesceAbs(ctx, f.absRemote(dir))

	err := f.wrapped.Rmdir(ctx, dir)
	if err != nil {
		if removed, oerr := f.removeOrphanSidecars(ctx, dir); oerr == nil && removed > 0 {
			if rerr := f.wrapped.Rmdir(ctx, dir); rerr == nil {
				err = nil
			}
		}
	}
	if err != nil {
		return err
	}

	f.invalidatePrefix(dir)
	if derr := f.deleteSidecarImmediate(ctx, dir); derr != nil {
		fs.Debugf(f, "posixmeta: cannot remove directory sidecar for %q: %v", dir, derr)
	}
	return nil
}

// removeOrphanSidecars deletes sidecars in dir whose payload no longer exists.
// A sidecar that stores a proxied symlink is the entry's only storage, so it is
// never treated as an orphan.
func (f *Fs) removeOrphanSidecars(ctx context.Context, dir string) (removed int, err error) {
	entries, err := f.wrapped.List(ctx, dir)
	if err != nil {
		return 0, err
	}
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		names[entry.Remote()] = struct{}{}
	}
	for _, entry := range entries {
		remote := entry.Remote()
		target, ok := f.sidecarTarget(remote)
		if !ok {
			continue
		}
		if _, ok := names[target]; ok {
			continue // the entry itself still exists, keep its metadata
		}
		if f.opt.ProxyLinks {
			sc, err := f.getSidecar(ctx, target)
			if err != nil {
				fs.Debugf(f, "posixmeta: cannot check orphan sidecar %q: %v", remote, err)
				continue
			}
			if sc != nil && sc.Symlink != "" {
				continue // the sidecar is the proxied symlink's payload
			}
		}
		obj, ok := entry.(fs.Object)
		if !ok {
			continue
		}
		if rerr := obj.Remove(ctx); rerr != nil {
			fs.CountError(ctx, rerr)
			fs.Debugf(f, "posixmeta: cannot remove orphan sidecar %q: %v", remote, rerr)
			continue
		}
		f.invalidateCacheAbs(f.absRemote(target))
		removed++
	}
	return removed, nil
}

// Purge deletes a directory and all its contents (including sidecars).
func (f *Fs) Purge(ctx context.Context, dir string) error {
	absDir := f.absRemote(dir)
	f.quiescePrefix(ctx, dir)

	if do := f.wrapped.Features().Purge; do != nil {
		err := do(ctx, dir)
		if err == nil {
			f.invalidatePrefix(dir)
			if derr := f.deleteSidecarAbsImmediate(ctx, absDir); derr != nil {
				fs.Debugf(f, "posixmeta: cannot remove directory sidecar for %q: %v", dir, derr)
			}
			return nil
		}
		if !errors.Is(err, fs.ErrorCantPurge) {
			return err
		}
		// Fall through to the local purge, which also removes sidecars that
		// operations.Purge's generic fallback cannot see.
	}

	if err := f.purgeRecurse(ctx, dir); err != nil {
		return err
	}
	f.invalidatePrefix(dir)
	if derr := f.deleteSidecarAbsImmediate(ctx, absDir); derr != nil {
		fs.Debugf(f, "posixmeta: cannot remove directory sidecar for %q: %v", dir, derr)
	}
	return nil
}

// purgeRecurse removes every raw object (payloads and sidecars) below dir and
// then the directories themselves.
func (f *Fs) purgeRecurse(ctx context.Context, dir string) error {
	entries, err := f.wrapped.List(ctx, dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch x := entry.(type) {
		case fs.Directory:
			if err := f.purgeRecurse(ctx, x.Remote()); err != nil {
				return err
			}
		case fs.Object:
			if err := x.Remove(ctx); err != nil {
				return err
			}
		}
	}
	err = f.wrapped.Rmdir(ctx, dir)
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil
	}
	return err
}

// Move moves/renames an object along with its sidecar file if present.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	if lo, ok := src.(*posixLinkObject); ok && f.opt.ProxyLinks {
		return f.moveLink(ctx, lo, remote)
	}

	doMove := f.wrapped.Features().Move
	if doMove == nil {
		return nil, fs.ErrorCantMove
	}

	srcRemote := src.Remote()
	srcAbs := f.absRemote(srcRemote)
	dstAbs := f.absRemote(remote)

	// The source sidecar must be up to date before it is moved, and a pending
	// update for the destination describes the object being replaced.
	if err := f.flushPending(ctx, srcRemote); err != nil {
		return nil, err
	}
	// A queued or in-flight writeback for the destination describes the object
	// being replaced, so it must not run after the sidecar move below.
	f.quiesceAbs(ctx, dstAbs)

	movedRaw, err := doMove(ctx, unwrapObject(src), remote)
	if err != nil {
		return nil, err
	}

	srcSidecar := f.sidecarRemote(srcAbs)
	dstSidecar := f.sidecarRemote(dstAbs)
	movedSidecar, merr := f.moveSidecarAbs(ctx, srcAbs, dstAbs, srcSidecar, dstSidecar)
	if merr != nil {
		fs.CountError(ctx, merr)
		fs.Errorf(f, "posixmeta: failed to move sidecar %q -> %q: %v", srcSidecar, dstSidecar, merr)
	}
	if !movedSidecar {
		// Without source metadata the destination must not keep the metadata of
		// the object it just replaced.
		if derr := f.deleteSidecarAbsImmediate(ctx, dstAbs); derr != nil {
			fs.Debugf(f, "posixmeta: cannot remove stale sidecar for %q: %v", remote, derr)
		}
	}

	f.invalidateCacheAbs(srcAbs)
	f.invalidateCacheAbs(dstAbs)
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

	// Persist metadata for the tree before it moves, otherwise unflushed updates
	// would be written back to the now nonexistent source path.
	if err := f.flushPrefixPending(ctx, srcRemote); err != nil {
		return err
	}

	if err := doDirMove(ctx, wrappedSrc, srcRemote, dstRemote); err != nil {
		return err
	}

	// Move the directory's own metadata (`<parent>/.posixmeta.<dir>` or the key in
	// the parent directory map) if one exists. Paths are base-relative so a moved
	// root directory keeps its metadata, which lives just outside the wrapped
	// root.
	srcDirAbs := joinRootRemote(srcRoot, srcRemote)
	dstDirAbs := f.absRemote(dstRemote)
	if srcBase == f.base {
		if _, merr := f.moveSidecarAbs(ctx, srcDirAbs, dstDirAbs, f.sidecarRemote(srcDirAbs), f.sidecarRemote(dstDirAbs)); merr != nil {
			fs.CountError(ctx, merr)
			fs.Errorf(f, "posixmeta: failed to move dir sidecar %q -> %q: %v", srcDirAbs, dstDirAbs, merr)
		}
	} else if doMove := srcBase.Features().Move; doMove != nil {
		srcSidecar := f.sidecarRemote(srcDirAbs)
		dstSidecar := f.sidecarRemote(dstDirAbs)
		if scObj, err := srcBase.NewObject(ctx, srcSidecar); err == nil {
			if _, mvErr := doMove(ctx, scObj, dstSidecar); mvErr != nil {
				fs.CountError(ctx, mvErr)
				fs.Errorf(f, "posixmeta: failed to move dir sidecar %q -> %q: %v", srcSidecar, dstSidecar, mvErr)
			}
		}
	}

	f.quiescePrefix(ctx, srcRemote)
	f.quiescePrefix(ctx, dstRemote)
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
	flushErr := f.flushAllPending(ctx)
	if flushErr != nil {
		fs.CountError(ctx, flushErr)
	}
	if do := f.wrapped.Features().CleanUp; do != nil {
		if err := do(ctx); err != nil && flushErr == nil {
			return err
		}
	}
	return flushErr
}

// Shutdown flushes debounced sidecar updates and stops the writeback workers.
//
// rclone calls Shutdown when the Fs leaves the fs cache. Because a `rclone
// mount` process does not evict its Fs on unmount, the same flush is also
// registered with lib/atexit (see registerExitFlush), so a chmod immediately
// followed by an unmount is still persisted.
func (f *Fs) Shutdown(ctx context.Context) error {
	// Refuse new queue entries first: any update arriving during shutdown is
	// then persisted synchronously instead of landing in a queue that will never
	// be drained.
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()

	flushErr := f.flushAllPending(ctx)
	f.stopWriteback()
	unregisterExitFlush(f)
	if flushErr != nil {
		fs.CountError(ctx, flushErr)
	}
	return flushErr
}

// atexitFlushTimeout bounds the final flush performed while the process exits.
const atexitFlushTimeout = 30 * time.Second

var (
	liveFsMu     sync.Mutex
	liveFs       = map[*Fs]struct{}{}
	exitFlushOne sync.Once
)

// registerExitFlush arranges for this Fs to flush its debounced updates when the
// process exits, including on SIGINT/SIGTERM.
func (f *Fs) registerExitFlush() {
	exitFlushOne.Do(func() {
		atexit.Register(func() { flushLiveFs() })
	})
	liveFsMu.Lock()
	liveFs[f] = struct{}{}
	liveFsMu.Unlock()
}

// unregisterExitFlush stops the exit handler from flushing an Fs which has been
// shut down already.
func unregisterExitFlush(f *Fs) {
	liveFsMu.Lock()
	delete(liveFs, f)
	liveFsMu.Unlock()
}

// flushLiveFs flushes every live Fs that still has debounced sidecar updates.
func flushLiveFs() {
	liveFsMu.Lock()
	pending := make([]*Fs, 0, len(liveFs))
	for f := range liveFs {
		pending = append(pending, f)
	}
	liveFsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), atexitFlushTimeout)
	defer cancel()
	for _, f := range pending {
		f.mu.Lock()
		queued := len(f.pending)
		f.mu.Unlock()
		if queued == 0 && len(f.mapDirtyDirs()) == 0 {
			continue
		}
		if err := f.flushAllPending(ctx); err != nil {
			fs.Errorf(f, "posixmeta: could not flush %d sidecar update(s) on exit: %v", queued, err)
		}
	}
}

// Command allows calling backend-specific helpers such as `setmeta` for testing and tooling.
// Example: rclone backend setmeta pm_local: hello.txt -o mode=0750 -o uid=1001 -o gid=2002
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (any, error) {
	switch name {
	case "cachestats":
		return f.cacheStatsSnapshot(), nil
	case "repair":
		if !f.dirMode {
			return nil, fmt.Errorf("posixmeta: repair requires sidecar_layout=dir (this remote uses %q, whose sidecars are self-contained)", layoutSparse)
		}
		return f.repairCommand(ctx, arg, opt)
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
			type metadataObject interface {
				fs.Metadataer
				fs.SetMetadataer
			}
			mo, ok := obj.(metadataObject)
			if !ok {
				return nil, fmt.Errorf("posixmeta: object %q cannot store metadata", target)
			}
			if err := mo.SetMetadata(ctx, meta); err != nil {
				return nil, err
			}
			if flushErr := f.flushPending(ctx, target); flushErr != nil {
				return nil, flushErr
			}
			return mo.Metadata(ctx)
		}
		// A proxied symlink can be created (or edited) through setmeta when its
		// sidecar does not exist yet.
		if f.opt.ProxyLinks && isLinkRemote(target) {
			if linkTarget, ok := lookupMetadata(meta, "symlink"); ok && linkTarget != "" {
				if err := f.storeLink(ctx, target, linkTarget, meta); err != nil {
					return nil, err
				}
				obj, err := f.NewObject(ctx, target)
				if err != nil {
					return nil, err
				}
				mo, ok := obj.(fs.Metadataer)
				if !ok {
					return nil, fmt.Errorf("posixmeta: object %q cannot report metadata", target)
				}
				return mo.Metadata(ctx)
			}
		}
		// Fallback to directory if not an object.
		pd := &posixDirectory{Directory: &staticDir{remote: target, when: time.Now().UTC(), fsInfo: f}, f: f}
		if err := pd.SetMetadata(ctx, meta); err != nil {
			return nil, err
		}
		if flushErr := f.flushPending(ctx, target); flushErr != nil {
			return nil, flushErr
		}
		return pd.Metadata(ctx)
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
//
// Concurrent reads of the same path are coalesced so a burst of stat calls after
// a cache miss costs one remote read chain instead of one per caller.
func (f *Fs) getSidecarAbs(ctx context.Context, absRemote string) (*sidecarData, error) {
	sc, _, err := f.getSidecarInfoAbs(ctx, absRemote)
	return sc, err
}

// getSidecarInfoAbs is getSidecarAbs plus the modification time of the sidecar
// object, which is the only mtime available for entries that have no payload
// (proxied symlinks). Pending updates report the current time because their
// object does not exist yet.
func (f *Fs) getSidecarInfoAbs(ctx context.Context, absRemote string) (*sidecarData, time.Time, error) {
	if f.dirMode {
		return f.mapEntryInfo(ctx, absRemote)
	}
	now := time.Now()

	for {
		f.mu.Lock()
		if pw, ok := f.pending[absRemote]; ok {
			cp := cloneSidecar(pw.data)
			f.mu.Unlock()
			return cp, now, nil
		}
		if ce, ok := f.cache[absRemote]; ok {
			if now.Before(ce.expiresAt) {
				cp := cloneSidecar(ce.data)
				modTime := ce.modTime
				if ce.elem != nil {
					f.cacheLRU.MoveToBack(ce.elem)
				}
				f.cacheStat.hits++
				f.mu.Unlock()
				return cp, modTime, nil
			}
			// Drop expired entries eagerly so cache_max_entries / cache_max_size
			// track live entries instead of accumulating dead keys forever.
			f.removeCacheEntryLocked(absRemote)
		}
		if ir, ok := f.reads[absRemote]; ok {
			f.mu.Unlock()
			select {
			case <-ir.done:
				return cloneSidecar(ir.sc), ir.modTime, ir.err
			case <-ctx.Done():
				return nil, time.Time{}, ctx.Err()
			}
		}
		f.cacheStat.misses++
		ir := &inflightRead{done: make(chan struct{})}
		f.reads[absRemote] = ir
		f.mu.Unlock()

		ir.sc, ir.modTime, ir.err = f.readSidecarAbs(ctx, absRemote)

		f.mu.Lock()
		delete(f.reads, absRemote)
		f.mu.Unlock()
		close(ir.done)
		return cloneSidecar(ir.sc), ir.modTime, ir.err
	}
}

// readSidecarAbs reads and parses one sidecar from the wrapped base remote and
// primes the cache with the result (including negative results).
func (f *Fs) readSidecarAbs(ctx context.Context, absRemote string) (*sidecarData, time.Time, error) {
	sc, modTime, err := readSidecarObject(ctx, f.base, f.sidecarRemote(absRemote))
	if err != nil {
		return nil, time.Time{}, err
	}
	f.putCacheAt(absRemote, sc, modTime)
	return sc, modTime, nil
}

// readSidecarObject reads and parses the sidecar at scPath from base.
//
// It returns (nil, time.Time{}, nil) when the sidecar does not exist or is
// unusable (corrupt, truncated or written by a newer schema); those cases are
// logged and counted instead of making every stat on the entry fail. Otherwise
// it also returns the sidecar object's modification time.
func readSidecarObject(ctx context.Context, base fs.Fs, scPath string) (*sidecarData, time.Time, error) {
	scObj, err := base.NewObject(ctx, scPath)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, err
	}
	modTime := scObj.ModTime(ctx)

	rc, err := scObj.Open(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer func() { _ = rc.Close() }()

	// A real sidecar is well under 1 KiB; never load an arbitrarily large
	// object (which would be corrupt or hostile) into memory.
	raw, err := io.ReadAll(io.LimitReader(rc, maxSidecarSize+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	if int64(len(raw)) > maxSidecarSize {
		err = fmt.Errorf("sidecar %q is larger than %d bytes", scPath, maxSidecarSize)
		fs.CountError(ctx, err)
		fs.Errorf(nil, "posixmeta: %v; ignoring it", err)
		return nil, time.Time{}, nil
	}

	var parsed sidecarData
	if err := json.Unmarshal(raw, &parsed); err != nil {
		fs.CountError(ctx, err)
		fs.Errorf(nil, "posixmeta: corrupt sidecar JSON at %q: %v; ignoring it", scPath, err)
		return nil, time.Time{}, nil
	}
	if parsed.Version > sidecarSchemaVersion {
		err := fmt.Errorf("sidecar %q has schema version %d, newer than supported %d", scPath, parsed.Version, sidecarSchemaVersion)
		fs.CountError(ctx, err)
		fs.Errorf(nil, "posixmeta: %v; ignoring it", err)
		return nil, time.Time{}, nil
	}
	return &parsed, modTime, nil
}

// resolve computes the effective POSIX attributes for remote.
//
// The entry's own sidecar is read first and ancestors are only consulted for the
// ownership fields it does not override, so a fully specified sidecar costs one
// lookup instead of one per path component.
func (f *Fs) resolve(ctx context.Context, remote string, isDir bool) (resolvedMeta, error) {
	var res resolvedMeta
	if isDir {
		res.mode = f.defaultDirMode
	} else {
		res.mode = f.defaultFileMode
	}
	res.uid = f.defaultUID
	res.gid = f.defaultGID

	sc, err := f.getSidecar(ctx, remote)
	if err != nil {
		return res, err
	}
	if sc != nil {
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
	}

	if f.opt.InheritParentOwner && (!res.hasUID || !res.hasGID) {
		uid, gid := f.inheritedOwner(ctx, f.absRemote(remote))
		if !res.hasUID && uid != nil {
			res.uid = *uid
			res.hasUID = true
		}
		if !res.hasGID && gid != nil {
			res.gid = *gid
			res.hasGID = true
		}
	}
	return res, nil
}

// inheritedOwner returns the uid and gid inherited from the nearest ancestor
// directory sidecars of absRemote (nearest first). Missing values are nil.
//
// Read errors on ancestors are counted and ignored: inheritance is best-effort
// and must not make every stat on the entry fail.
func (f *Fs) inheritedOwner(ctx context.Context, absRemote string) (uid, gid *uint32) {
	for _, ancestor := range ancestorDirs(absRemote) {
		sc, err := f.getSidecarAbs(ctx, ancestor)
		if err != nil {
			fs.CountError(ctx, err)
			fs.Debugf(f, "posixmeta: cannot read ancestor sidecar %q: %v", ancestor, err)
			continue
		}
		if sc == nil {
			continue
		}
		if uid == nil && sc.UID != nil {
			uid = sc.UID
		}
		if gid == nil && sc.GID != nil {
			gid = sc.GID
		}
		if uid != nil && gid != nil {
			break
		}
	}
	return uid, gid
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
		if uid, gid := f.inheritedOwner(ctx, f.absRemote(remote)); uid != nil || gid != nil {
			if uid != nil {
				inheritedUID = *uid
			}
			if gid != nil {
				inheritedGID = *gid
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

// scheduleSidecarUpdate queues (or, when writeback_delay is zero, applies) a
// sidecar update. The cache is updated immediately so readers never observe
// stale metadata while the write is still debounced.
//
// The returned error is only non-nil for the synchronous paths; queued updates
// are retried in the background until they succeed, so they are never silently
// dropped.
func (f *Fs) scheduleSidecarUpdate(ctx context.Context, remote string, sc *sidecarData) error {
	if f.dirMode {
		return f.scheduleDirUpdate(ctx, remote, sc)
	}
	absRemote := f.absRemote(remote)
	f.putCache(absRemote, sc)

	delay := time.Duration(f.opt.WritebackDelay)
	if delay <= 0 || f.opt.WritebackWorkers <= 0 || f.isStopped() {
		f.quiesceAbs(ctx, absRemote)
		return f.persistSidecarAbs(ctx, absRemote, sc)
	}

	f.startWriteback()

	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		f.quiesceAbs(ctx, absRemote)
		return f.persistSidecarAbs(ctx, absRemote, sc)
	}
	// Keeping the debounce queue bounded matters on a mount that touches more
	// entries than expected: persist synchronously rather than grow without
	// limit. Nothing is lost, only coalesced less.
	if f.opt.PendingMaxEntries > 0 && len(f.pending) >= f.opt.PendingMaxEntries {
		if _, queued := f.pending[absRemote]; !queued {
			f.writeStat.overflow++
			f.mu.Unlock()
			f.quiesceAbs(ctx, absRemote)
			return f.persistSidecarAbs(ctx, absRemote, sc)
		}
	}

	f.writeGen++
	gen := f.writeGen
	pw, queued := f.pending[absRemote]
	if !queued {
		pw = &pendingWrite{}
		f.pending[absRemote] = pw
	}
	if pw.timer != nil {
		pw.timer.Stop()
	}
	pw.data = cloneSidecar(sc)
	pw.gen = gen
	pw.fails = 0
	pw.due = time.Now().Add(delay)
	pw.timer = time.AfterFunc(delay, func() { f.enqueueWrite(absRemote) })
	f.mu.Unlock()
	return nil
}

// startWriteback lazily starts the writeback worker pool.
func (f *Fs) startWriteback() {
	f.startOnce.Do(func() {
		for i := 0; i < f.opt.WritebackWorkers; i++ {
			f.workers.Add(1)
			go f.writebackWorker()
		}
	})
}

// stopWriteback stops the worker pool. Pending updates must be flushed first.
func (f *Fs) stopWriteback() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.stopOnce.Do(func() { close(f.stopCh) })
	f.workers.Wait()
}

// isStopped reports whether the writeback engine has been shut down.
func (f *Fs) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// enqueueWrite hands a due path to the worker pool, blocking briefly when the
// queue is full so a burst of debounced updates cannot be dropped. It returns
// immediately once the engine is stopped; Shutdown has already flushed by then.
func (f *Fs) enqueueWrite(absRemote string) {
	select {
	case f.ready <- absRemote:
	case <-f.stopCh:
	}
}

func (f *Fs) writebackWorker() {
	defer f.workers.Done()
	for {
		select {
		case absRemote := <-f.ready:
			f.writebackOnce(absRemote)
		case <-f.stopCh:
			return
		}
	}
}

// writebackOnce persists the current pending update for absRemote.
func (f *Fs) writebackOnce(absRemote string) {
	f.mu.Lock()
	pw, ok := f.pending[absRemote]
	if !ok {
		f.mu.Unlock()
		return
	}
	if _, busy := f.inflight[absRemote]; busy {
		// The in-flight write reschedules a superseding generation when it
		// finishes, so there is nothing to do here.
		f.mu.Unlock()
		return
	}
	gen := pw.gen
	data := cloneSidecar(pw.data)
	pw.timer = nil
	f.inflight[absRemote] = gen
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), writebackTimeout)
	var err error
	if f.dirMode {
		err = f.mapWrite(ctx, absRemote) // absRemote is the map directory in this layout
	} else {
		err = f.persistSidecarAbs(ctx, absRemote, data)
	}
	cancel()
	f.finishWriteback(ctx, absRemote, gen, err)
}

// finishWriteback releases the in-flight marker and either drops the pending
// update (success, still current), reschedules it (success, superseded by a
// newer generation) or retries it with exponential backoff.
func (f *Fs) finishWriteback(ctx context.Context, absRemote string, gen uint64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.inflight, absRemote)
	pw := f.pending[absRemote]

	if err == nil {
		f.writeStat.written++
		if pw == nil || pw.gen == gen {
			delete(f.pending, absRemote)
		} else {
			// A newer update arrived while this one was being written.
			f.armRetryLocked(absRemote, pw, 0)
		}
		f.notifyLocked()
		return
	}

	f.writeStat.failed++
	f.writeStat.lastErr = err
	if pw == nil {
		f.notifyLocked()
		return
	}
	pw.fails++
	f.writeStat.retried++
	backoff := f.retryBackoff(pw.fails)
	f.armRetryLocked(absRemote, pw, backoff)
	f.notifyLocked()
	fs.CountError(ctx, err)
	fs.Errorf(f, "posixmeta: sidecar writeback failed for %q (attempt %d, retrying in %v): %v", absRemote, pw.fails, backoff, err)
}

// retryBackoff returns the exponential backoff before retry number n.
func (f *Fs) retryBackoff(n int) time.Duration {
	maxDelay := time.Duration(f.opt.WritebackRetryMax)
	if maxDelay <= 0 {
		maxDelay = 5 * time.Minute
	}
	delay := time.Second
	for i := 1; i < n && delay < maxDelay; i++ {
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

// armRetryLocked (re)arms the timer that enqueues a pending update.
func (f *Fs) armRetryLocked(absRemote string, pw *pendingWrite, delay time.Duration) {
	if pw.timer != nil {
		pw.timer.Stop()
	}
	pw.due = time.Now().Add(delay)
	pw.timer = time.AfterFunc(delay, func() { f.enqueueWrite(absRemote) })
}

// notifyLocked wakes goroutines waiting in flushPendingAbs.
func (f *Fs) notifyLocked() {
	close(f.notify)
	f.notify = make(chan struct{})
}

// flushPending synchronously persists any debounced update for remote.
func (f *Fs) flushPending(ctx context.Context, remote string) error {
	return f.flushPendingAbs(ctx, f.writeUnit(f.absRemote(remote)))
}

// flushPendingAbs synchronously persists the pending update for absRemote,
// waiting for an in-flight writeback of the same path first so updates are
// applied in order. Failures leave the update queued for retry.
func (f *Fs) flushPendingAbs(ctx context.Context, absRemote string) error {
	if f.dirMode {
		for {
			st, err := f.mapStateFor(ctx, absRemote)
			if err != nil {
				return err
			}
			f.mu.Lock()
			dirty := st.dirty()
			f.mu.Unlock()
			if !dirty {
				f.cancelPendingAbs(absRemote)
				return nil
			}
			if err := f.waitInflight(ctx, absRemote); err != nil {
				return err
			}
			if err := f.mapWrite(ctx, absRemote); err != nil {
				return err
			}
		}
	}
	for {
		f.mu.Lock()
		pw, queued := f.pending[absRemote]
		if !queued {
			f.mu.Unlock()
			return nil
		}
		if _, busy := f.inflight[absRemote]; busy {
			waiter := f.notify
			f.mu.Unlock()
			select {
			case <-waiter:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if pw.timer != nil {
			pw.timer.Stop()
			pw.timer = nil
		}
		gen := pw.gen
		data := cloneSidecar(pw.data)
		f.inflight[absRemote] = gen
		f.mu.Unlock()

		err := f.persistSidecarAbs(ctx, absRemote, data)
		f.finishWriteback(ctx, absRemote, gen, err)
		if err != nil {
			return err
		}
	}
}

// flushAllPending persists every queued update. It is used by CleanUp and
// Shutdown, and by tests; updates that fail stay queued for the retry loop.
func (f *Fs) flushAllPending(ctx context.Context) error {
	if f.dirMode {
		return f.flushAllMaps(ctx)
	}
	f.mu.Lock()
	remotes := make([]string, 0, len(f.pending))
	for r := range f.pending {
		remotes = append(remotes, r)
	}
	failedBefore := f.writeStat.failed
	f.mu.Unlock()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, max(1, f.opt.WritebackWorkers))
	)
	for _, r := range remotes {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			mu.Lock()
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			mu.Unlock()
		}
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := f.flushPendingAbs(ctx, r); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()

	if firstErr == nil {
		f.mu.Lock()
		if f.writeStat.failed > failedBefore {
			firstErr = f.writeStat.lastErr
		}
		f.mu.Unlock()
	}
	return firstErr
}

// flushPrefixPending flushes pending updates for dir and everything below it.
func (f *Fs) flushPrefixPending(ctx context.Context, dir string) error {
	prefix := f.absRemote(strings.TrimSuffix(dir, "/"))
	f.mu.Lock()
	var remotes []string
	for r := range f.pending {
		if prefix == "" || r == prefix || strings.HasPrefix(r, prefix+"/") {
			remotes = append(remotes, r)
		}
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

// cancelPendingAbs drops a queued update without writing it, for cases where the
// entry itself is being replaced or deleted. Pending is the source of truth for
// unflushed metadata, so this must only be used when the update is obsolete.
func (f *Fs) cancelPendingAbs(absRemote string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pw, ok := f.pending[absRemote]; ok {
		if pw.timer != nil {
			pw.timer.Stop()
		}
		delete(f.pending, absRemote)
	}
}

// waitInflight waits until no writeback for absRemote is in progress.
func (f *Fs) waitInflight(ctx context.Context, absRemote string) error {
	for {
		f.mu.Lock()
		if _, busy := f.inflight[absRemote]; !busy {
			f.mu.Unlock()
			return nil
		}
		waiter := f.notify
		f.mu.Unlock()
		select {
		case <-waiter:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// quiesceAbs cancels queued updates for absRemote and waits for an in-flight
// writeback of it, so a following move or delete cannot race a stale write that
// would resurrect or overwrite the sidecar.
func (f *Fs) quiesceAbs(ctx context.Context, absRemote string) {
	absRemote = f.writeUnit(absRemote)
	f.cancelPendingAbs(absRemote)
	if err := f.waitInflight(ctx, absRemote); err != nil {
		fs.Debugf(f, "posixmeta: writeback for %q still in flight: %v", absRemote, err)
	}
}

// quiescePrefix is quiesceAbs for dir and everything below it.
func (f *Fs) quiescePrefix(ctx context.Context, dir string) {
	prefix := f.absRemote(strings.TrimSuffix(dir, "/"))
	f.cancelPendingPrefix(dir)

	f.mu.Lock()
	var busy []string
	for r := range f.inflight {
		if prefix == "" || r == prefix || strings.HasPrefix(r, prefix+"/") {
			busy = append(busy, r)
		}
	}
	f.mu.Unlock()

	for _, r := range busy {
		if err := f.waitInflight(ctx, r); err != nil {
			fs.Debugf(f, "posixmeta: writeback for %q still in flight: %v", r, err)
			return
		}
	}
}

// cancelPendingPrefix drops queued updates for dir and everything below it.
func (f *Fs) cancelPendingPrefix(dir string) {
	prefix := f.absRemote(strings.TrimSuffix(dir, "/"))
	f.mu.Lock()
	defer f.mu.Unlock()
	for r, pw := range f.pending {
		if prefix == "" || r == prefix || strings.HasPrefix(r, prefix+"/") {
			if pw.timer != nil {
				pw.timer.Stop()
			}
			delete(f.pending, r)
		}
	}
}

// persistSidecarAbs writes (or deletes) the sidecar for an absolute path.
func (f *Fs) persistSidecarAbs(ctx context.Context, absRemote string, sc *sidecarData) error {
	if f.dirMode {
		changed, err := f.mapApply(ctx, absRemote, sc, time.Now())
		if err != nil || !changed {
			return err
		}
		return f.mapWrite(ctx, f.mapDirOf(absRemote))
	}
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
	if f.dirMode {
		changed, err := f.mapApply(ctx, absRemote, nil, time.Time{})
		if err != nil || !changed {
			return err
		}
		return f.mapWrite(ctx, f.mapDirOf(absRemote))
	}
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

// cacheEntrySize estimates the memory cost of caching one sidecar resolution.
func cacheEntrySize(absRemote string, sc *sidecarData) int64 {
	size := int64(cacheEntryOverhead + len(absRemote))
	if sc == nil {
		return size
	}
	size += int64(len(sc.Mode) + len(sc.Symlink) + len(sc.BTime))
	if sc.UID != nil {
		size += 4
	}
	if sc.GID != nil {
		size += 4
	}
	return size
}

func (f *Fs) putCache(absRemote string, sc *sidecarData) {
	f.putCacheAt(absRemote, sc, time.Now())
}

// putCacheAt is putCache with an explicit sidecar modification time.
func (f *Fs) putCacheAt(absRemote string, sc *sidecarData, modTime time.Time) {
	if f.dirMode {
		return // the map state already holds the update
	}
	f.mu.Lock()
	f.putCacheLocked(absRemote, sc, modTime)
	evicted, entries, bytes := f.evictCacheLocked(absRemote)
	f.mu.Unlock()

	if evicted > 0 {
		fs.Debugf(f, "sidecar cache evicted %d entries (entries=%d max_entries=%d bytes=%d max_bytes=%d)",
			evicted, entries, f.opt.CacheMaxEntries, bytes, int64(f.opt.CacheMaxSize))
	}
}

// putCacheLocked inserts or refreshes one cache entry. It is split from putCache
// so callers that already hold f.mu (listing priming) do not need to re-lock.
func (f *Fs) putCacheLocked(absRemote string, sc *sidecarData, modTime time.Time) {
	ttl := time.Duration(f.opt.CacheTTL)
	if ttl <= 0 {
		return
	}
	size := cacheEntrySize(absRemote, sc)
	data := cloneSidecar(sc)
	expiresAt := time.Now().Add(ttl)

	if ce, ok := f.cache[absRemote]; ok {
		f.cacheBytes += size - ce.size
		ce.data = data
		ce.modTime = modTime
		ce.expiresAt = expiresAt
		ce.size = size
		if ce.elem != nil {
			f.cacheLRU.MoveToBack(ce.elem)
		}
		return
	}
	elem := f.cacheLRU.PushBack(absRemote)
	f.cache[absRemote] = &cacheEntry{
		data:      data,
		modTime:   modTime,
		expiresAt: expiresAt,
		size:      size,
		elem:      elem,
	}
	f.cacheBytes += size
}

// primeNegative records that absRemote has no sidecar, using information already
// obtained from a directory listing instead of a per-entry remote read. A live
// positive entry is never downgraded.
func (f *Fs) primeNegative(absRemote string) {
	if f.dirMode {
		return // maps are cached as a whole by mapStateFor
	}
	if f.opt.CacheTTL <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ce, ok := f.cache[absRemote]; ok && time.Now().Before(ce.expiresAt) {
		return
	}
	f.putCacheLocked(absRemote, nil, time.Time{})
	f.evictCacheLocked(absRemote)
}

// prefetchSidecars warms the sidecar cache for entries whose sidecar appeared in
// a listing, so the stats that follow a directory read are cache hits.
//
// The work is bounded both in concurrency (prefetch_workers) and in total size,
// and failures are ignored because the on-demand path retries and reports them.
func (f *Fs) prefetchSidecars(ctx context.Context, absRemotes []string) {
	if f.dirMode {
		return // one map read already covers the whole directory
	}
	n := f.opt.PrefetchWorkers
	if n <= 0 || len(absRemotes) == 0 || f.opt.CacheTTL <= 0 {
		return
	}
	if n > len(absRemotes) {
		n = len(absRemotes)
	}
	// A directory with thousands of sidecars is rare; cap the work so a listing
	// never stalls for long. Entries beyond the cap are read on demand.
	if limit := 64 * n; len(absRemotes) > limit {
		absRemotes = absRemotes[:limit]
	}

	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, absRemote := range absRemotes {
		select {
		case sem <- struct{}{}:
		case <-pctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(absRemote string) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := f.getSidecarAbs(pctx, absRemote); err != nil {
				fs.Debugf(f, "posixmeta: sidecar prefetch %q failed: %v", absRemote, err)
			}
		}(absRemote)
	}
	wg.Wait()
}

// evictCacheLocked enforces cache_max_entries and cache_max_size by dropping
// least recently used entries. keep is never evicted, so a single entry larger
// than the byte budget stays cached rather than thrashing on every lookup.
// Returns the number evicted plus the resulting occupancy.
func (f *Fs) evictCacheLocked(keep string) (evicted, entries int, bytes int64) {
	maxEntries := f.opt.CacheMaxEntries
	maxBytes := int64(f.opt.CacheMaxSize)
	for (maxEntries > 0 && len(f.cache) > maxEntries) || (maxBytes > 0 && f.cacheBytes > maxBytes) {
		victim := f.lruVictimLocked(keep)
		if victim == "" {
			break
		}
		f.removeCacheEntryLocked(victim)
		evicted++
	}
	f.cacheStat.evictions += uint64(evicted)
	return evicted, len(f.cache), f.cacheBytes
}

// lruVictimLocked returns the least recently used key that is not keep.
func (f *Fs) lruVictimLocked(keep string) string {
	for front := f.cacheLRU.Front(); front != nil; front = front.Next() {
		key, ok := front.Value.(string)
		if !ok {
			continue
		}
		if key == keep {
			continue
		}
		return key
	}
	return ""
}

// removeCacheEntryLocked drops a single entry and keeps the LRU order and byte
// accounting consistent.
func (f *Fs) removeCacheEntryLocked(absRemote string) {
	ce, ok := f.cache[absRemote]
	if !ok {
		return
	}
	delete(f.cache, absRemote)
	if ce.elem != nil {
		f.cacheLRU.Remove(ce.elem)
	}
	f.cacheBytes -= ce.size
	if f.cacheBytes < 0 {
		f.cacheBytes = 0
	}
}

// clearCacheLocked resets the cache while preserving its capacity hint.
func (f *Fs) clearCacheLocked() {
	f.cache = make(map[string]*cacheEntry, len(f.cache))
	f.cacheLRU.Init()
	f.cacheBytes = 0
}

func (f *Fs) invalidateCache(remote string) {
	f.invalidateCacheAbs(f.absRemote(remote))
}

func (f *Fs) invalidateCacheAbs(absRemote string) {
	if f.dirMode {
		// The map of the entry's directory holds its attributes, and a directory
		// entry has a map of its own.
		f.mapDrop(f.mapDirOf(absRemote))
		f.mapDrop(absRemote)
		return
	}
	f.mu.Lock()
	f.removeCacheEntryLocked(absRemote)
	f.mu.Unlock()
}

func (f *Fs) invalidatePrefix(dir string) {
	prefix := f.absRemote(strings.TrimSuffix(dir, "/"))
	if f.dirMode {
		// Everything under prefix is gone or moved, including unflushed state.
		f.mapDiscardPrefix(prefix)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if prefix == "" {
		f.clearCacheLocked()
		return
	}
	for k := range f.cache {
		if k == prefix || strings.HasPrefix(k, prefix+"/") {
			f.removeCacheEntryLocked(k)
		}
	}
}

// invalidateSubtree drops cached resolutions for everything below dir, keeping
// dir's own entry. Owner changes only affect descendants, so this avoids
// throwing away the entry that was just written.
func (f *Fs) invalidateSubtree(dir string) {
	if f.dirMode {
		// Resolution walks the maps on every call, so nothing per-entry is cached
		// below dir; the map that changed was already updated in memory.
		return
	}
	prefix := f.absRemote(strings.TrimSuffix(dir, "/"))
	f.mu.Lock()
	defer f.mu.Unlock()
	if prefix == "" {
		// Nothing is "below" the base root without also being a child, so walk
		// the map and keep the base root entry itself.
		for k := range f.cache {
			if k != "" {
				f.removeCacheEntryLocked(k)
			}
		}
		return
	}
	for k := range f.cache {
		if strings.HasPrefix(k, prefix+"/") {
			f.removeCacheEntryLocked(k)
		}
	}
}

// cacheStatsSnapshot reports cache occupancy and effectiveness for the
// `cachestats` backend command so long-lived mounts can be monitored.
func (f *Fs) cacheStatsSnapshot() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	corruptMaps := 0
	dirtyMaps := 0
	for _, st := range f.maps {
		if st.corrupt != nil {
			corruptMaps++
		}
		if st.dirty() {
			dirtyMaps++
		}
	}
	return map[string]any{
		"layout":           f.opt.SidecarLayout,
		"entries":          len(f.cache),
		"bytes":            f.cacheBytes,
		"max_entries":      f.opt.CacheMaxEntries,
		"max_bytes":        int64(f.opt.CacheMaxSize),
		"maps":             len(f.maps),
		"maps_bytes":       f.mapBytes,
		"corrupt_maps":     corruptMaps,
		"dirty_maps":       dirtyMaps,
		"hits":             f.cacheStat.hits,
		"misses":           f.cacheStat.misses,
		"evictions":        f.cacheStat.evictions,
		"ttl":              time.Duration(f.opt.CacheTTL).String(),
		"pending_writes":   len(f.pending),
		"inflight_writes":  len(f.inflight),
		"written":          f.writeStat.written,
		"failed_writes":    f.writeStat.failed,
		"retried_writes":   f.writeStat.retried,
		"overflow_writes":  f.writeStat.overflow,
		"last_write_error": errString(f.writeStat.lastErr),
	}
}

// errString renders an error for the cachestats report.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
	// Drop any debounced update and wait out an in-flight write first: a late
	// writeback would otherwise recreate the sidecar of a deleted object.
	o.f.quiesceAbs(ctx, o.f.absRemote(remote))
	if err := o.f.deleteSidecarImmediate(ctx, remote); err != nil {
		fs.Debugf(o.f, "posixmeta: cannot remove sidecar for %q: %v", remote, err)
	}
	return nil
}

// Chmod updates the POSIX permission bits on the object via its sparse sidecar.
func (o *posixObject) Chmod(ctx context.Context, mode os.FileMode) error {
	return o.SetMetadata(ctx, chmodMetadata(mode))
}

// Chown updates the POSIX uid and gid on the object via its sparse sidecar.
func (o *posixObject) Chown(ctx context.Context, uid, gid int) error {
	if meta := chownMetadata(uid, gid); len(meta) > 0 {
		return o.SetMetadata(ctx, meta)
	}
	return nil
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

	touched, passthrough, err := applyPOSIXAttrs(&cur, meta, true)
	if err != nil {
		return err
	}
	if touched {
		sc := o.f.buildSparseSidecar(ctx, remote, false, cur)
		if err := o.f.scheduleSidecarUpdate(ctx, remote, sc); err != nil {
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

	oldUID, oldHasUID, oldGID, oldHasGID := cur.uid, cur.hasUID, cur.gid, cur.hasGID

	touched, passthrough, err := applyPOSIXAttrs(&cur, meta, false)
	if err != nil {
		return err
	}
	if touched {
		sc := d.f.buildSparseSidecar(ctx, remote, true, cur)
		if err := d.f.scheduleSidecarUpdate(ctx, remote, sc); err != nil {
			return err
		}
		// Only an ownership change alters what descendants inherit, so avoid
		// throwing the whole subtree's cache away for a plain chmod.
		if cur.uid != oldUID || cur.hasUID != oldHasUID || cur.gid != oldGID || cur.hasGID != oldHasGID {
			d.f.invalidateSubtree(remote)
		}
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
	return d.SetMetadata(ctx, chmodMetadata(mode))
}

// Chown updates the POSIX uid and gid on the directory via its sparse sidecar.
func (d *posixDirectory) Chown(ctx context.Context, uid, gid int) error {
	if meta := chownMetadata(uid, gid); len(meta) > 0 {
		return d.SetMetadata(ctx, meta)
	}
	return nil
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
	// Keep the permission bits plus setuid/setgid/sticky; a larger value is a
	// file-type bit leaking in from somewhere and would corrupt getattr.
	return os.FileMode(val) & 0o7777, nil
}

// posixMetadataKeys is the set of metadata keys this backend stores in sidecars.
var posixMetadataKeys = map[string]struct{}{
	"mode":    {},
	"uid":     {},
	"gid":     {},
	"symlink": {},
	"btime":   {},
}

// hasPOSIXMetadata reports whether meta carries any attribute this backend owns.
func hasPOSIXMetadata(meta fs.Metadata) bool {
	for k := range meta {
		if _, ok := posixMetadataKeys[strings.ToLower(k)]; ok {
			return true
		}
	}
	return false
}

// splitPOSIXMetadata separates the attributes owned by this backend from those
// that belong to the wrapped backend.
func splitPOSIXMetadata(meta fs.Metadata) (posix, passthrough fs.Metadata) {
	posix = make(fs.Metadata)
	passthrough = make(fs.Metadata)
	for k, v := range meta {
		if _, ok := posixMetadataKeys[strings.ToLower(k)]; ok {
			posix[k] = v
		} else {
			passthrough[k] = v
		}
	}
	return posix, passthrough
}

// applyPOSIXAttrs applies the POSIX subset of meta to cur.
//
// It returns whether any POSIX attribute was set, plus the metadata entries that
// belong to the wrapped backend. allowSymlink is false for directories, whose
// symlink attribute is meaningless; atime/ctime are always dropped so reads and
// unrelated updates never cause sidecar churn.
func applyPOSIXAttrs(cur *resolvedMeta, meta fs.Metadata, allowSymlink bool) (touched bool, passthrough fs.Metadata, err error) {
	passthrough = make(fs.Metadata)
	for k, v := range meta {
		switch strings.ToLower(k) {
		case "mode":
			m, perr := parseOctalMode(v, cur.mode)
			if perr != nil {
				return false, nil, fmt.Errorf("posixmeta: invalid mode %q: %w", v, perr)
			}
			cur.mode = m
			cur.hasMode = true
			touched = true
		case "uid":
			u, perr := strconv.ParseUint(v, 10, 32)
			if perr != nil {
				return false, nil, fmt.Errorf("posixmeta: invalid uid %q: %w", v, perr)
			}
			cur.uid = uint32(u)
			cur.hasUID = true
			touched = true
		case "gid":
			g, perr := strconv.ParseUint(v, 10, 32)
			if perr != nil {
				return false, nil, fmt.Errorf("posixmeta: invalid gid %q: %w", v, perr)
			}
			cur.gid = uint32(g)
			cur.hasGID = true
			touched = true
		case "symlink":
			if !allowSymlink {
				continue
			}
			cur.symlink = v
			touched = true
		case "btime":
			if v == "" {
				cur.btime = time.Time{}
				cur.hasBTime = false
			} else {
				bt, perr := time.Parse(time.RFC3339Nano, v)
				if perr != nil {
					return false, nil, fmt.Errorf("posixmeta: invalid btime %q: %w", v, perr)
				}
				cur.btime = bt
				cur.hasBTime = true
			}
			touched = true
		case "atime", "ctime":
			// Intentionally ignored to prevent read/mutation write churn.
		default:
			passthrough[k] = v
		}
	}
	return touched, passthrough, nil
}

// chmodMetadata renders a chmod request as metadata, keeping setuid/setgid/sticky.
func chmodMetadata(mode os.FileMode) fs.Metadata {
	return fs.Metadata{"mode": fmt.Sprintf("0%o", uint32(mode)&0o7777)}
}

// chownMetadata renders a chown request as metadata, omitting unchanged fields
// (the kernel passes -1 for them).
func chownMetadata(uid, gid int) fs.Metadata {
	meta := make(fs.Metadata, 2)
	if uid >= 0 {
		meta["uid"] = strconv.Itoa(uid)
	}
	if gid >= 0 {
		meta["gid"] = strconv.Itoa(gid)
	}
	return meta
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
	_ fs.Copier          = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.CleanUpper      = (*Fs)(nil)
	_ fs.Shutdowner      = (*Fs)(nil)
	_ fs.ListRer         = (*Fs)(nil)
	_ fs.ChangeNotifier  = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.MkdirMetadataer = (*Fs)(nil)
	_ fs.PutUncheckeder  = (*Fs)(nil)
	_ fs.DirSetModTimer  = (*Fs)(nil)
	_ fs.Commander       = (*Fs)(nil)
	_ fs.Object          = (*posixObject)(nil)
	_ fs.ObjectUnWrapper = (*posixObject)(nil)
	_ fs.Metadataer      = (*posixObject)(nil)
	_ fs.SetMetadataer   = (*posixObject)(nil)
	_ fs.Directory       = (*posixDirectory)(nil)
	_ fs.Metadataer      = (*posixDirectory)(nil)
	_ fs.SetMetadataer   = (*posixDirectory)(nil)
)

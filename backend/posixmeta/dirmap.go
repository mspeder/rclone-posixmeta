package posixmeta

// This file implements the alternative `sidecar_layout = dir` storage: one
// aggregate JSON map per directory (`<dir>/<prefix>`) instead of one
// `<dir>/<prefix>.<name>` sidecar per entry.
//
// The document is
//
//	{"v":2,"crc":"<8 hex>","entries":{"<name>":{...}}}
//
// where the checksum is the CRC-32 (IEEE) of the canonical (sorted-key) JSON of
// "entries", written as 8 lowercase hex digits, and the reserved key "." holds
// the base root entry's own attributes (every other directory keeps its
// attributes in its parent's map). The schema version is the sole layout
// discriminator: v1 is the per-entry sparse format and v2 this directory map, so
// a mismatch is reported explicitly instead of being silently ignored.
//
// A map is the unit of read caching and of writeback: it is read once per
// directory (TTL/LRU bounded, single-flight), mutated in memory, and written
// back as a whole snapshot through a staging object plus a server-side move. A
// map that cannot be parsed or whose checksum does not match is never
// overwritten: reads fail for that directory and mutations are refused until it
// is repaired (`rclone backend repair`) or deleted.

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

const (
	// layoutSparse is the default one-file-per-entry layout (schema v1).
	layoutSparse = "sparse"
	// layoutDir is the per-directory aggregate map layout (schema v2).
	layoutDir = "dir"

	// mapSchemaVersion is the sole layout discriminator: v1 is the per-entry
	// sparse sidecar format, v2 this directory map.
	mapSchemaVersion = 2
	mapSelfKey       = "."
	mapTempSuffix    = ".tmp"

	defaultMapMaxEntries = 100000
	defaultMapMaxBytes   = 8 << 20

	onCorruptError = "error"
	onCorruptWarn  = "warn"

	// mapDetectReadSize bounds how much of the root map is read to identify the
	// on-disk layout; the header is always at the start of the document.
	mapDetectReadSize = 4096
)

// errObjectTooLarge is returned by readObjectRaw when a document exceeds the
// size cap, so callers can treat it as corruption instead of an IO failure.
var errObjectTooLarge = errors.New("object is larger than the read limit")

// mapEntry is one entry's attributes inside a directory map. It is sidecarData
// without the per-file schema version (the map document carries it).
type mapEntry struct {
	Mode    string  `json:"mode,omitempty"`
	UID     *uint32 `json:"uid,omitempty"`
	GID     *uint32 `json:"gid,omitempty"`
	Symlink string  `json:"symlink,omitempty"`
	BTime   string  `json:"btime,omitempty"`
}

func mapEntryOf(sc *sidecarData) mapEntry {
	e := mapEntry{Mode: sc.Mode, Symlink: sc.Symlink, BTime: sc.BTime}
	if sc.UID != nil {
		u := *sc.UID
		e.UID = &u
	}
	if sc.GID != nil {
		g := *sc.GID
		e.GID = &g
	}
	return e
}

func (e mapEntry) sidecar() *sidecarData {
	return &sidecarData{
		Version: sidecarSchemaVersion,
		Mode:    e.Mode,
		UID:     e.UID,
		GID:     e.GID,
		Symlink: e.Symlink,
		BTime:   e.BTime,
	}
}

// sidecarMap is the on-disk directory map document.
type sidecarMap struct {
	Version int                 `json:"v"`
	CRC     string              `json:"crc,omitempty"`
	Entries map[string]mapEntry `json:"entries"`
}

// mapSchemaError reports a document that is not a v2 directory map. It is
// either the other layout (v1 sparse) or a future/unknown schema.
type mapSchemaError struct {
	version int
}

func (e *mapSchemaError) Error() string {
	switch {
	case e.version == sidecarSchemaVersion:
		return "stores the per-entry sparse layout (schema v1)"
	case e.version > mapSchemaVersion:
		return fmt.Sprintf("has schema version %d, newer than supported %d", e.version, mapSchemaVersion)
	default:
		return fmt.Sprintf("has unsupported schema version %d", e.version)
	}
}

// mapState is the cached and pending-write state of one directory map.
// entries, gen, bytes and modTime are guarded by Fs.mu; wmu serialises whole-map
// writes so a stale snapshot can never land after a newer one.
type mapState struct {
	dir       string // base-relative directory path ("" is the base root)
	entries   map[string]*sidecarData
	bytes     int64 // sum of entry sizes, for cache_max_size accounting
	gen       uint64
	written   uint64 // gen of the last successful write
	modTime   time.Time
	expiresAt time.Time
	elem      *list.Element
	corrupt   error // non-nil: reads fail, mutations are refused
	wmu       sync.Mutex
}

// mapLoad deduplicates concurrent loads of the same directory map.
type mapLoad struct {
	done chan struct{}
	st   *mapState
	err  error
}

func (st *mapState) dirty() bool { return st.gen != st.written }

func (st *mapState) size() int64 {
	return cacheEntryOverhead + int64(len(st.dir)) + st.bytes
}

// mapPathOf returns the base-relative object path of the map holding dir's
// entries: "<dir>/<prefix>", or just "<prefix>" for the base root.
func (f *Fs) mapPathOf(dir string) string {
	return joinRootRemote(dir, f.opt.SidecarPrefix)
}

// mapDirOf returns the directory whose map holds the attributes of abs.
func (f *Fs) mapDirOf(absRemote string) string {
	return parentRemote(absRemote)
}

// mapKeyOf returns the key of abs inside its directory map.
func mapKeyOf(absRemote string) string {
	clean := strings.Trim(absRemote, "/")
	if clean == "" {
		return mapSelfKey
	}
	return path.Base(clean)
}

// validMapKey reports whether key may appear inside a directory map.
func validMapKey(key string) bool {
	return key != "" && !strings.ContainsAny(key, "/\x00")
}

// encodeMap serialises a directory map, deriving the checksum from the
// canonical JSON of its entries.
func encodeMap(entries map[string]*sidecarData) ([]byte, error) {
	wire := make(map[string]mapEntry, len(entries))
	for k, sc := range entries {
		if sc == nil {
			continue
		}
		wire[k] = mapEntryOf(sc)
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	sum := fmt.Sprintf("%08x", crc32.ChecksumIEEE(body))
	head := fmt.Sprintf(`{"v":%d,"crc":%q,"entries":`, mapSchemaVersion, sum)
	payload := make([]byte, 0, len(head)+len(body)+2)
	payload = append(payload, head...)
	payload = append(payload, body...)
	return append(payload, '}', '\n'), nil
}

// decodeMap parses and verifies a directory map document.
func decodeMap(raw []byte) (map[string]*sidecarData, error) {
	var doc sidecarMap
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if doc.Version != mapSchemaVersion {
		return nil, &mapSchemaError{version: doc.Version}
	}
	if doc.Entries == nil {
		doc.Entries = map[string]mapEntry{}
	}
	body, err := json.Marshal(doc.Entries)
	if err != nil {
		return nil, err
	}
	if want := fmt.Sprintf("%08x", crc32.ChecksumIEEE(body)); doc.CRC != want {
		return nil, fmt.Errorf("checksum mismatch (stored %q, computed %q)", doc.CRC, want)
	}
	out := make(map[string]*sidecarData, len(doc.Entries))
	for k, e := range doc.Entries {
		if !validMapKey(k) {
			return nil, fmt.Errorf("invalid entry name %q", k)
		}
		out[k] = e.sidecar()
	}
	return out, nil
}

// mapDocVersion extracts the schema version from a document without validating
// it, for layout detection. The schema version is the sole layout discriminator.
func mapDocVersion(raw []byte) (version int, ok bool) {
	var doc struct {
		Version int `json:"v"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, false
	}
	return doc.Version, true
}

// readObjectRaw reads a whole small object, returning (nil, now, nil) when it
// does not exist.
func readObjectRaw(ctx context.Context, fsys fs.Fs, remote string, limit int64) ([]byte, time.Time, error) {
	obj, err := fsys.NewObject(ctx, remote)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, err
	}
	modTime := obj.ModTime(ctx)
	rc, err := obj.Open(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	if int64(len(raw)) > limit {
		return nil, modTime, errObjectTooLarge
	}
	return raw, modTime, nil
}

// putObjectRaw uploads a small object to fsys.
func putObjectRaw(ctx context.Context, fsys fs.Fs, remote string, payload []byte) error {
	info := object.NewStaticObjectInfo(remote, time.Now().UTC(), int64(len(payload)), true, nil, fsys)
	_, err := fsys.Put(ctx, bytes.NewReader(payload), info)
	return err
}

// removeObjectRaw deletes an object if it exists.
func removeObjectRaw(ctx context.Context, fsys fs.Fs, remote string) error {
	obj, err := fsys.NewObject(ctx, remote)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return nil
		}
		return err
	}
	if err := obj.Remove(ctx); err != nil && !errors.Is(err, fs.ErrorObjectNotFound) {
		return err
	}
	return nil
}

// putMapObject publishes a map document through a staging object and a
// server-side move when available, so a failed upload never truncates the
// directory's metadata.
func (f *Fs) putMapObject(ctx context.Context, mapPath string, payload []byte) error {
	doMove := f.base.Features().Move
	if doMove == nil {
		return putObjectRaw(ctx, f.base, mapPath, payload)
	}
	tmpPath := mapPath + mapTempSuffix
	if err := putObjectRaw(ctx, f.base, tmpPath, payload); err != nil {
		return err
	}
	tmpObj, err := f.base.NewObject(ctx, tmpPath)
	if err != nil {
		return err
	}
	if _, err := doMove(ctx, tmpObj, mapPath); err != nil {
		_ = tmpObj.Remove(ctx)
		return fmt.Errorf("publish %q: %w", mapPath, err)
	}
	return nil
}

// mapCacheInsertLocked stores a freshly loaded state.
func (f *Fs) mapCacheInsertLocked(st *mapState) {
	if _, ok := f.maps[st.dir]; ok {
		f.mapCacheRemoveLocked(st.dir)
	}
	if ttl := time.Duration(f.opt.CacheTTL); ttl <= 0 {
		st.expiresAt = time.Now()
	} else if st.expiresAt.IsZero() {
		st.expiresAt = time.Now().Add(ttl)
	}
	st.elem = f.mapsLRU.PushBack(st.dir)
	f.maps[st.dir] = st
	f.mapBytes += st.size()
}

// mapCacheRemoveLocked drops a state and keeps the LRU and byte accounting
// exact.
func (f *Fs) mapCacheRemoveLocked(dir string) {
	st, ok := f.maps[dir]
	if !ok {
		return
	}
	delete(f.maps, dir)
	if st.elem != nil {
		f.mapsLRU.Remove(st.elem)
	}
	f.mapBytes -= st.size()
	if f.mapBytes < 0 {
		f.mapBytes = 0
	}
}

// mapEvictLocked enforces cache_max_entries / cache_max_size over maps. Dirty
// maps are never evicted: their state is the only copy of unflushed updates.
func (f *Fs) mapEvictLocked(keep string) (evicted int) {
	maxEntries := f.opt.CacheMaxEntries
	maxBytes := int64(f.opt.CacheMaxSize)
	for (maxEntries > 0 && len(f.maps) > maxEntries) || (maxBytes > 0 && f.mapBytes > maxBytes) {
		victim := ""
		for front := f.mapsLRU.Front(); front != nil; front = front.Next() {
			dir := front.Value.(string)
			if dir == keep {
				continue
			}
			if st := f.maps[dir]; st != nil && st.dirty() {
				continue
			}
			victim = dir
			break
		}
		if victim == "" {
			break
		}
		f.mapCacheRemoveLocked(victim)
		evicted++
	}
	f.cacheStat.evictions += uint64(evicted)
	return evicted
}

// mapDrop forgets a directory map so the next read reloads it. Dirty maps are
// kept because they hold unflushed updates.
func (f *Fs) mapDrop(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.maps[dir]; ok && st.dirty() {
		return
	}
	f.mapCacheRemoveLocked(dir)
}

// mapDiscardPrefix drops the state of every map at or below dir, including dirty
// and corrupt ones. It is used when the subtree they described has been removed.
func (f *Fs) mapDiscardPrefix(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for d := range f.maps {
		if dir != "" && d != dir && !strings.HasPrefix(d, dir+"/") {
			continue
		}
		f.mapCacheRemoveLocked(d)
	}
}

// dropAllMaps clears the map cache (repair after a full scan).
func (f *Fs) dropAllMaps() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maps = make(map[string]*mapState, len(f.maps))
	f.mapsLRU.Init()
	f.mapBytes = 0
}

// mapDirtyDirs lists directories with unflushed updates.
func (f *Fs) mapDirtyDirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var dirs []string
	for dir, st := range f.maps {
		if st.dirty() {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs
}

func sidecarEqual(a, b *sidecarData) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Mode == b.Mode && a.Symlink == b.Symlink && a.BTime == b.BTime &&
		uint32PtrEqual(a.UID, b.UID) && uint32PtrEqual(a.GID, b.GID)
}

func uint32PtrEqual(a, b *uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sidecarBytes estimates the memory cost of one map entry.
func sidecarBytes(key string, sc *sidecarData) int64 {
	size := int64(len(key) + 16)
	if sc == nil {
		return size
	}
	return size + int64(len(sc.Mode)+len(sc.Symlink)+len(sc.BTime)) + 8
}

// loadMap reads one directory map. A corrupt document yields a cached state
// carrying the error, so repeated stats do not re-read the object.
func (f *Fs) loadMap(ctx context.Context, dir string) (*mapState, error) {
	limit := int64(f.opt.MapMaxBytes)
	raw, modTime, err := readObjectRaw(ctx, f.base, f.mapPathOf(dir), limit)
	st := &mapState{
		dir:     dir,
		entries: make(map[string]*sidecarData),
		modTime: modTime,
	}
	if err != nil {
		if !errors.Is(err, errObjectTooLarge) {
			return nil, err
		}
		st.corrupt = f.corruptMapError(dir, fmt.Errorf("larger than map_max_bytes (%d)", limit))
		return st, st.corrupt
	}
	if raw == nil {
		return st, nil
	}
	entries, derr := decodeMap(raw)
	if derr == nil && f.opt.MapMaxEntries > 0 && len(entries) > f.opt.MapMaxEntries {
		derr = fmt.Errorf("holds %d entries, more than map_max_entries (%d)", len(entries), f.opt.MapMaxEntries)
	}
	if derr != nil {
		st.corrupt = f.corruptMapError(dir, derr)
		if f.opt.OnCorrupt == onCorruptWarn {
			fs.Errorf(f, "%v; continuing with defaults (on_corrupt=warn)", st.corrupt)
			return st, nil
		}
		return st, st.corrupt
	}
	st.entries = entries
	for k, sc := range entries {
		st.bytes += sidecarBytes(k, sc)
	}
	return st, nil
}

// mapStateFor returns the cached state of dir's map, loading it on a miss.
// Concurrent loads are coalesced. A reload refreshes the existing state in place
// so pointers held by in-flight updates stay valid; a dirty map is never
// reloaded, because its state is the only copy of unflushed updates. A corrupt
// map is returned together with its error (on_corrupt=error) so callers fail
// closed.
func (f *Fs) mapStateFor(ctx context.Context, dir string) (*mapState, error) {
	now := time.Now()
	for {
		f.mu.Lock()
		if st, ok := f.maps[dir]; ok && (now.Before(st.expiresAt) || st.dirty()) {
			if st.elem != nil {
				f.mapsLRU.MoveToBack(st.elem)
			}
			f.cacheStat.hits++
			err := st.corrupt
			f.mu.Unlock()
			return st, err
		}
		if ml, ok := f.mapLoads[dir]; ok {
			f.mu.Unlock()
			select {
			case <-ml.done:
				return ml.st, ml.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		f.cacheStat.misses++
		ml := &mapLoad{done: make(chan struct{})}
		f.mapLoads[dir] = ml
		f.mu.Unlock()

		fresh, err := f.loadMap(ctx, dir)

		f.mu.Lock()
		delete(f.mapLoads, dir)
		var st *mapState
		if fresh != nil {
			if cur, ok := f.maps[dir]; ok {
				// Refresh in place: another goroutine may still hold the pointer.
				cur.entries = fresh.entries
				cur.bytes = fresh.bytes
				cur.modTime = fresh.modTime
				cur.corrupt = fresh.corrupt
				f.mapTouchLocked(cur)
				st = cur
			} else {
				f.mapCacheInsertLocked(fresh)
				f.mapEvictLocked(dir)
				st = fresh
			}
		}
		f.mu.Unlock()
		ml.st, ml.err = st, err
		close(ml.done)
		return st, err
	}
}

// mapTouchLocked refreshes the TTL of a state and marks it most recently used.
func (f *Fs) mapTouchLocked(st *mapState) {
	if ttl := time.Duration(f.opt.CacheTTL); ttl > 0 {
		st.expiresAt = time.Now().Add(ttl)
	} else {
		st.expiresAt = time.Now() // cache_ttl=0: reload on the next access
	}
	if st.elem != nil {
		f.mapsLRU.MoveToBack(st.elem)
	}
}

// mapEntryInfo resolves the attributes of abs from its directory map.
func (f *Fs) mapEntryInfo(ctx context.Context, absRemote string) (*sidecarData, time.Time, error) {
	st, err := f.mapStateFor(ctx, f.mapDirOf(absRemote))
	if err != nil {
		return nil, time.Time{}, err
	}
	f.mu.Lock()
	sc := cloneSidecar(st.entries[mapKeyOf(absRemote)])
	modTime := st.modTime
	f.mu.Unlock()
	return sc, modTime, nil
}

// corruptMapError wraps the reason a directory map cannot be used, with the
// actionable remedies.
func (f *Fs) corruptMapError(dir string, reason error) error {
	return fmt.Errorf("posixmeta: directory metadata map %q is corrupt (%v); refusing to use it (run `rclone backend repair %s:` to salvage it, or delete the object to reset the directory)",
		f.mapPathOf(dir), reason, f.name)
}

// mapApply sets (or, with sc == nil, deletes) one entry in memory and reports
// whether anything changed. It is the only mutation path, so a corrupt map can
// never be modified and later overwritten.
func (f *Fs) mapApply(ctx context.Context, absRemote string, sc *sidecarData, modTime time.Time) (changed bool, err error) {
	dir := f.mapDirOf(absRemote)
	key := mapKeyOf(absRemote)
	for attempt := 0; ; attempt++ {
		st, err := f.mapStateFor(ctx, dir)
		if err != nil {
			return false, err
		}
		if st.corrupt != nil {
			return false, st.corrupt
		}
		f.mu.Lock()
		if f.maps[dir] != st {
			// The state was evicted between load and lock (only possible while it
			// was still clean): re-apply on the current one.
			f.mu.Unlock()
			if attempt < 8 {
				continue
			}
			return false, fmt.Errorf("posixmeta: directory map %q is being reloaded, retry the operation", f.mapPathOf(dir))
		}
		cur := st.entries[key]
		if sidecarEqual(cur, sc) {
			f.mu.Unlock()
			return false, nil
		}
		st.bytes += sidecarBytes(key, sc) - sidecarBytes(key, cur)
		if sc == nil {
			delete(st.entries, key)
		} else {
			st.entries[key] = cloneSidecar(sc)
			st.modTime = modTime
		}
		st.gen++
		f.mu.Unlock()
		return true, nil
	}
}

// mapWrite persists the whole map of dir as one snapshot. Writes of a map are
// serialised by mapState.wmu, so an older snapshot can never land last.
func (f *Fs) mapWrite(ctx context.Context, dir string) error {
	for attempt := 0; ; attempt++ {
		st, err := f.mapStateFor(ctx, dir)
		if err != nil {
			return err
		}
		if st.corrupt != nil {
			return st.corrupt
		}
		st.wmu.Lock()
		f.mu.Lock()
		attached := f.maps[dir] == st
		clean := st.gen == st.written
		var entries map[string]*sidecarData
		var gen uint64
		if attached {
			entries = make(map[string]*sidecarData, len(st.entries))
			for k, sc := range st.entries {
				entries[k] = cloneSidecar(sc)
			}
			gen = st.gen
		}
		f.mu.Unlock()
		if attached && clean {
			// Everything is already on the remote (e.g. a synchronous write raced
			// the debounce timer): never upload an unchanged map.
			st.wmu.Unlock()
			return nil
		}
		if !attached {
			st.wmu.Unlock()
			if attempt < 8 {
				continue
			}
			return fmt.Errorf("posixmeta: directory map %q is being reloaded, retry the operation", f.mapPathOf(dir))
		}

		err = f.writeMapSnapshot(ctx, dir, st, entries, gen)
		st.wmu.Unlock()
		return err
	}
}

// writeMapSnapshot writes one whole-map snapshot. The caller holds st.wmu, so
// snapshots of the same map cannot be reordered.
func (f *Fs) writeMapSnapshot(ctx context.Context, dir string, st *mapState, entries map[string]*sidecarData, gen uint64) error {
	// Record the layout once, so the remote always states which layout wrote its
	// metadata even when the root has no attributes yet.
	if err := f.ensureRootMap(ctx, dir); err != nil {
		return err
	}

	mapPath := f.mapPathOf(dir)
	if len(entries) == 0 && dir != "" {
		if err := removeObjectRaw(ctx, f.base, mapPath); err != nil {
			return err
		}
	} else {
		payload, err := encodeMap(entries)
		if err != nil {
			return err
		}
		if f.opt.MapMaxEntries > 0 && len(entries) > f.opt.MapMaxEntries {
			return fmt.Errorf("posixmeta: map %q would hold %d entries, more than map_max_entries (%d)", mapPath, len(entries), f.opt.MapMaxEntries)
		}
		if f.opt.MapMaxBytes > 0 && int64(len(payload)) > int64(f.opt.MapMaxBytes) {
			return fmt.Errorf("posixmeta: map %q would be %d bytes, more than map_max_bytes (%d)", mapPath, len(payload), int64(f.opt.MapMaxBytes))
		}
		if err := f.putMapObject(ctx, mapPath, payload); err != nil {
			return err
		}
	}

	f.mu.Lock()
	if st.gen == gen {
		st.written = gen
		st.modTime = time.Now()
	}
	f.mu.Unlock()
	return nil
}

// ensureRootMap writes an empty root map once per process when it is missing, so
// the layout is declared at the remote root.
func (f *Fs) ensureRootMap(ctx context.Context, dir string) error {
	if dir == "" {
		return nil
	}
	f.mu.Lock()
	done := f.rootMapEnsured
	f.mu.Unlock()
	if done {
		return nil
	}
	rootPath := f.mapPathOf("")
	raw, _, err := readObjectRaw(ctx, f.base, rootPath, mapDetectReadSize)
	if err != nil && !errors.Is(err, errObjectTooLarge) {
		return err
	}
	if raw == nil {
		payload, err := encodeMap(nil)
		if err != nil {
			return err
		}
		if err := f.putMapObject(ctx, rootPath, payload); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.rootMapEnsured = true
	f.mu.Unlock()
	return nil
}

// writeUnit maps an absolute entry path to the writeback unit: the entry itself
// for the sparse layout, the directory map for the dir layout.
func (f *Fs) writeUnit(absRemote string) string {
	if f.dirMode {
		return f.mapDirOf(absRemote)
	}
	return absRemote
}

// scheduleDirUpdate applies an update to the in-memory map and debounces the
// whole-map write. The map state is the source of truth, so cancelling a timer
// only postpones the flush instead of losing it.
func (f *Fs) scheduleDirUpdate(ctx context.Context, remote string, sc *sidecarData) error {
	absRemote := f.absRemote(remote)
	dir := f.mapDirOf(absRemote)
	changed, err := f.mapApply(ctx, absRemote, sc, time.Now())
	if err != nil || !changed {
		return err
	}

	delay := time.Duration(f.opt.WritebackDelay)
	if delay <= 0 || f.opt.WritebackWorkers <= 0 || f.isStopped() {
		f.cancelPendingAbs(dir)
		if err := f.waitInflight(ctx, dir); err != nil {
			return err
		}
		return f.mapWrite(ctx, dir)
	}

	f.startWriteback()

	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return f.mapWrite(ctx, dir)
	}
	if f.opt.PendingMaxEntries > 0 && len(f.pending) >= f.opt.PendingMaxEntries {
		if _, queued := f.pending[dir]; !queued {
			f.writeStat.overflow++
			f.mu.Unlock()
			f.cancelPendingAbs(dir)
			if err := f.waitInflight(ctx, dir); err != nil {
				return err
			}
			return f.mapWrite(ctx, dir)
		}
	}
	f.writeGen++
	pw, queued := f.pending[dir]
	if !queued {
		pw = &pendingWrite{}
		f.pending[dir] = pw
	}
	if pw.timer != nil {
		pw.timer.Stop()
	}
	pw.data = nil
	pw.gen = f.writeGen
	pw.fails = 0
	pw.due = time.Now().Add(delay)
	pw.timer = time.AfterFunc(delay, func() { f.enqueueWrite(dir) })
	f.mu.Unlock()
	return nil
}

// appendLinks adds synthetic link objects to a listing, sorted by remote.
func (f *Fs) appendLinks(out fs.DirEntries, links map[string]sidecarLink) fs.DirEntries {
	if len(links) == 0 {
		return out
	}
	targets := make([]string, 0, len(links))
	for target := range links {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, target := range targets {
		out = append(out, f.newLinkObjectFromSidecar(links[target]))
	}
	return out
}

// linksFromState derives proxied symlinks from a directory map: a key with a
// `symlink` attribute and no payload of that name is a link object.
func (f *Fs) linksFromState(st *mapState, dir string, present map[string]struct{}) map[string]sidecarLink {
	f.mu.Lock()
	defer f.mu.Unlock()
	var links map[string]sidecarLink
	for key, sc := range st.entries {
		if key == mapSelfKey || sc == nil || sc.Symlink == "" {
			continue
		}
		remote := joinRootRemote(dir, key)
		if _, ok := present[remote]; ok {
			continue
		}
		if links == nil {
			links = make(map[string]sidecarLink)
		}
		links[remote] = sidecarLink{remote: remote + fs.LinkSuffix, target: sc.Symlink, modTime: st.modTime}
	}
	return links
}

// checkSparseEvidence fails when a directory still holds per-entry sparse
// sidecars, which means the remote was written with sidecar_layout=sparse.
func (f *Fs) checkSparseEvidence(entries fs.DirEntries) error {
	prefix := f.opt.SidecarPrefix
	for _, entry := range entries {
		base := path.Base(strings.TrimSuffix(entry.Remote(), "/"))
		if base == prefix || base == prefix+mapTempSuffix || strings.HasPrefix(base, prefix+".corrupt.") {
			continue
		}
		if strings.HasPrefix(base, prefix+".") {
			return fmt.Errorf("posixmeta: %q holds the per-entry sidecar %q but sidecar_layout=dir; refusing to mix layouts (set sidecar_layout=sparse to use it)",
				entry.Remote(), base)
		}
	}
	return nil
}

// checkDirEvidence fails when a directory holds a bare-prefix map anywhere other
// than the base root's own sidecar, which means the remote was written with
// sidecar_layout=dir.
func (f *Fs) checkDirEvidence(entries fs.DirEntries) error {
	prefix := f.opt.SidecarPrefix
	rootSidecar := f.absRemote("")
	for _, entry := range entries {
		remote := entry.Remote()
		if path.Base(strings.TrimSuffix(remote, "/")) != prefix {
			continue
		}
		if f.absRemote(remote) == rootSidecar {
			continue // the base root entry's own sparse sidecar
		}
		return fmt.Errorf("posixmeta: %q holds the directory map %q but sidecar_layout=sparse; refusing to mix layouts (set sidecar_layout=dir to use it)",
			remote, prefix)
	}
	return nil
}

// flushAllMaps persists every dirty directory map, in parallel. It is the dir
// layout counterpart of flushAllPending and is used by CleanUp, Shutdown and the
// atexit handler.
func (f *Fs) flushAllMaps(ctx context.Context) error {
	dirs := f.mapDirtyDirs()
	if len(dirs) == 0 {
		return nil
	}
	f.mu.Lock()
	failedBefore := f.writeStat.failed
	f.mu.Unlock()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, max(1, f.opt.WritebackWorkers))
	)
	for _, dir := range dirs {
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
		go func(dir string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := f.flushPendingAbs(ctx, dir); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(dir)
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

// writeMapDoc writes a validated directory map document directly, without
// touching the cache. It is used by repair.
func (f *Fs) writeMapDoc(ctx context.Context, dir string, entries map[string]*sidecarData) error {
	mapPath := f.mapPathOf(dir)
	if len(entries) == 0 && dir != "" {
		return removeObjectRaw(ctx, f.base, mapPath)
	}
	payload, err := encodeMap(entries)
	if err != nil {
		return err
	}
	if f.opt.MapMaxBytes > 0 && int64(len(payload)) > int64(f.opt.MapMaxBytes) {
		return fmt.Errorf("posixmeta: map %q would be %d bytes, more than map_max_bytes (%d)", mapPath, len(payload), int64(f.opt.MapMaxBytes))
	}
	return f.putMapObject(ctx, mapPath, payload)
}

// readMapEntryRaw reads one entry's attributes out of a directory map without
// any cache or Fs state, for probes that run before an Fs exists.
func readMapEntryRaw(ctx context.Context, fsys fs.Fs, prefix, absRemote string) (*sidecarData, time.Time, error) {
	dir := parentRemote(absRemote)
	raw, modTime, err := readObjectRaw(ctx, fsys, joinRootRemote(dir, prefix), defaultMapMaxBytes)
	if err != nil || raw == nil {
		return nil, time.Time{}, err
	}
	entries, derr := decodeMap(raw)
	if derr != nil {
		return nil, modTime, nil // unusable maps are absent for probing purposes
	}
	return entries[mapKeyOf(absRemote)], modTime, nil
}

// moveSidecarAbs moves the metadata of srcAbs to dstAbs, keeping the layouts
// apart. It reports whether anything was moved; err reports a failed move.
func (f *Fs) moveSidecarAbs(ctx context.Context, srcAbs, dstAbs, srcSidecar, dstSidecar string) (moved bool, err error) {
	if !f.dirMode {
		// Sidecars are addressed relative to the wrapped base root so entries at
		// or above this Fs root (e.g. a root-level file's own sidecar) are handled.
		doMove := f.base.Features().Move
		if doMove == nil || srcSidecar == dstSidecar {
			return false, nil
		}
		scObj, nerr := f.base.NewObject(ctx, srcSidecar)
		if nerr != nil {
			if errors.Is(nerr, fs.ErrorObjectNotFound) {
				return false, nil
			}
			return false, nerr
		}
		if _, mvErr := doMove(ctx, scObj, dstSidecar); mvErr != nil {
			return false, mvErr
		}
		f.invalidateCacheAbs(srcAbs)
		f.invalidateCacheAbs(dstAbs)
		return true, nil
	}

	sc, modTime, err := f.mapEntryInfo(ctx, srcAbs)
	if err != nil {
		return false, err
	}
	if sc == nil || srcAbs == dstAbs {
		return false, nil
	}
	if _, err := f.mapApply(ctx, dstAbs, sc, modTime); err != nil {
		return false, err
	}
	if _, err := f.mapApply(ctx, srcAbs, nil, time.Time{}); err != nil {
		return false, err
	}
	if err := f.mapWrite(ctx, f.mapDirOf(dstAbs)); err != nil {
		return false, err
	}
	if f.mapDirOf(srcAbs) != f.mapDirOf(dstAbs) {
		if err := f.mapWrite(ctx, f.mapDirOf(srcAbs)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// sortedLinkTargets returns the link targets of links in a stable order.
func sortedLinkTargets(links map[string]sidecarLink) []string {
	if len(links) == 0 {
		return nil
	}
	targets := make([]string, 0, len(links))
	for target := range links {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}

// detectLayout verifies that the remote's on-disk layout matches the configured
// one. Metadata is never converted between layouts.
func (f *Fs) detectLayout(ctx context.Context) error {
	rootMap := f.mapPathOf("")
	raw, _, err := readObjectRaw(ctx, f.base, rootMap, mapDetectReadSize)
	if err != nil && !errors.Is(err, errObjectTooLarge) {
		return fmt.Errorf("posixmeta: cannot inspect %q: %w", rootMap, err)
	}
	version, headerOK := mapDocVersion(raw)
	isDirMap := raw != nil && headerOK && version == mapSchemaVersion

	if !f.dirMode {
		if isDirMap {
			return fmt.Errorf("posixmeta: %q holds a schema v%d directory map but sidecar_layout=sparse; refusing to mix layouts (set sidecar_layout=dir to use it)", rootMap, mapSchemaVersion)
		}
		return nil
	}

	if raw == nil {
		// No root map yet: inherited sparse sidecars in the root would mean the
		// remote belongs to the other layout.
		entries, lerr := f.base.List(ctx, "")
		if lerr != nil {
			fs.Debugf(f, "posixmeta: cannot check %q for sparse sidecars: %v", f.opt.Remote, lerr)
			return nil
		}
		return f.checkSparseEvidence(entries)
	}
	if errors.Is(err, errObjectTooLarge) {
		return nil // a large map is a valid dir map; only its header was unreadable
	}
	if isDirMap {
		return nil
	}
	if headerOK && version == sidecarSchemaVersion {
		return fmt.Errorf("posixmeta: %q holds a schema v%d per-entry sidecar but sidecar_layout=dir; refusing to mix layouts (set sidecar_layout=sparse to use it)", rootMap, version)
	}
	cerr := f.corruptMapError("", fmt.Errorf("%q does not hold a schema v%d directory map", rootMap, mapSchemaVersion))
	if f.opt.OnCorrupt == onCorruptWarn {
		fs.Errorf(f, "%v; continuing with defaults (on_corrupt=warn)", cerr)
		return nil
	}
	return cerr
}

// listMap is List for the dir layout: one map read per directory covers every
// entry, so a stat storm after a listing costs no extra remote reads.
func (f *Fs) listMap(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := f.wrapped.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := f.checkSparseEvidence(entries); err != nil {
		return nil, err
	}
	st, err := f.mapStateFor(ctx, f.absRemote(dir))
	if err != nil {
		return nil, err
	}

	present := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if remote := entry.Remote(); !f.isSidecar(remote) {
			present[remote] = struct{}{}
		}
	}
	var links map[string]sidecarLink
	if f.opt.ProxyLinks {
		links = f.linksFromState(st, dir, present)
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
				continue // a proxied symlink takes precedence over a stale payload
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
	return f.appendLinks(out, links), nil
}

// listRMap is ListR for the dir layout. Each batch loads the maps of the
// directories it touches (cache hits after the first), which also materialises
// proxied symlinks.
func (f *Fs) listRMap(ctx context.Context, dir string, callback fs.ListRCallback) error {
	seenLinks := make(map[string]struct{})
	wrapped := func(entries fs.DirEntries) error {
		if err := f.checkSparseEvidence(entries); err != nil {
			return err
		}
		present := make(map[string]struct{}, len(entries))
		states := make(map[string]*mapState)

		// A map object in the batch is the analogue of a sparse sidecar: load
		// it so its symlink entries can be published.
		for _, entry := range entries {
			remote := entry.Remote()
			if f.isSidecar(remote) {
				if path.Base(remote) == f.opt.SidecarPrefix {
					if _, err := f.mapStateFor(ctx, f.mapDirOf(f.absRemote(remote))); err != nil {
						return err
					}
				}
				continue
			}
			present[remote] = struct{}{}
		}

		var links map[string]sidecarLink
		if f.opt.ProxyLinks {
			links = make(map[string]sidecarLink)
			for _, entry := range entries {
				remote := entry.Remote()
				if f.isSidecar(remote) {
					continue
				}
				rdir := parentRemote(remote)
				st, ok := states[rdir]
				if !ok {
					var err error
					if st, err = f.mapStateFor(ctx, f.absRemote(rdir)); err != nil {
						return err
					}
					states[rdir] = st
				}
				for target, link := range f.linksFromState(st, rdir, present) {
					links[target] = link
				}
			}
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
		// A batch can split a directory, so a link is published at most once.
		for _, target := range sortedLinkTargets(links) {
			if _, done := seenLinks[target]; done {
				continue
			}
			seenLinks[target] = struct{}{}
			out = append(out, f.newLinkObjectFromSidecar(links[target]))
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

// quiesceUnit cancels the queued write of a unit and waits for one in flight.
func (f *Fs) quiesceUnit(ctx context.Context, unit string) {
	f.cancelPendingAbs(unit)
	if err := f.waitInflight(ctx, unit); err != nil {
		fs.Debugf(f, "posixmeta: writeback for %q still in flight: %v", unit, err)
	}
}

// rmdirMap removes an empty directory whose only remaining object may be its own
// map, then drops the directory's key from its parent map.
func (f *Fs) rmdirMap(ctx context.Context, dir string) error {
	absDir := f.absRemote(dir)
	// The map of dir only holds its children and disappears with the directory;
	// the map of its parent holds dir's own attributes and must survive.
	f.quiesceUnit(ctx, absDir)
	f.quiesceAbs(ctx, absDir)

	err := f.wrapped.Rmdir(ctx, dir)
	if err != nil {
		entries, lerr := f.wrapped.List(ctx, dir)
		if lerr != nil {
			return err
		}
		removed := false
		for _, entry := range entries {
			base := path.Base(strings.TrimSuffix(entry.Remote(), "/"))
			if base == f.opt.SidecarPrefix || base == f.opt.SidecarPrefix+mapTempSuffix {
				if rerr := removeObjectRaw(ctx, f.base, f.absRemote(entry.Remote())); rerr == nil {
					removed = true
				}
				continue
			}
			return err // a real entry is still there
		}
		if !removed {
			return err
		}
		if rerr := f.wrapped.Rmdir(ctx, dir); rerr != nil {
			return err
		}
	}

	f.mapDiscardPrefix(absDir)
	if derr := f.deleteSidecarAbsImmediate(ctx, absDir); derr != nil {
		fs.Debugf(f, "posixmeta: cannot remove directory metadata for %q: %v", dir, derr)
	}
	return nil
}

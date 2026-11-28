package posixmeta

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

// flakyBase fails the first fails Put calls to exercise writeback retries.
type flakyBase struct {
	fs.Fs
	mu    sync.Mutex
	fails int
	puts  int
}

func (b *flakyBase) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	b.mu.Lock()
	b.puts++
	fail := b.fails > 0
	if fail {
		b.fails--
	}
	b.mu.Unlock()
	if fail {
		return nil, errors.New("injected Put failure")
	}
	return b.Fs.Put(ctx, in, src, options...)
}

func (b *flakyBase) counts() (puts, remainingFails int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.puts, b.fails
}

// gatedBase blocks the first Put until release is closed, so a test can force a
// newer update to arrive while an older one is still being written.
type gatedBase struct {
	fs.Fs
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (b *gatedBase) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	blocked := false
	b.once.Do(func() { blocked = true })
	if blocked {
		close(b.started)
		<-b.release
	}
	return b.Fs.Put(ctx, in, src, options...)
}

// slowCountingBase counts NewObject calls and makes them slow enough for
// concurrent callers to overlap.
type slowCountingBase struct {
	fs.Fs
	mu      sync.Mutex
	objects int
}

func (b *slowCountingBase) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	b.mu.Lock()
	b.objects++
	b.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	return b.Fs.NewObject(ctx, remote)
}

func (b *slowCountingBase) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.objects
}

// noPurgeBase hides the wrapped backend's Purge so the fallback path is used.
type noPurgeBase struct {
	fs.Fs
}

func (b noPurgeBase) Features() *fs.Features {
	feats := *b.Fs.Features()
	feats.Purge = nil
	return &feats
}

// waitFor polls cond until it is true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWritebackRetriesUntilSuccess(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rawDir, "d"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f := newTestFs(t, rawDir, map[string]string{
		"writeback_delay":     "1ms",
		"writeback_retry_max": "5ms",
	})
	base := &flakyBase{Fs: f.base, fails: 2}
	f.base = base

	if err := f.scheduleSidecarUpdate(ctx, "d/x.txt", &sidecarData{
		Version: sidecarSchemaVersion,
		Mode:    "0700",
	}); err != nil {
		t.Fatalf("scheduleSidecarUpdate: %v", err)
	}

	sidecarPath := filepath.Join(rawDir, sidecarRel("d/x.txt"))
	waitFor(t, 10*time.Second, "sidecar to be written after retries", func() bool {
		_, err := os.Stat(sidecarPath)
		return err == nil
	})
	if sc := readSidecar(t, rawDir, "d/x.txt"); sc.Mode != "0700" {
		t.Fatalf("sidecar mode = %q, want 0700", sc.Mode)
	}

	puts, remaining := base.counts()
	if remaining != 0 {
		t.Fatalf("%d injected failures were not retried", remaining)
	}
	if puts < 3 {
		t.Fatalf("Put attempts = %d, want at least 3 (2 failures + 1 success)", puts)
	}

	f.mu.Lock()
	failed, retried := f.writeStat.failed, f.writeStat.retried
	f.mu.Unlock()
	if failed < 2 || retried < 2 {
		t.Fatalf("writeStats failed=%d retried=%d, want both >= 2", failed, retried)
	}
	if stats := f.cacheStatsSnapshot(); stats["last_write_error"] == "" {
		t.Fatal("cachestats should report the last writeback error")
	}
}

func TestWritebackKeepsNewestUpdate(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{
		"writeback_delay":     "1ms",
		"writeback_retry_max": "5ms",
	})
	base := &gatedBase{Fs: f.base, started: make(chan struct{}), release: make(chan struct{})}
	f.base = base

	if err := f.scheduleSidecarUpdate(ctx, "x.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"}); err != nil {
		t.Fatalf("scheduleSidecarUpdate: %v", err)
	}
	select {
	case <-base.started:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the first writeback to start")
	}

	// A newer update arrives while the older one is still being written.
	if err := f.scheduleSidecarUpdate(ctx, "x.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0750"}); err != nil {
		t.Fatalf("scheduleSidecarUpdate: %v", err)
	}
	close(base.release)

	if err := f.flushAllPending(ctx); err != nil {
		t.Fatalf("flushAllPending: %v", err)
	}
	if sc := readSidecar(t, rawDir, "x.txt"); sc.Mode != "0750" {
		t.Fatalf("sidecar mode = %q, want the newest value 0750", sc.Mode)
	}
}

func TestPendingCancelledOnRemove(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "f.txt"), "hello")
	// A very long debounce proves Remove cancels the queued update instead of
	// letting it recreate the sidecar of the deleted object.
	f := newTestFs(t, rawDir, map[string]string{"writeback_delay": "1h"})

	obj, err := f.NewObject(ctx, "f.txt")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	po, ok := obj.(*posixObject)
	if !ok {
		t.Fatalf("NewObject returned %T, want *posixObject", obj)
	}
	if err := po.SetMetadata(ctx, fs.Metadata{"mode": "0700"}); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	f.mu.Lock()
	queued := len(f.pending)
	f.mu.Unlock()
	if queued != 1 {
		t.Fatalf("pending updates = %d, want 1", queued)
	}

	if err := po.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := f.flushAllPending(ctx); err != nil {
		t.Fatalf("flushAllPending: %v", err)
	}

	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("f.txt"))); !os.IsNotExist(err) {
		t.Fatalf("sidecar must not be resurrected after Remove (stat err=%v)", err)
	}
	f.mu.Lock()
	queued = len(f.pending)
	f.mu.Unlock()
	if queued != 0 {
		t.Fatalf("pending updates after Remove = %d, want 0", queued)
	}
}

func TestShutdownFlushesPending(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{"writeback_delay": "1h"})

	if err := f.scheduleSidecarUpdate(ctx, "x.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"}); err != nil {
		t.Fatalf("scheduleSidecarUpdate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("x.txt"))); !os.IsNotExist(err) {
		t.Fatalf("sidecar must still be debounced before Shutdown (stat err=%v)", err)
	}

	if err := f.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if sc := readSidecar(t, rawDir, "x.txt"); sc.Mode != "0700" {
		t.Fatalf("sidecar mode = %q, want 0700", sc.Mode)
	}
	if !f.isStopped() {
		t.Fatal("Shutdown must stop the writeback engine")
	}

	// After Shutdown updates are persisted synchronously, so nothing is lost.
	if err := f.scheduleSidecarUpdate(ctx, "y.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0750"}); err != nil {
		t.Fatalf("scheduleSidecarUpdate after Shutdown: %v", err)
	}
	if sc := readSidecar(t, rawDir, "y.txt"); sc.Mode != "0750" {
		t.Fatalf("sidecar mode after Shutdown = %q, want 0750", sc.Mode)
	}

	// Shutdown must be idempotent (CleanUp and Shutdown can both run).
	if err := f.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestPendingCapFallsBackToSync(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{
		"writeback_delay":     "1h",
		"pending_max_entries": "1",
	})

	if err := f.scheduleSidecarUpdate(ctx, "a.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"}); err != nil {
		t.Fatalf("scheduleSidecarUpdate(a): %v", err)
	}
	if err := f.scheduleSidecarUpdate(ctx, "b.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0750"}); err != nil {
		t.Fatalf("scheduleSidecarUpdate(b): %v", err)
	}

	if sc := readSidecar(t, rawDir, "b.txt"); sc.Mode != "0750" {
		t.Fatalf("overflow update was not persisted synchronously (mode=%q)", sc.Mode)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("a.txt"))); !os.IsNotExist(err) {
		t.Fatalf("queued update must stay debounced (stat err=%v)", err)
	}
	f.mu.Lock()
	queued, overflow := len(f.pending), f.writeStat.overflow
	f.mu.Unlock()
	if queued != 1 || overflow != 1 {
		t.Fatalf("pending=%d overflow=%d, want 1 and 1", queued, overflow)
	}
}

func TestGetSidecarSingleflight(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{"cache_ttl": "5m"})
	base := &slowCountingBase{Fs: f.base}
	f.base = base

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc, err := f.getSidecarAbs(ctx, "missing.txt")
			if err != nil {
				t.Errorf("getSidecarAbs: %v", err)
			}
			if sc != nil {
				t.Errorf("sidecar = %+v, want nil", sc)
			}
		}()
	}
	wg.Wait()

	if got := base.count(); got != 1 {
		t.Fatalf("sidecar reads = %d, want 1 (concurrent reads must be coalesced)", got)
	}
}

func TestListPrimesSidecarCache(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "a.txt"), "hello")
	writeFile(t, filepath.Join(rawDir, "b.txt"), "hello")
	writeSidecar(t, rawDir, "a.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})

	f := newTestFs(t, rawDir, map[string]string{"cache_ttl": "5m"})
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatalf("List: %v", err)
	}

	f.mu.Lock()
	missesAfterList := f.cacheStat.misses
	f.mu.Unlock()

	sc, err := f.getSidecarAbs(ctx, "a.txt")
	if err != nil {
		t.Fatalf("getSidecarAbs(a.txt): %v", err)
	}
	if sc == nil || sc.Mode != "0700" {
		t.Fatalf("a.txt sidecar = %+v, want mode 0700", sc)
	}
	sc, err = f.getSidecarAbs(ctx, "b.txt")
	if err != nil {
		t.Fatalf("getSidecarAbs(b.txt): %v", err)
	}
	if sc != nil {
		t.Fatalf("b.txt sidecar = %+v, want nil", sc)
	}

	f.mu.Lock()
	misses, hits := f.cacheStat.misses, f.cacheStat.hits
	f.mu.Unlock()
	if misses != missesAfterList {
		t.Fatalf("lookups after List caused %d extra remote reads, want 0", misses-missesAfterList)
	}
	if hits < 2 {
		t.Fatalf("cache hits = %d, want at least 2", hits)
	}
}
func TestRmdirRemovesOrphanSidecar(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rawDir, "d"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// A sidecar whose payload is gone must not make the directory unremovable.
	writeSidecar(t, rawDir, "d/orphan", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	f := newTestFs(t, rawDir, nil)

	if err := f.Rmdir(ctx, "d"); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "d")); !os.IsNotExist(err) {
		t.Fatalf("directory still exists (stat err=%v)", err)
	}
}

func TestPurgeFallbackRemovesSidecars(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "d", "f.txt"), "hello")
	writeSidecar(t, rawDir, "d/f.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	f := newTestFs(t, rawDir, nil)
	f.wrapped = noPurgeBase{Fs: f.wrapped}

	if err := f.Purge(ctx, "d"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "d")); !os.IsNotExist(err) {
		t.Fatalf("purged directory still exists (stat err=%v)", err)
	}
}

func TestResolveIgnoresUnusableSidecars(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "big.txt"), "hello")
	writeFile(t, filepath.Join(rawDir, "new.txt"), "hello")
	// Oversized (treated as corrupt rather than loaded) and future-schema.
	if err := os.WriteFile(filepath.Join(rawDir, sidecarRel("big.txt")), bytes.Repeat([]byte("{"), maxSidecarSize+1), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writeFile(t, filepath.Join(rawDir, sidecarRel("new.txt")), `{"v":99,"mode":"0700"}`)

	f := newTestFs(t, rawDir, nil)
	for _, remote := range []string{"big.txt", "new.txt"} {
		res, err := f.resolve(ctx, remote, false)
		if err != nil {
			t.Fatalf("resolve(%s): %v", remote, err)
		}
		if res.hasMode || res.mode != f.defaultFileMode {
			t.Fatalf("resolve(%s) mode=%o hasMode=%v, want default 0644", remote, res.mode, res.hasMode)
		}
	}
}

func TestMkdirMetadataStoresSidecar(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, nil)

	d, err := f.MkdirMetadata(ctx, "proj", fs.Metadata{
		"mode": "0700",
		"uid":  "2001",
		"gid":  "2002",
	})
	if err != nil {
		t.Fatalf("MkdirMetadata: %v", err)
	}
	if d.Remote() != "proj" {
		t.Fatalf("MkdirMetadata remote = %q, want proj", d.Remote())
	}
	sc := readSidecar(t, rawDir, "proj")
	if sc.Mode != "0700" || sc.UID == nil || *sc.UID != 2001 || sc.GID == nil || *sc.GID != 2002 {
		t.Fatalf("unexpected sidecar %+v", sc)
	}
}

// metaObjInfo is an ObjectInfo carrying metadata, standing in for a
// --metadata source.
type metaObjInfo struct {
	fs.ObjectInfo
	meta fs.Metadata
}

func (m metaObjInfo) Metadata(context.Context) (fs.Metadata, error) { return m.meta, nil }

func TestPutAppliesIncomingMetadata(t *testing.T) {
	ctx := context.Background()
	cfg := fs.GetConfig(ctx)
	wasMetadata := cfg.Metadata
	cfg.Metadata = true
	defer func() { cfg.Metadata = wasMetadata }()

	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, nil)
	src := metaObjInfo{
		ObjectInfo: object.NewStaticObjectInfo("x.txt", time.Now(), 3, true, nil, f.wrapped),
		meta:       fs.Metadata{"mode": "0600", "uid": "2001"},
	}
	if _, err := f.Put(ctx, strings.NewReader("abc"), src); err != nil {
		t.Fatalf("Put: %v", err)
	}
	sc := readSidecar(t, rawDir, "x.txt")
	if sc.Mode != "0600" || sc.UID == nil || *sc.UID != 2001 {
		t.Fatalf("unexpected sidecar %+v", sc)
	}
}

func TestPutOverwriteMetadataPolicy(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		policy   string
		wantMode string // "" means the sidecar must be gone
	}{
		{name: "preserve keeps previous metadata", policy: "preserve", wantMode: "0700"},
		{name: "reset drops stale metadata", policy: "reset", wantMode: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rawDir := t.TempDir()
			writeFile(t, filepath.Join(rawDir, "x.txt"), "old")
			writeSidecar(t, rawDir, "x.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
			f := newTestFs(t, rawDir, map[string]string{"overwrite_metadata": tc.policy})

			src := object.NewStaticObjectInfo("x.txt", time.Now(), 3, true, nil, f.wrapped)
			if _, err := f.Put(ctx, strings.NewReader("new"), src); err != nil {
				t.Fatalf("Put: %v", err)
			}
			_, statErr := os.Stat(filepath.Join(rawDir, sidecarRel("x.txt")))
			if tc.wantMode == "" {
				if !os.IsNotExist(statErr) {
					t.Fatalf("stale sidecar survived the overwrite (stat err=%v)", statErr)
				}
				return
			}
			if statErr != nil {
				t.Fatalf("sidecar missing after overwrite: %v", statErr)
			}
			if sc := readSidecar(t, rawDir, "x.txt"); sc.Mode != tc.wantMode {
				t.Fatalf("sidecar mode = %q, want %q", sc.Mode, tc.wantMode)
			}
		})
	}
}

func TestInvalidOverwriteMetadataRejected(t *testing.T) {
	_, err := NewFs(context.Background(), "test", "", testConfig(t.TempDir(), map[string]string{
		"overwrite_metadata": "nonsense",
	}))
	if err == nil {
		t.Fatal("expected NewFs to reject an unknown overwrite_metadata value")
	}
	if !strings.Contains(err.Error(), "overwrite_metadata") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestChmodMetadataKeepsSpecialBits(t *testing.T) {
	meta := chmodMetadata(0o4755 | os.ModeSetuid)
	if meta["mode"] != "04755" {
		t.Fatalf("chmod metadata = %q, want 04755", meta["mode"])
	}
	mode, err := parseOctalMode(meta["mode"], 0)
	if err != nil {
		t.Fatalf("parseOctalMode: %v", err)
	}
	if mode != 0o4755 {
		t.Fatalf("parsed mode = %o, want 04755", mode)
	}
	// File-type bits must never leak into a stored mode.
	if mode, err := parseOctalMode("10644", 0); err != nil || mode != 0o644 {
		t.Fatalf("parseOctalMode(10644) = %o, %v; want 0644", mode, err)
	}
}

func TestListRHidesSidecars(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "a.txt"), "hello")
	writeSidecar(t, rawDir, "a.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	writeFile(t, filepath.Join(rawDir, "sub", "b.txt"), "hello")

	f := newTestFs(t, rawDir, map[string]string{"cache_ttl": "5m"})
	var seen []string
	err := f.ListR(ctx, "", func(entries fs.DirEntries) error {
		for _, entry := range entries {
			seen = append(seen, entry.Remote())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ListR: %v", err)
	}
	want := map[string]bool{"a.txt": true, "sub": true, "sub/b.txt": true}
	for _, remote := range seen {
		if isTestSidecar(remote) {
			t.Fatalf("ListR leaked sidecar %q", remote)
		}
		delete(want, remote)
	}
	if len(want) != 0 {
		t.Fatalf("ListR missed entries %v (saw %v)", want, seen)
	}

	// A recursive walk must not poison the cache with "no metadata" for entries
	// whose sidecar it did not look at.
	res, err := f.resolve(ctx, "a.txt", false)
	if err != nil {
		t.Fatalf("resolve after ListR: %v", err)
	}
	if !res.hasMode || res.mode != 0o700 {
		t.Fatalf("mode after ListR = %o (has=%v), want 0700", res.mode, res.hasMode)
	}
}

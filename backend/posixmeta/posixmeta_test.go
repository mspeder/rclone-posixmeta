package posixmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"

	_ "github.com/rclone/rclone/backend/local" // register the local backend for wrapped remotes
)

// TestMain installs an isolated config containing a `[local]` remote so the
// wrapped local backend can be opened without touching the developer's
// rclone.conf.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "posixmeta-test-config-")
	if err != nil {
		panic(err)
	}
	configPath := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(configPath, []byte("[local]\ntype = local\n"), 0o600); err != nil {
		panic(err)
	}
	config.SetConfigPath(configPath)
	configfile.Install()

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// testConfig returns the default config map used by the unit tests.
func testConfig(rawDir string, extra map[string]string) configmap.Simple {
	m := configmap.Simple{
		"remote":               "local:" + rawDir,
		"default_file_mode":    "0644",
		"default_dir_mode":     "0755",
		"default_uid":          "1000",
		"default_gid":          "1000",
		"inherit_parent_owner": "true",
		"sidecar_prefix":       ".posixmeta",
		"hide_sidecars":        "true",
		"cache_ttl":            "0",
		"writeback_delay":      "0",
		"writeback_workers":    "4",
		"writeback_retry_max":  "1m",
		"pending_max_entries":  "1024",
		"cache_max_entries":    "65536",
		"cache_max_size":       "64M",
		"prefetch_workers":     "4",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// newTestFsAt builds a posixmeta Fs rooted at rawDir/root.
func newTestFsAt(t *testing.T, rawDir, root string, extra map[string]string) *Fs {
	t.Helper()
	fsi, err := NewFs(context.Background(), "test", root, testConfig(rawDir, extra))
	if err != nil {
		t.Fatalf("NewFs(root=%q): %v", root, err)
	}
	f, ok := fsi.(*Fs)
	if !ok {
		t.Fatalf("NewFs returned %T, want *Fs", fsi)
	}
	t.Cleanup(func() { _ = f.Shutdown(context.Background()) })
	return f
}

// newTestFs builds a posixmeta Fs wrapping a real local directory with
// deterministic defaults. cache_ttl/writeback_delay are zero so sidecar
// reads and writes are synchronous in tests.
func newTestFs(t *testing.T, rawDir string, extra map[string]string) *Fs {
	t.Helper()
	return newTestFsAt(t, rawDir, "", extra)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %q: %v", path, err)
	}
}

func writeSidecar(t *testing.T, rawDir, remote string, sc *sidecarData) {
	t.Helper()
	payload, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	writeFile(t, filepath.Join(rawDir, sidecarRel(remote)), string(payload))
}

func readSidecar(t *testing.T, rawDir, remote string) sidecarData {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(rawDir, sidecarRel(remote)))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var sc sidecarData
	if err := json.Unmarshal(payload, &sc); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}
	return sc
}

func uint32Ptr(v uint32) *uint32 { return &v }

// sidecarRel returns the sidecar path (relative to the wrapped root) of a test
// remote, e.g. "d/x.txt" -> "d/.posixmeta.x.txt".
func sidecarRel(remote string) string {
	return sidecarPathFor(defaultSidecarPrefix, remote)
}

// isTestSidecar reports whether remote names a sidecar of the current layout.
func isTestSidecar(remote string) bool {
	base := filepath.Base(remote)
	return base == defaultSidecarPrefix || strings.HasPrefix(base, defaultSidecarPrefix+".")
}

func TestParseOctalMode(t *testing.T) {
	tests := []struct {
		in       string
		want     os.FileMode
		wantErr  bool
		fallback os.FileMode
	}{
		{in: "0644", want: 0o644},
		{in: "644", want: 0o644},
		{in: "0o750", want: 0o750},
		{in: "0O755", want: 0o755},
		{in: "  0700  ", want: 0o700},
		{in: "", want: 0o755, fallback: 0o755},
		{in: "999", wantErr: true},
		{in: "not-a-mode", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseOctalMode(tc.in, tc.fallback)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseOctalMode(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseOctalMode(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseOctalMode(%q) = %04o, want %04o", tc.in, got, tc.want)
		}
	}
}

func TestAncestorDirs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{in: "", want: nil},
		{in: "a", want: nil},
		{in: "a/b", want: []string{"a"}},
		{in: "a/b/c", want: []string{"a/b", "a"}},
		{in: "/a/b/", want: []string{"a"}},
	}
	for _, tc := range tests {
		got := ancestorDirs(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("ancestorDirs(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ancestorDirs(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestNewFsValidation(t *testing.T) {
	base := func() configmap.Simple {
		return configmap.Simple{
			"remote":            "local:" + t.TempDir(),
			"default_file_mode": "0644",
			"default_dir_mode":  "0755",
		}
	}

	t.Run("missing remote", func(t *testing.T) {
		m := base()
		delete(m, "remote")
		if _, err := NewFs(context.Background(), "test", "", m); err == nil {
			t.Fatal("expected error for missing remote")
		}
	})

	t.Run("suffix with slash", func(t *testing.T) {
		m := base()
		m["sidecar_prefix"] = "bad/suffix"
		if _, err := NewFs(context.Background(), "test", "", m); err == nil {
			t.Fatal("expected error for sidecar_prefix containing '/'")
		}
	})

	t.Run("invalid default file mode", func(t *testing.T) {
		m := base()
		m["default_file_mode"] = "not-octal"
		if _, err := NewFs(context.Background(), "test", "", m); err == nil {
			t.Fatal("expected error for invalid default_file_mode")
		}
	})
}

func TestBuildSparseSidecar(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, nil)

	btime := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		remote  string
		isDir   bool
		desired resolvedMeta
		check   func(t *testing.T, sc *sidecarData)
	}{
		{
			name:   "all defaults -> nil",
			remote: "a.txt",
			desired: resolvedMeta{
				mode: 0o644, hasMode: true,
				uid: 1000, hasUID: true,
				gid: 1000, hasGID: true,
			},
			check: func(t *testing.T, sc *sidecarData) {
				if sc != nil {
					t.Fatalf("expected nil sparse sidecar, got %+v", sc)
				}
			},
		},
		{
			name:   "dir defaults -> nil",
			remote: "d",
			isDir:  true,
			desired: resolvedMeta{
				mode: 0o755, hasMode: true,
				uid: 1000, hasUID: true,
				gid: 1000, hasGID: true,
			},
			check: func(t *testing.T, sc *sidecarData) {
				if sc != nil {
					t.Fatalf("expected nil sparse sidecar, got %+v", sc)
				}
			},
		},
		{
			name:   "mode override",
			remote: "a.txt",
			desired: resolvedMeta{
				mode: 0o750, hasMode: true,
				uid: 1000, hasUID: true,
				gid: 1000, hasGID: true,
			},
			check: func(t *testing.T, sc *sidecarData) {
				if sc == nil || sc.Mode != "0750" {
					t.Fatalf("expected mode 0750, got %+v", sc)
				}
				if sc.UID != nil || sc.GID != nil {
					t.Fatalf("uid/gid must remain sparse, got %+v", sc)
				}
			},
		},
		{
			name:   "uid and gid override",
			remote: "a.txt",
			desired: resolvedMeta{
				mode: 0o644, hasMode: true,
				uid: 2001, hasUID: true,
				gid: 2002, hasGID: true,
			},
			check: func(t *testing.T, sc *sidecarData) {
				if sc == nil || sc.UID == nil || *sc.UID != 2001 || sc.GID == nil || *sc.GID != 2002 {
					t.Fatalf("expected uid/gid 2001/2002, got %+v", sc)
				}
				if sc.Mode != "" {
					t.Fatalf("mode must remain sparse, got %+v", sc)
				}
			},
		},
		{
			name:   "btime and symlink",
			remote: "a.txt",
			desired: resolvedMeta{
				mode: 0o644, hasMode: true,
				uid: 1000, hasUID: true,
				gid: 1000, hasGID: true,
				btime: btime, hasBTime: true,
				symlink: "/target",
			},
			check: func(t *testing.T, sc *sidecarData) {
				if sc == nil {
					t.Fatal("expected sidecar")
				}
				if sc.BTime != "2026-11-22T08:00:00Z" {
					t.Fatalf("btime = %q", sc.BTime)
				}
				if sc.Symlink != "/target" {
					t.Fatalf("symlink = %q", sc.Symlink)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := f.buildSparseSidecar(ctx, tc.remote, tc.isDir, tc.desired)
			tc.check(t, sc)
		})
	}
}

func TestBuildSparseSidecarInheritsParentOwner(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, nil)

	writeSidecar(t, rawDir, "proj", &sidecarData{
		Version: sidecarSchemaVersion,
		UID:     uint32Ptr(2001),
		GID:     uint32Ptr(2002),
	})

	// Matching the inherited owner must stay sparse.
	sc := f.buildSparseSidecar(ctx, "proj/f.txt", false, resolvedMeta{
		uid: 2001, hasUID: true,
		gid: 2002, hasGID: true,
	})
	if sc != nil {
		t.Fatalf("expected nil sidecar when matching inherited owner, got %+v", sc)
	}

	// Overriding only the uid must persist only the uid.
	sc = f.buildSparseSidecar(ctx, "proj/f.txt", false, resolvedMeta{
		uid: 3000, hasUID: true,
		gid: 2002, hasGID: true,
	})
	if sc == nil || sc.UID == nil || *sc.UID != 3000 {
		t.Fatalf("expected uid 3000 override, got %+v", sc)
	}
	if sc.GID != nil {
		t.Fatalf("gid must stay inherited, got %+v", sc)
	}
}

func TestSetMetadataIgnoresAtimeCtimeAndReverts(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, nil)
	writeFile(t, filepath.Join(rawDir, "f.txt"), "hello")

	obj, err := f.NewObject(ctx, "f.txt")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	po, ok := obj.(*posixObject)
	if !ok {
		t.Fatalf("NewObject returned %T, want *posixObject", obj)
	}
	sidecarPath := filepath.Join(rawDir, sidecarRel("f.txt"))

	// atime/ctime must not produce a sidecar.
	if err := po.SetMetadata(ctx, fs.Metadata{
		"atime": "2020-01-01T00:00:00Z",
		"ctime": "2021-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("SetMetadata(atime/ctime): %v", err)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Fatalf("atime/ctime must not create a sidecar (stat err=%v)", err)
	}

	// A real override must be persisted sparsely.
	if err := po.SetMetadata(ctx, fs.Metadata{"mode": "0750"}); err != nil {
		t.Fatalf("SetMetadata(mode): %v", err)
	}
	sc := readSidecar(t, rawDir, "f.txt")
	if sc.Mode != "0750" {
		t.Fatalf("sidecar mode = %q, want 0750", sc.Mode)
	}
	if sc.UID != nil || sc.GID != nil {
		t.Fatalf("uid/gid should stay sparse, got %+v", sc)
	}
	meta, err := po.Metadata(ctx)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if meta["mode"] != "0750" {
		t.Fatalf("resolved mode = %q, want 0750", meta["mode"])
	}

	// Reverting to the default must delete the sidecar.
	if err := po.SetMetadata(ctx, fs.Metadata{"mode": "0644"}); err != nil {
		t.Fatalf("SetMetadata(revert): %v", err)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Fatalf("sidecar must be deleted after reverting mode (stat err=%v)", err)
	}
}

func TestPutRejectsSidecarPath(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, nil)

	src := object.NewStaticObjectInfo(sidecarRel("x.txt"), time.Now(), 1, true, nil, f.wrapped)
	_, err := f.Put(ctx, strings.NewReader("x"), src)
	if err == nil {
		t.Fatal("expected Put to reject a sidecar path")
	}
	if !strings.Contains(err.Error(), "cannot write directly to sidecar path") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(rawDir, sidecarRel("x.txt"))); !os.IsNotExist(statErr) {
		t.Fatalf("sidecar path must not be written (stat err=%v)", statErr)
	}
}

func TestListHidesSidecars(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "a.txt"), "hello")
	writeFile(t, filepath.Join(rawDir, sidecarRel("a.txt")), `{"v":1,"mode":"0700"}`)
	writeFile(t, filepath.Join(rawDir, sidecarRel("orphan")), `{"v":1,"mode":"0700"}`)

	f := newTestFs(t, rawDir, nil)

	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		names[entry.Remote()] = true
	}
	if !names["a.txt"] {
		t.Fatalf("List did not return a.txt: %v", names)
	}
	for name := range names {
		if isTestSidecar(name) {
			t.Fatalf("List leaked sidecar %q", name)
		}
	}

	if _, err := f.NewObject(ctx, sidecarRel("a.txt")); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("NewObject on sidecar = %v, want fs.ErrorObjectNotFound", err)
	}
}

func TestListWithHideSidecarsDisabled(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "a.txt"), "hello")
	writeFile(t, filepath.Join(rawDir, sidecarRel("a.txt")), `{"v":1,"mode":"0700"}`)

	f := newTestFs(t, rawDir, map[string]string{"hide_sidecars": "false"})

	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Remote() == sidecarRel("a.txt") {
			found = true
		}
	}
	if !found {
		t.Fatal("sidecar should be listed when hide_sidecars=false")
	}
	if _, err := f.NewObject(ctx, sidecarRel("a.txt")); err != nil {
		t.Fatalf("NewObject on sidecar with hide_sidecars=false: %v", err)
	}
}

func TestFlushAllPendingWritesSidecar(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rawDir, "d"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// A very long debounce proves the sidecar is still in memory until the
	// explicit flush, which is what CleanUp/unmount relies on.
	f := newTestFs(t, rawDir, map[string]string{"writeback_delay": "1h"})
	sidecarPath := filepath.Join(rawDir, sidecarRel("d/x.txt"))

	if err := f.scheduleSidecarUpdate(ctx, "d/x.txt", &sidecarData{
		Version: sidecarSchemaVersion,
		Mode:    "0700",
	}); err != nil {
		t.Fatalf("scheduleSidecarUpdate: %v", err)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Fatalf("sidecar must not be written before flush (stat err=%v)", err)
	}

	if err := f.flushAllPending(ctx); err != nil {
		t.Fatalf("flushAllPending: %v", err)
	}
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("sidecar missing after flush: %v", err)
	}
	sc := readSidecar(t, rawDir, "d/x.txt")
	if sc.Mode != "0700" {
		t.Fatalf("sidecar mode = %q, want 0700", sc.Mode)
	}
}

func TestScheduleNilDeletesSidecar(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, nil)
	writeSidecar(t, rawDir, "x.txt", &sidecarData{
		Version: sidecarSchemaVersion,
		Mode:    "0700",
	})

	if err := f.scheduleSidecarUpdate(ctx, "x.txt", nil); err != nil {
		t.Fatalf("scheduleSidecarUpdate(nil): %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("x.txt"))); !os.IsNotExist(err) {
		t.Fatalf("sidecar must be deleted when scheduling nil (stat err=%v)", err)
	}
}

func TestMoveMovesSidecar(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "f.txt"), "hello")
	writeSidecar(t, rawDir, "f.txt", &sidecarData{
		Version: sidecarSchemaVersion,
		Mode:    "0700",
	})
	f := newTestFs(t, rawDir, nil)

	obj, err := f.NewObject(ctx, "f.txt")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if _, err := f.Move(ctx, obj, "g.txt"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("f.txt"))); !os.IsNotExist(err) {
		t.Fatalf("source sidecar must be moved away (stat err=%v)", err)
	}
	sc := readSidecar(t, rawDir, "g.txt")
	if sc.Mode != "0700" {
		t.Fatalf("destination sidecar mode = %q, want 0700", sc.Mode)
	}
}

func TestDirMoveMovesDirectorySidecar(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "d1", "f.txt"), "hello")
	writeSidecar(t, rawDir, "d1", &sidecarData{
		Version: sidecarSchemaVersion,
		UID:     uint32Ptr(2001),
	})
	f := newTestFs(t, rawDir, nil)

	if err := f.DirMove(ctx, f, "d1", "d2"); err != nil {
		t.Fatalf("DirMove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "d2", "f.txt")); err != nil {
		t.Fatalf("directory content missing after DirMove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("d1"))); !os.IsNotExist(err) {
		t.Fatalf("source directory sidecar must be moved away (stat err=%v)", err)
	}
	sc := readSidecar(t, rawDir, "d2")
	if sc.UID == nil || *sc.UID != 2001 {
		t.Fatalf("destination directory sidecar uid = %+v, want 2001", sc.UID)
	}
}

func TestPurgeRemovesSidecars(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "d", "f.txt"), "hello")
	writeSidecar(t, rawDir, "d", &sidecarData{Version: sidecarSchemaVersion, UID: uint32Ptr(2001)})
	writeSidecar(t, rawDir, "d/f.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	f := newTestFs(t, rawDir, nil)

	err := f.Purge(ctx, "d")
	if errors.Is(err, fs.ErrorCantPurge) {
		t.Skip("wrapped backend does not implement Purge")
	}
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "d")); !os.IsNotExist(err) {
		t.Fatalf("purged directory still exists (stat err=%v)", err)
	}
}

func TestRmdirRemovesDirectorySidecar(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rawDir, "d"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeSidecar(t, rawDir, "d", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	f := newTestFs(t, rawDir, nil)

	if err := f.Rmdir(ctx, "d"); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "d")); !os.IsNotExist(err) {
		t.Fatalf("directory still exists (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("d"))); !os.IsNotExist(err) {
		t.Fatalf("directory sidecar still exists (stat err=%v)", err)
	}
}

func TestNewFsFileRootReturnsParent(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "f.txt"), "hello")

	fsi, err := NewFs(ctx, "test", "f.txt", testConfig(rawDir, nil))
	if !errors.Is(err, fs.ErrorIsFile) {
		t.Fatalf("NewFs(file root) error = %v, want fs.ErrorIsFile", err)
	}
	f, ok := fsi.(*Fs)
	if !ok {
		t.Fatalf("NewFs returned %T, want *Fs", fsi)
	}
	if f.Root() != "" {
		t.Fatalf("Root() = %q, want parent root %q", f.Root(), "")
	}
	if f.wrapped.Root() != rawDir {
		t.Fatalf("wrapped Root() = %q, want %q", f.wrapped.Root(), rawDir)
	}
	if _, err := f.NewObject(ctx, "f.txt"); err != nil {
		t.Fatalf("NewObject(f.txt) on parent-rooted fs: %v", err)
	}
}

func TestSubpathInheritsOwnerFromSidecarOutsideRoot(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "proj", "inh.txt"), "hello")
	writeSidecar(t, rawDir, "proj", &sidecarData{
		Version: sidecarSchemaVersion,
		UID:     uint32Ptr(2001),
		GID:     uint32Ptr(2002),
	})
	f := newTestFsAt(t, rawDir, "proj", nil)

	res, err := f.resolve(ctx, "inh.txt", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.hasUID || res.uid != 2001 {
		t.Fatalf("inherited uid = %v (has=%v), want 2001", res.uid, res.hasUID)
	}
	if !res.hasGID || res.gid != 2002 {
		t.Fatalf("inherited gid = %v (has=%v), want 2002", res.gid, res.hasGID)
	}
}

func TestRmdirAtRootRemovesSidecarOutsideRoot(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rawDir, "d"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeSidecar(t, rawDir, "d", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	f := newTestFsAt(t, rawDir, "d", nil)

	if err := f.Rmdir(ctx, ""); err != nil {
		t.Fatalf("Rmdir(root): %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "d")); !os.IsNotExist(err) {
		t.Fatalf("root directory still exists (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("d"))); !os.IsNotExist(err) {
		t.Fatalf("root directory sidecar still exists (stat err=%v)", err)
	}
}

func TestCacheMaxEntriesIsEnforced(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{
		"cache_ttl":         "5m",
		"cache_max_entries": "8",
		"cache_max_size":    "0",
	})

	for i := 0; i < 100; i++ {
		if _, err := f.getSidecarAbs(ctx, fmt.Sprintf("dir%02d/file%03d.txt", i%5, i)); err != nil {
			t.Fatalf("getSidecarAbs(%d): %v", i, err)
		}
	}

	f.mu.Lock()
	entries, lruLen := len(f.cache), f.cacheLRU.Len()
	f.mu.Unlock()
	if entries > 8 {
		t.Fatalf("cache holds %d entries, want <= 8", entries)
	}
	if entries != lruLen {
		t.Fatalf("cache map has %d entries but LRU list has %d", entries, lruLen)
	}
	assertCacheConsistent(t, f)
}

func TestCacheKeepsRecentlyUsedEntries(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{
		"cache_ttl":         "5m",
		"cache_max_entries": "3",
		"cache_max_size":    "0",
	})
	mode := func(s string) *sidecarData { return &sidecarData{Version: sidecarSchemaVersion, Mode: s} }

	f.putCache("hot.txt", mode("0700"))
	f.putCache("a.txt", mode("0701"))
	f.putCache("b.txt", mode("0702"))
	// Re-reading the hot entry must protect it from the next eviction.
	if _, err := f.getSidecarAbs(ctx, "hot.txt"); err != nil {
		t.Fatalf("getSidecarAbs(hot.txt): %v", err)
	}
	f.putCache("c.txt", mode("0703"))

	f.mu.Lock()
	_, hotOK := f.cache["hot.txt"]
	_, aOK := f.cache["a.txt"]
	_, bOK := f.cache["b.txt"]
	_, cOK := f.cache["c.txt"]
	f.mu.Unlock()
	if !hotOK {
		t.Fatal("recently used entry hot.txt was evicted")
	}
	if aOK {
		t.Fatal("least recently used entry a.txt should have been evicted")
	}
	if !bOK || !cOK {
		t.Fatal("expected b.txt and c.txt to remain cached")
	}
	assertCacheConsistent(t, f)
}

func TestCacheDropsExpiredEntriesOnAccess(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeSidecar(t, rawDir, "e.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	f := newTestFs(t, rawDir, map[string]string{
		"cache_ttl":         "5ms",
		"cache_max_entries": "0",
		"cache_max_size":    "0",
	})

	if _, err := f.getSidecarAbs(ctx, "e.txt"); err != nil {
		t.Fatalf("getSidecarAbs: %v", err)
	}
	if err := os.Remove(filepath.Join(rawDir, sidecarRel("e.txt"))); err != nil {
		t.Fatalf("Remove sidecar: %v", err)
	}
	time.Sleep(25 * time.Millisecond)

	sc, err := f.getSidecarAbs(ctx, "e.txt")
	if err != nil {
		t.Fatalf("getSidecarAbs after expiry: %v", err)
	}
	if sc != nil {
		t.Fatalf("expired cache entry was served: %+v", sc)
	}

	f.mu.Lock()
	entries, bytes, lruLen := len(f.cache), f.cacheBytes, f.cacheLRU.Len()
	f.mu.Unlock()
	if entries != 1 || lruLen != 1 {
		t.Fatalf("expired entry was not replaced: entries=%d lru=%d", entries, lruLen)
	}
	want := cacheEntrySize("e.txt", nil)
	if bytes != want {
		t.Fatalf("cacheBytes = %d, want %d", bytes, want)
	}
	assertCacheConsistent(t, f)
}

func TestCacheByteBudget(t *testing.T) {
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{
		"cache_ttl":         "5m",
		"cache_max_entries": "0",
		"cache_max_size":    "2k",
	})
	target := strings.Repeat("t", 1500)
	for i := 0; i < 10; i++ {
		f.putCache(fmt.Sprintf("link%02d", i), &sidecarData{Version: sidecarSchemaVersion, Symlink: target})
	}

	f.mu.Lock()
	entries, bytes := len(f.cache), f.cacheBytes
	f.mu.Unlock()
	if bytes > 2048 {
		t.Fatalf("cacheBytes = %d, want <= 2048", bytes)
	}
	if entries == 0 {
		t.Fatal("byte budget evicted every entry, including the one just inserted")
	}
	assertCacheConsistent(t, f)
}

func TestPurgeAtRootRemovesSidecarOutsideRoot(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "d", "f.txt"), "hello")
	writeSidecar(t, rawDir, "d", &sidecarData{Version: sidecarSchemaVersion, UID: uint32Ptr(2001)})
	f := newTestFsAt(t, rawDir, "d", nil)

	err := f.Purge(ctx, "")
	if errors.Is(err, fs.ErrorCantPurge) {
		t.Skip("wrapped backend cannot purge its own root")
	}
	if err != nil {
		t.Fatalf("Purge(root): %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "d")); !os.IsNotExist(err) {
		t.Fatalf("root directory still exists (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("d"))); !os.IsNotExist(err) {
		t.Fatalf("root directory sidecar still exists (stat err=%v)", err)
	}
}

// assertCacheConsistent verifies that the cache map, LRU list and byte counter
// agree, which is what makes eviction and invalidation safe.
func assertCacheConsistent(t *testing.T, f *Fs) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cache) != f.cacheLRU.Len() {
		t.Fatalf("cache map has %d entries but LRU list has %d", len(f.cache), f.cacheLRU.Len())
	}
	var recomputed int64
	for key, ce := range f.cache {
		if ce.elem == nil {
			t.Fatalf("cache entry %q has no LRU element", key)
		}
		if got, _ := ce.elem.Value.(string); got != key {
			t.Fatalf("cache entry %q is linked to LRU item %q", key, got)
		}
		recomputed += ce.size
	}
	if recomputed != f.cacheBytes {
		t.Fatalf("cacheBytes = %d, want %d", f.cacheBytes, recomputed)
	}
}

func TestCacheInvalidatePrefixFreesBudget(t *testing.T) {
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{
		"cache_ttl":         "5m",
		"cache_max_entries": "0",
		"cache_max_size":    "0",
	})
	f.putCache("d/a.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	f.putCache("d/b.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0701"})
	f.putCache("e/c.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0702"})

	f.invalidatePrefix("d")
	f.mu.Lock()
	entries, bytes, lruLen := len(f.cache), f.cacheBytes, f.cacheLRU.Len()
	f.mu.Unlock()
	if entries != 1 || lruLen != 1 {
		t.Fatalf("after invalidatePrefix(d): entries=%d lru=%d, want 1", entries, lruLen)
	}
	if want := cacheEntrySize("e/c.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0702"}); bytes != want {
		t.Fatalf("cacheBytes = %d, want %d", bytes, want)
	}
	assertCacheConsistent(t, f)

	f.invalidatePrefix("")
	f.mu.Lock()
	entries, bytes, lruLen = len(f.cache), f.cacheBytes, f.cacheLRU.Len()
	f.mu.Unlock()
	if entries != 0 || lruLen != 0 || bytes != 0 {
		t.Fatalf("after root invalidate: entries=%d lru=%d bytes=%d, want all zero", entries, lruLen, bytes)
	}
}

func TestCacheUnlimitedWhenZero(t *testing.T) {
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{
		"cache_ttl":         "5m",
		"cache_max_entries": "0",
		"cache_max_size":    "0",
	})
	for i := 0; i < 200; i++ {
		f.putCache(fmt.Sprintf("f%03d", i), &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	}
	f.mu.Lock()
	entries := len(f.cache)
	f.mu.Unlock()
	if entries != 200 {
		t.Fatalf("unbounded cache holds %d entries, want 200", entries)
	}
	assertCacheConsistent(t, f)
}

func TestCacheStatsCommand(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{
		"cache_ttl":         "5m",
		"cache_max_entries": "4",
		"cache_max_size":    "1M",
	})
	f.putCache("a.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
	if _, err := f.getSidecarAbs(ctx, "a.txt"); err != nil {
		t.Fatalf("getSidecarAbs: %v", err)
	}

	res, err := f.Command(ctx, "cachestats", nil, nil)
	if err != nil {
		t.Fatalf("Command(cachestats): %v", err)
	}
	stats, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("Command(cachestats) returned %T, want map[string]any", res)
	}
	for _, key := range []string{"entries", "bytes", "max_entries", "max_bytes", "hits", "misses", "evictions", "ttl", "pending_writes"} {
		if _, ok := stats[key]; !ok {
			t.Fatalf("cachestats missing %q key: %#v", key, stats)
		}
	}
	if got := stats["entries"]; got != 1 {
		t.Fatalf("cachestats entries = %v, want 1", got)
	}
	if got := stats["hits"]; got != uint64(1) {
		t.Fatalf("cachestats hits = %v, want 1", got)
	}
	if got := stats["max_entries"]; got != 4 {
		t.Fatalf("cachestats max_entries = %v, want 4", got)
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, map[string]string{
		"cache_ttl":         "5m",
		"cache_max_entries": "16",
		"cache_max_size":    "8k",
	})

	const workers = 8
	const iterations = 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				key := fmt.Sprintf("d%d/f%d.txt", w, i%32)
				switch i % 4 {
				case 0:
					f.putCache(key, &sidecarData{Version: sidecarSchemaVersion, Mode: "0700"})
				case 1:
					if _, err := f.getSidecarAbs(ctx, key); err != nil {
						t.Errorf("getSidecarAbs(%q): %v", key, err)
						return
					}
				case 2:
					f.invalidateCacheAbs(key)
				case 3:
					f.invalidatePrefix(fmt.Sprintf("d%d", w))
				}
			}
		}(w)
	}
	wg.Wait()

	f.mu.Lock()
	entries := len(f.cache)
	bytes := f.cacheBytes
	f.mu.Unlock()
	if entries > 16 {
		t.Fatalf("cache holds %d entries after concurrent access, want <= 16", entries)
	}
	if bytes > 8192+2048 {
		t.Fatalf("cacheBytes = %d after concurrent access, want <= 10240", bytes)
	}
	assertCacheConsistent(t, f)
}

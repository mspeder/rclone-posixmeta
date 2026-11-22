package posixmeta

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
		"sidecar_suffix":       ".posixmeta",
		"hide_sidecars":        "true",
		"cache_ttl":            "0",
		"writeback_delay":      "0",
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
	t.Cleanup(func() { _ = f.CleanUp(context.Background()) })
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
	writeFile(t, filepath.Join(rawDir, remote+defaultSuffix), string(payload))
}

func readSidecar(t *testing.T, rawDir, remote string) sidecarData {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(rawDir, remote+defaultSuffix))
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
		m["sidecar_suffix"] = "bad/suffix"
		if _, err := NewFs(context.Background(), "test", "", m); err == nil {
			t.Fatal("expected error for sidecar_suffix containing '/'")
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
	sidecarPath := filepath.Join(rawDir, "f.txt"+defaultSuffix)

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

	src := object.NewStaticObjectInfo("x.txt"+defaultSuffix, time.Now(), 1, true, nil, f.wrapped)
	_, err := f.Put(ctx, strings.NewReader("x"), src)
	if err == nil {
		t.Fatal("expected Put to reject a sidecar path")
	}
	if !strings.Contains(err.Error(), "cannot write directly to sidecar path") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(rawDir, "x.txt"+defaultSuffix)); !os.IsNotExist(statErr) {
		t.Fatalf("sidecar path must not be written (stat err=%v)", statErr)
	}
}

func TestListHidesSidecars(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "a.txt"), "hello")
	writeFile(t, filepath.Join(rawDir, "a.txt"+defaultSuffix), `{"v":1,"mode":"0700"}`)
	writeFile(t, filepath.Join(rawDir, "orphan"+defaultSuffix), `{"v":1,"mode":"0700"}`)

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
		if strings.HasSuffix(name, defaultSuffix) {
			t.Fatalf("List leaked sidecar %q", name)
		}
	}

	if _, err := f.NewObject(ctx, "a.txt"+defaultSuffix); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("NewObject on sidecar = %v, want fs.ErrorObjectNotFound", err)
	}
}

func TestListWithHideSidecarsDisabled(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "a.txt"), "hello")
	writeFile(t, filepath.Join(rawDir, "a.txt"+defaultSuffix), `{"v":1,"mode":"0700"}`)

	f := newTestFs(t, rawDir, map[string]string{"hide_sidecars": "false"})

	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Remote() == "a.txt"+defaultSuffix {
			found = true
		}
	}
	if !found {
		t.Fatal("sidecar should be listed when hide_sidecars=false")
	}
	if _, err := f.NewObject(ctx, "a.txt"+defaultSuffix); err != nil {
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
	sidecarPath := filepath.Join(rawDir, "d", "x.txt"+defaultSuffix)

	if err := f.scheduleSidecarUpdate(ctx, "d/x.txt", false, &sidecarData{
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

	if err := f.scheduleSidecarUpdate(ctx, "x.txt", false, nil); err != nil {
		t.Fatalf("scheduleSidecarUpdate(nil): %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "x.txt"+defaultSuffix)); !os.IsNotExist(err) {
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
	if _, err := os.Stat(filepath.Join(rawDir, "f.txt"+defaultSuffix)); !os.IsNotExist(err) {
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
	if _, err := os.Stat(filepath.Join(rawDir, "d1"+defaultSuffix)); !os.IsNotExist(err) {
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
	if _, err := os.Stat(filepath.Join(rawDir, "d"+defaultSuffix)); !os.IsNotExist(err) {
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
	if _, err := os.Stat(filepath.Join(rawDir, "d"+defaultSuffix)); !os.IsNotExist(err) {
		t.Fatalf("root directory sidecar still exists (stat err=%v)", err)
	}
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
	if _, err := os.Stat(filepath.Join(rawDir, "d"+defaultSuffix)); !os.IsNotExist(err) {
		t.Fatalf("root directory sidecar still exists (stat err=%v)", err)
	}
}

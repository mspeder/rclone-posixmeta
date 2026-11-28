package posixmeta

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

// putLinkForTest uploads a `.rclonelink` object with target as payload.
func putLinkForTest(t *testing.T, f *Fs, remote, target string) fs.Object {
	t.Helper()
	src := object.NewStaticObjectInfo(remote, time.Now(), int64(len(target)), true, nil, f.wrapped)
	obj, err := f.Put(context.Background(), strings.NewReader(target), src)
	if err != nil {
		t.Fatalf("Put(%q): %v", remote, err)
	}
	return obj
}

// readObjectForTest returns the full payload of obj.
func readObjectForTest(t *testing.T, obj fs.Object) string {
	t.Helper()
	rc, err := obj.Open(context.Background())
	if err != nil {
		t.Fatalf("Open(%q): %v", obj.Remote(), err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll(%q): %v", obj.Remote(), err)
	}
	return string(raw)
}

func proxyLinkTestFs(t *testing.T, extra map[string]string) (*Fs, string) {
	t.Helper()
	rawDir := t.TempDir()
	if extra == nil {
		extra = map[string]string{}
	}
	extra["proxy_links"] = "true"
	return newTestFs(t, rawDir, extra), rawDir
}

func TestProxyLinkStoresOnlySidecar(t *testing.T) {
	ctx := context.Background()
	f, rawDir := proxyLinkTestFs(t, nil)
	const target = "dir/target.txt"

	obj := putLinkForTest(t, f, "link.txt.rclonelink", target)
	if obj.Remote() != "link.txt.rclonelink" {
		t.Fatalf("remote = %q", obj.Remote())
	}
	if obj.Size() != int64(len(target)) {
		t.Fatalf("size = %d, want %d", obj.Size(), len(target))
	}
	if got := readObjectForTest(t, obj); got != target {
		t.Fatalf("payload = %q, want %q", got, target)
	}

	// Exactly one object at rest: the sidecar. No `.rclonelink` payload.
	files, err := os.ReadDir(rawDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(files) != 1 || files[0].Name() != sidecarRel("link.txt") {
		names := make([]string, 0, len(files))
		for _, file := range files {
			names = append(names, file.Name())
		}
		t.Fatalf("objects at rest = %v, want [%q]", names, sidecarRel("link.txt"))
	}
	if _, err := os.Stat(filepath.Join(rawDir, "link.txt.rclonelink")); !os.IsNotExist(err) {
		t.Fatalf("link payload must not exist (stat err=%v)", err)
	}

	sc := readSidecar(t, rawDir, "link.txt")
	if sc.Symlink != target {
		t.Fatalf("sidecar symlink = %q, want %q", sc.Symlink, target)
	}
	if sc.Mode != "" || sc.UID != nil || sc.GID != nil {
		t.Fatalf("sidecar must stay sparse: %+v", sc)
	}

	// NewObject serves the link from the sidecar.
	o, err := f.NewObject(ctx, "link.txt.rclonelink")
	if err != nil {
		t.Fatalf("NewObject(link): %v", err)
	}
	if got := readObjectForTest(t, o); got != target {
		t.Fatalf("NewObject payload = %q, want %q", got, target)
	}
	// The visible name has no payload, so it stays absent.
	if _, err := f.NewObject(ctx, "link.txt"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("NewObject(link.txt) = %v, want fs.ErrorObjectNotFound", err)
	}

	// A listing publishes the link and hides the sidecar.
	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Remote() != "link.txt.rclonelink" {
		remotes := make([]string, 0, len(entries))
		for _, entry := range entries {
			remotes = append(remotes, entry.Remote())
		}
		t.Fatalf("List = %v, want [link.txt.rclonelink]", remotes)
	}
}

func TestProxyLinkRejectsInvalidTargets(t *testing.T) {
	f, rawDir := proxyLinkTestFs(t, nil)
	for name, target := range map[string]string{
		"empty":    "",
		"nul":      "a\x00b",
		"too long": strings.Repeat("x", maxLinkTargetSize+1),
	} {
		t.Run(name, func(t *testing.T) {
			src := object.NewStaticObjectInfo("bad.rclonelink", time.Now(), int64(len(target)), true, nil, f.wrapped)
			if _, err := f.Put(context.Background(), strings.NewReader(target), src); err == nil {
				t.Fatalf("Put(%s target) succeeded, want error", name)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("bad"))); !os.IsNotExist(err) {
		t.Fatalf("rejected link must not leave a sidecar (stat err=%v)", err)
	}
}

func TestProxyLinkRemoveDeletesSidecar(t *testing.T) {
	ctx := context.Background()
	f, rawDir := proxyLinkTestFs(t, nil)
	obj := putLinkForTest(t, f, "link.txt.rclonelink", "target.txt")

	if err := obj.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("link.txt"))); !os.IsNotExist(err) {
		t.Fatalf("sidecar survived Remove (stat err=%v)", err)
	}
	if _, err := f.NewObject(ctx, "link.txt.rclonelink"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("NewObject after Remove = %v, want fs.ErrorObjectNotFound", err)
	}
}

func TestProxyLinkMoveMovesSidecar(t *testing.T) {
	ctx := context.Background()
	f, rawDir := proxyLinkTestFs(t, nil)
	obj := putLinkForTest(t, f, "d/link.txt.rclonelink", "../target.txt")

	moved, err := f.Move(ctx, obj, "d/renamed.rclonelink")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if moved.Remote() != "d/renamed.rclonelink" {
		t.Fatalf("moved remote = %q", moved.Remote())
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("d/link.txt"))); !os.IsNotExist(err) {
		t.Fatalf("source sidecar must be moved away (stat err=%v)", err)
	}
	if sc := readSidecar(t, rawDir, "d/renamed"); sc.Symlink != "../target.txt" {
		t.Fatalf("renamed sidecar symlink = %q", sc.Symlink)
	}
	if got := readObjectForTest(t, moved); got != "../target.txt" {
		t.Fatalf("moved target = %q", got)
	}
}

func TestProxyLinkCopyKeepsLink(t *testing.T) {
	ctx := context.Background()
	f, rawDir := proxyLinkTestFs(t, nil)
	obj := putLinkForTest(t, f, "link.txt.rclonelink", "target.txt")

	copied, err := f.Copy(ctx, obj, "copy.txt.rclonelink")
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := readObjectForTest(t, copied); got != "target.txt" {
		t.Fatalf("copied target = %q", got)
	}
	if sc := readSidecar(t, rawDir, "copy.txt"); sc.Symlink != "target.txt" {
		t.Fatalf("copied sidecar symlink = %q", sc.Symlink)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "copy.txt.rclonelink")); !os.IsNotExist(err) {
		t.Fatalf("copy must not upload a payload (stat err=%v)", err)
	}
}

func TestProxyLinkShadowsLegacyPayload(t *testing.T) {
	ctx := context.Background()
	f, rawDir := proxyLinkTestFs(t, nil)
	// A real `.rclonelink` payload from before proxy_links was enabled.
	writeFile(t, filepath.Join(rawDir, "legacy.txt.rclonelink"), "old-target")
	writeSidecar(t, rawDir, "legacy.txt", &sidecarData{
		Version: sidecarSchemaVersion,
		Symlink: "new-target",
	})

	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	links := 0
	for _, entry := range entries {
		if entry.Remote() == "legacy.txt.rclonelink" {
			links++
			if got := readObjectForTest(t, entry.(fs.Object)); got != "new-target" {
				t.Fatalf("listed link target = %q, want new-target", got)
			}
		}
	}
	if links != 1 {
		t.Fatalf("List returned %d legacy links, want 1", links)
	}

	obj, err := f.NewObject(ctx, "legacy.txt.rclonelink")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if got := readObjectForTest(t, obj); got != "new-target" {
		t.Fatalf("NewObject target = %q, want new-target", got)
	}
}

func TestProxyLinkDisabledUploadsPayload(t *testing.T) {
	rawDir := t.TempDir()
	f := newTestFs(t, rawDir, nil)
	putLinkForTest(t, f, "link.txt.rclonelink", "target.txt")
	if _, err := os.Stat(filepath.Join(rawDir, "link.txt.rclonelink")); err != nil {
		t.Fatalf("payload missing with proxy_links=false: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, sidecarRel("link.txt"))); !os.IsNotExist(err) {
		t.Fatalf("proxy_links=false must not write a link sidecar (stat err=%v)", err)
	}
}

func TestProxyLinkSetMetaCreatesLink(t *testing.T) {
	ctx := context.Background()
	f, rawDir := proxyLinkTestFs(t, nil)

	if _, err := f.Command(ctx, "setmeta", []string{"link.txt.rclonelink"}, map[string]string{"symlink": "target.txt"}); err != nil {
		t.Fatalf("setmeta: %v", err)
	}
	sc := readSidecar(t, rawDir, "link.txt")
	if sc.Symlink != "target.txt" {
		t.Fatalf("sidecar symlink = %q", sc.Symlink)
	}
	obj, err := f.NewObject(ctx, "link.txt.rclonelink")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if got := readObjectForTest(t, obj); got != "target.txt" {
		t.Fatalf("target = %q", got)
	}
}

func TestProxyLinkNewFsFileRoot(t *testing.T) {
	ctx := context.Background()
	f, rawDir := proxyLinkTestFs(t, nil)
	putLinkForTest(t, f, "sub/link.txt.rclonelink", "target.txt")

	fsi, err := NewFs(ctx, "test", "sub/link.txt.rclonelink", testConfig(rawDir, map[string]string{"proxy_links": "true"}))
	if !errors.Is(err, fs.ErrorIsFile) {
		t.Fatalf("NewFs(link root) error = %v, want fs.ErrorIsFile", err)
	}
	if got := fsi.Root(); got != "sub" {
		t.Fatalf("root = %q, want sub", got)
	}
	obj, err := fsi.NewObject(ctx, "link.txt.rclonelink")
	if err != nil {
		t.Fatalf("NewObject at file root: %v", err)
	}
	if got := readObjectForTest(t, obj); got != "target.txt" {
		t.Fatalf("target at file root = %q", got)
	}

	if _, err := NewFs(ctx, "test", "missing.rclonelink", testConfig(rawDir, map[string]string{"proxy_links": "true"})); err != nil {
		t.Fatalf("NewFs(missing link root): %v", err)
	}
}

func TestProxyLinkListR(t *testing.T) {
	ctx := context.Background()
	f, rawDir := proxyLinkTestFs(t, nil)
	putLinkForTest(t, f, "d/link.txt.rclonelink", "target.txt")
	writeFile(t, filepath.Join(rawDir, "d", "plain.txt"), "data")

	var seen []string
	if err := f.ListR(ctx, "", func(entries fs.DirEntries) error {
		for _, entry := range entries {
			seen = append(seen, entry.Remote())
		}
		return nil
	}); err != nil {
		t.Fatalf("ListR: %v", err)
	}
	want := map[string]bool{"d": true, "d/plain.txt": true, "d/link.txt.rclonelink": true}
	for _, remote := range seen {
		if isTestSidecar(remote) {
			t.Fatalf("ListR leaked sidecar %q", remote)
		}
		delete(want, remote)
	}
	if len(want) != 0 {
		t.Fatalf("ListR missed entries %v (saw %v)", want, seen)
	}
}

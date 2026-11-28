package posixmeta

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
)

// TestVFSChmodAPIIsUnchanged checks the fork leaves the public VFS chmod/chown
// API exactly as upstream, even for a backend which implements
// fs.SetMetadataer (`local` applies mode/uid/gid with chmod/chown).
//
// `rclone serve nfs` calls VFS.Chmod/VFS.Chown and relies on them returning
// ENOSYS; the mount path does not use them (`cmd/mount` calls SetMetadata on
// the node). This locks in that the fork does not make chmod effective for
// other VFS users.
func TestVFSChmodAPIIsUnchanged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	f, err := fs.NewFs(ctx, root)
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	v := vfs.New(ctx, f, nil)
	t.Cleanup(func() {
		_ = v.CleanUp()
		v.Shutdown()
	})

	h, err := v.Create("gate.txt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := h.Write([]byte("payload\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(root, "gate.txt")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat backing file: %v", err)
	}

	if err := v.Chmod("gate.txt", 0o600); err != vfs.ENOSYS {
		t.Fatalf("Chmod = %v, want ENOSYS", err)
	}
	if err := v.Chown("gate.txt", -1, -1); err != vfs.ENOSYS {
		t.Fatalf("Chown = %v, want ENOSYS", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat backing file: %v", err)
	}
	if after.Mode() != before.Mode() {
		t.Fatalf("chmod leaked to the backend: %v -> %v", before.Mode(), after.Mode())
	}

	// A missing entry keeps upstream's ENOENT-style error, not ENOSYS.
	if err := v.Chmod("missing.txt", 0o600); err == nil || err == vfs.ENOSYS {
		t.Fatalf("Chmod of a missing entry = %v, want not found", err)
	}
}

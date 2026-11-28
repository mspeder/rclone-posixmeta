package posixmeta

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

// dirLayoutFs builds a posixmeta Fs using the per-directory map layout.
func dirLayoutFs(t *testing.T, extra map[string]string) (*Fs, string) {
	t.Helper()
	rawDir := t.TempDir()
	if extra == nil {
		extra = map[string]string{}
	}
	extra["sidecar_layout"] = layoutDir
	return newTestFs(t, rawDir, extra), rawDir
}

// putFileForTest uploads a regular file through the Fs.
func putFileForTest(t *testing.T, f *Fs, remote, content string) fs.Object {
	t.Helper()
	src := object.NewStaticObjectInfo(remote, time.Now(), int64(len(content)), true, nil, f.wrapped)
	obj, err := f.Put(context.Background(), strings.NewReader(content), src)
	if err != nil {
		t.Fatalf("Put(%q): %v", remote, err)
	}
	return obj
}

// setmetaForTest applies POSIX metadata through the backend command, which
// flushes synchronously.
func setmetaForTest(t *testing.T, f *Fs, target string, kv ...string) {
	t.Helper()
	opt := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		opt[kv[i]] = kv[i+1]
	}
	if _, err := f.Command(context.Background(), "setmeta", []string{target}, opt); err != nil {
		t.Fatalf("setmeta %q %v: %v", target, opt, err)
	}
}

// readMapDoc parses the map document of dir, failing if it does not exist.
func readMapDoc(t *testing.T, rawDir, dir string) sidecarMap {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(rawDir, dir, defaultSidecarPrefix))
	if err != nil {
		t.Fatalf("read map of %q: %v", dir, err)
	}
	var doc sidecarMap
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("parse map of %q: %v", dir, err)
	}
	return doc
}

// mapFiles lists every metadata object under root, relative to root.
func mapFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base == defaultSidecarPrefix || strings.HasPrefix(base, defaultSidecarPrefix+".") {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", root, err)
	}
	return out
}

func mapKeys(doc sidecarMap) []string {
	keys := make([]string, 0, len(doc.Entries))
	for k := range doc.Entries {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(in []string) {
	sort.Strings(in)
}

func requireKeys(t *testing.T, doc sidecarMap, want ...string) {
	t.Helper()
	got := mapKeys(doc)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("map keys = %v, want %v", got, want)
	}
}

func metadataForTest(t *testing.T, f *Fs, remote string) fs.Metadata {
	t.Helper()
	obj, err := f.NewObject(context.Background(), remote)
	if err != nil {
		t.Fatalf("NewObject(%q): %v", remote, err)
	}
	meta, err := obj.(fs.Metadataer).Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata(%q): %v", remote, err)
	}
	return meta
}

func TestDirLayoutStoresOneMapPerDirectory(t *testing.T) {
	ctx := context.Background()
	f, rawDir := dirLayoutFs(t, nil)
	putFileForTest(t, f, "a.txt", "a")
	putFileForTest(t, f, "dir/b.txt", "b")
	setmetaForTest(t, f, "a.txt", "mode", "0700")
	setmetaForTest(t, f, "dir/b.txt", "mode", "0600")
	setmetaForTest(t, f, "dir", "uid", "2001", "gid", "2002")

	rootDoc := readMapDoc(t, rawDir, "")
	if rootDoc.Version != mapSchemaVersion {
		t.Fatalf("root map = %+v, want schema v%d", rootDoc, mapSchemaVersion)
	}
	requireKeys(t, rootDoc, "a.txt", "dir")
	if got := rootDoc.Entries["a.txt"].Mode; got != "0700" {
		t.Fatalf("a.txt mode = %q, want 0700", got)
	}
	if uid := rootDoc.Entries["dir"].UID; uid == nil || *uid != 2001 {
		t.Fatalf("dir uid = %v, want 2001", uid)
	}
	requireKeys(t, readMapDoc(t, rawDir, "dir"), "b.txt")

	// One aggregate object per directory and no per-entry sidecars.
	for _, name := range mapFiles(t, rawDir) {
		if strings.HasPrefix(filepath.Base(name), defaultSidecarPrefix+".") {
			t.Fatalf("unexpected per-entry sidecar %q", name)
		}
	}

	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List returned %d entries, want 2 (map must be hidden)", len(entries))
	}
	if meta := metadataForTest(t, f, "a.txt"); meta["mode"] != "0700" {
		t.Fatalf("a.txt metadata = %v", meta)
	}
	child := metadataForTest(t, f, "dir/b.txt")
	if child["mode"] != "0600" || child["uid"] != "2001" || child["gid"] != "2002" {
		t.Fatalf("dir/b.txt metadata = %v, want mode 0600 uid 2001 gid 2002", child)
	}
}

// The map header is minimal: the schema version is the sole layout
// discriminator (no "layout" tag) and the checksum is bare lowercase hex.
func TestDirLayoutDocumentHeaderIsMinimal(t *testing.T) {
	f, rawDir := dirLayoutFs(t, nil)
	putFileForTest(t, f, "a.txt", "a")
	setmetaForTest(t, f, "a.txt", "mode", "0700")

	payload, err := os.ReadFile(filepath.Join(rawDir, defaultSidecarPrefix))
	if err != nil {
		t.Fatalf("read map: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("parse map: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("map header keys = %v, want exactly v, crc, entries", fields)
	}
	for _, key := range []string{"v", "crc", "entries"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("map header is missing %q: %s", key, payload)
		}
	}
	if _, ok := fields["layout"]; ok {
		t.Fatalf("map header still carries a layout tag: %s", payload)
	}
	var crc string
	if err := json.Unmarshal(fields["crc"], &crc); err != nil {
		t.Fatalf("parse crc: %v", err)
	}
	if len(crc) != 8 {
		t.Fatalf("crc = %q, want 8 bare hex digits", crc)
	}
	if _, err := hex.DecodeString(crc); err != nil {
		t.Fatalf("crc %q is not hex: %v", crc, err)
	}
}

// The retired "layout" tag is ignored by the decoder, but the checksum is bare
// hex only: a legacy "crc32:" prefix is a mismatch, so such a document is
// corrupt and must be repaired.
func TestDirLayoutRetiredHeaderFields(t *testing.T) {
	const entries = `{"a.txt":{"mode":"0700"}}`
	sum := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(entries)))

	if _, err := decodeMap([]byte(`{"v":2,"crc":"` + sum + `","entries":` + entries + `}`)); err != nil {
		t.Fatalf("bare header rejected: %v", err)
	}
	if _, err := decodeMap([]byte(`{"v":2,"layout":"dir","crc":"` + sum + `","entries":` + entries + `}`)); err != nil {
		t.Fatalf("retired layout tag must be ignored: %v", err)
	}
	if _, err := decodeMap([]byte(`{"v":2,"crc":"crc32:` + sum + `","entries":` + entries + `}`)); err == nil {
		t.Fatal("legacy crc32: prefix must be rejected")
	}
}

func TestDirLayoutRootEntryUsesSelfKey(t *testing.T) {
	f, rawDir := dirLayoutFs(t, nil)
	setmetaForTest(t, f, "", "mode", "0711", "uid", "3001")

	doc := readMapDoc(t, rawDir, "")
	requireKeys(t, doc, mapSelfKey)
	if doc.Entries[mapSelfKey].Mode != "0711" {
		t.Fatalf("root entry = %+v", doc.Entries[mapSelfKey])
	}
	res, err := f.resolve(context.Background(), "", true)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if res.mode != 0o711 || res.uid != 3001 {
		t.Fatalf("root resolved = %04o uid %d, want 0711 uid 3001", res.mode, res.uid)
	}
}

func TestDirLayoutChildNeverWritesOwnSidecar(t *testing.T) {
	f, rawDir := dirLayoutFs(t, nil)
	setmetaForTest(t, f, "d", "uid", "4001", "gid", "4002")
	putFileForTest(t, f, "d/x.txt", "x")

	doc := readMapDoc(t, rawDir, "")
	requireKeys(t, doc, "d")
	// d's own map only holds entries with overrides, and x.txt inherits.
	if _, err := os.Stat(filepath.Join(rawDir, "d", defaultSidecarPrefix)); !os.IsNotExist(err) {
		t.Fatalf("d must not get an empty map: %v", err)
	}
	if files := mapFiles(t, rawDir); len(files) != 1 || files[0] != defaultSidecarPrefix {
		t.Fatalf("metadata objects = %v, want only the root map", files)
	}
	meta := metadataForTest(t, f, "d/x.txt")
	if meta["uid"] != "4001" || meta["gid"] != "4002" || meta["mode"] != "0644" {
		t.Fatalf("inherited metadata = %v", meta)
	}
}

func TestDirLayoutMoveRemoveRmdirKeepMaps(t *testing.T) {
	ctx := context.Background()
	f, rawDir := dirLayoutFs(t, nil)
	putFileForTest(t, f, "dir/a.txt", "a")
	putFileForTest(t, f, "dir/b.txt", "b")
	setmetaForTest(t, f, "dir/a.txt", "mode", "0700")
	setmetaForTest(t, f, "dir", "uid", "6001")

	obj, err := f.NewObject(ctx, "dir/a.txt")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if _, err := f.Move(ctx, obj, "dir/renamed.txt"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	requireKeys(t, readMapDoc(t, rawDir, "dir"), "renamed.txt")
	if mode := readMapDoc(t, rawDir, "dir").Entries["renamed.txt"].Mode; mode != "0700" {
		t.Fatalf("moved mode = %q", mode)
	}

	// Moving into another directory moves the key into the destination map and
	// drops the now-empty source map object.
	moved, err := f.NewObject(ctx, "dir/renamed.txt")
	if err != nil {
		t.Fatalf("NewObject moved: %v", err)
	}
	if _, err := f.Move(ctx, moved, "other/deep.txt"); err != nil {
		t.Fatalf("Move across dirs: %v", err)
	}
	requireKeys(t, readMapDoc(t, rawDir, "other"), "deep.txt")
	if mode := readMapDoc(t, rawDir, "other").Entries["deep.txt"].Mode; mode != "0700" {
		t.Fatalf("cross-dir moved mode = %q", mode)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "dir", defaultSidecarPrefix)); !os.IsNotExist(err) {
		t.Fatalf("empty source map must be removed: %v", err)
	}

	b, err := f.NewObject(ctx, "dir/b.txt")
	if err != nil {
		t.Fatalf("NewObject b: %v", err)
	}
	if err := b.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	deep, err := f.NewObject(ctx, "other/deep.txt")
	if err != nil {
		t.Fatalf("NewObject deep: %v", err)
	}
	if err := deep.Remove(ctx); err != nil {
		t.Fatalf("Remove deep: %v", err)
	}

	// Rmdir removes the directory's own map object and its parent key.
	if err := f.Rmdir(ctx, "other"); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "other")); !os.IsNotExist(err) {
		t.Fatalf("other still exists: %v", err)
	}
	requireKeys(t, readMapDoc(t, rawDir, ""), "dir")

	// Purge the remaining tree, keeping only the layout declaration.
	if err := f.Purge(ctx, "dir"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	requireKeys(t, readMapDoc(t, rawDir, ""), "")
	if files := mapFiles(t, rawDir); len(files) != 1 || files[0] != defaultSidecarPrefix {
		t.Fatalf("metadata objects after purge = %v, want only the root map", files)
	}
}

func TestDirLayoutCorruptMapFailsClosed(t *testing.T) {
	ctx := context.Background()
	f, rawDir := dirLayoutFs(t, nil)
	putFileForTest(t, f, "a.txt", "a")
	setmetaForTest(t, f, "a.txt", "mode", "0700")

	mapPath := filepath.Join(rawDir, defaultSidecarPrefix)
	if err := os.WriteFile(mapPath, []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("corrupt map: %v", err)
	}
	before, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("read map: %v", err)
	}

	if _, err := f.List(ctx, ""); err == nil {
		t.Fatal("List must fail on a corrupt map")
	}
	obj, err := f.NewObject(ctx, "a.txt")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if _, err := obj.(fs.Metadataer).Metadata(ctx); err == nil {
		t.Fatal("Metadata must fail on a corrupt map")
	}
	if err := obj.(fs.SetMetadataer).SetMetadata(ctx, fs.Metadata{"mode": "0755"}); err == nil {
		t.Fatal("SetMetadata must refuse to modify a corrupt map")
	}
	if _, err := f.Command(ctx, "setmeta", []string{"a.txt"}, map[string]string{"mode": "0755"}); err == nil {
		t.Fatal("setmeta must refuse to modify a corrupt map")
	}
	after, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("read map: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("a corrupt map must never be rewritten")
	}

	// A mount (a fresh Fs) refuses to start rather than serve wrong metadata.
	if _, err := NewFs(ctx, "test", "", testConfig(rawDir, map[string]string{"sidecar_layout": layoutDir})); err == nil {
		t.Fatal("NewFs must fail on a corrupt root map")
	}
}

func TestDirLayoutOnCorruptWarnServesDefaults(t *testing.T) {
	ctx := context.Background()
	f, rawDir := dirLayoutFs(t, map[string]string{"on_corrupt": onCorruptWarn})
	putFileForTest(t, f, "a.txt", "a")
	setmetaForTest(t, f, "a.txt", "mode", "0700")

	mapPath := filepath.Join(rawDir, defaultSidecarPrefix)
	if err := os.WriteFile(mapPath, []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("corrupt map: %v", err)
	}
	before, _ := os.ReadFile(mapPath)

	if meta := metadataForTest(t, f, "a.txt"); meta["mode"] != "0644" {
		t.Fatalf("metadata = %v, want the default mode", meta)
	}
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatalf("List with on_corrupt=warn: %v", err)
	}
	if _, err := f.Command(ctx, "setmeta", []string{"a.txt"}, map[string]string{"mode": "0755"}); err == nil {
		t.Fatal("writes must still be refused on a corrupt map")
	}
	if after, _ := os.ReadFile(mapPath); string(after) != string(before) {
		t.Fatal("a corrupt map must never be overwritten")
	}
}

func TestDirLayoutMismatchIsRejected(t *testing.T) {
	ctx := context.Background()

	// Sparse data opened with sidecar_layout=dir.
	sparseDir := t.TempDir()
	sparse := newTestFs(t, sparseDir, nil)
	putFileForTest(t, sparse, "a.txt", "a")
	setmetaForTest(t, sparse, "a.txt", "mode", "0700")
	if _, err := NewFs(ctx, "test", "", testConfig(sparseDir, map[string]string{"sidecar_layout": layoutDir})); err == nil || !strings.Contains(err.Error(), "refusing to mix layouts") {
		t.Fatalf("dir layout over sparse data: err = %v", err)
	}

	// Dir data opened with sidecar_layout=sparse.
	dirDir := t.TempDir()
	dirFs := newTestFs(t, dirDir, map[string]string{"sidecar_layout": layoutDir})
	putFileForTest(t, dirFs, "a.txt", "a")
	setmetaForTest(t, dirFs, "a.txt", "mode", "0700")
	if _, err := NewFs(ctx, "test", "", testConfig(dirDir, nil)); err == nil || !strings.Contains(err.Error(), "refusing to mix layouts") {
		t.Fatalf("sparse layout over dir data: err = %v", err)
	}

	// Late evidence: a stray per-entry sidecar in a subdirectory is rejected by
	// the dir layout, and a stray directory map is rejected by the sparse one.
	writeFile(t, filepath.Join(dirDir, "sub", defaultSidecarPrefix+".stray"), `{"v":1,"mode":"0700"}`)
	if _, err := dirFs.List(ctx, "sub"); err == nil || !strings.Contains(err.Error(), "refusing to mix layouts") {
		t.Fatalf("dir List with a sparse sidecar: err = %v", err)
	}
	writeFile(t, filepath.Join(sparseDir, "sub2", defaultSidecarPrefix), `{"v":2,"entries":{}}`)
	if _, err := sparse.List(ctx, "sub2"); err == nil || !strings.Contains(err.Error(), "refusing to mix layouts") {
		t.Fatalf("sparse List with a directory map: err = %v", err)
	}
}

func TestDirLayoutProxyLinks(t *testing.T) {
	ctx := context.Background()
	f, rawDir := dirLayoutFs(t, map[string]string{"proxy_links": "true"})
	const target = "target.txt"
	putLinkForTest(t, f, "link.txt.rclonelink", target)

	// Only the directory map exists at rest: no `.rclonelink` payload.
	if files := mapFiles(t, rawDir); len(files) != 1 || files[0] != defaultSidecarPrefix {
		t.Fatalf("objects at rest = %v, want only the root map", files)
	}
	doc := readMapDoc(t, rawDir, "")
	requireKeys(t, doc, "link.txt")
	if doc.Entries["link.txt"].Symlink != target {
		t.Fatalf("link entry = %+v", doc.Entries["link.txt"])
	}

	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Remote() != "link.txt.rclonelink" {
		t.Fatalf("List = %v, want the emulated link", entries)
	}
	obj, err := f.NewObject(ctx, "link.txt.rclonelink")
	if err != nil {
		t.Fatalf("NewObject link: %v", err)
	}
	if got := readObjectForTest(t, obj); got != target {
		t.Fatalf("link target = %q, want %q", got, target)
	}
	if err := obj.Remove(ctx); err != nil {
		t.Fatalf("Remove link: %v", err)
	}
	requireKeys(t, readMapDoc(t, rawDir, ""), "")
}

func TestDirLayoutSubpathRootUsesParentMap(t *testing.T) {
	ctx := context.Background()
	rawDir := t.TempDir()
	writeFile(t, filepath.Join(rawDir, "sub", "x.txt"), "x")
	f := newTestFsAt(t, rawDir, "sub", map[string]string{"sidecar_layout": layoutDir})

	setmetaForTest(t, f, "", "mode", "0710", "uid", "5001")
	doc := readMapDoc(t, rawDir, "")
	requireKeys(t, doc, "sub")
	if doc.Entries["sub"].Mode != "0710" {
		t.Fatalf("subpath root entry = %+v", doc.Entries["sub"])
	}
	if meta := metadataForTest(t, f, "x.txt"); meta["uid"] != "5001" {
		t.Fatalf("inherited uid = %v", meta)
	}

	// Removing the subpath root drops the key in the parent map, and the map
	// inside the removed directory goes with it.
	if err := os.Remove(filepath.Join(rawDir, "sub", "x.txt")); err != nil {
		t.Fatalf("remove payload: %v", err)
	}
	if err := f.Rmdir(ctx, ""); err != nil {
		t.Fatalf("Rmdir root: %v", err)
	}
	requireKeys(t, readMapDoc(t, rawDir, ""), "")
}

func TestDirLayoutCoalescesUpdatesPerMap(t *testing.T) {
	ctx := context.Background()
	f, rawDir := dirLayoutFs(t, map[string]string{"writeback_delay": "1h"})
	putFileForTest(t, f, "a.txt", "a")
	putFileForTest(t, f, "b.txt", "b")

	for _, remote := range []string{"a.txt", "b.txt"} {
		obj, err := f.NewObject(ctx, remote)
		if err != nil {
			t.Fatalf("NewObject(%q): %v", remote, err)
		}
		if err := obj.(fs.SetMetadataer).SetMetadata(ctx, fs.Metadata{"mode": "0700"}); err != nil {
			t.Fatalf("SetMetadata(%q): %v", remote, err)
		}
	}
	// Debounced: nothing is uploaded yet, and both updates live in one map.
	if _, err := os.Stat(filepath.Join(rawDir, defaultSidecarPrefix)); !os.IsNotExist(err) {
		t.Fatalf("map written before the writeback delay: %v", err)
	}
	stats := f.cacheStatsSnapshot()
	if stats["dirty_maps"] != 1 || stats["pending_writes"] != 1 {
		t.Fatalf("cachestats = %v, want one dirty map and one pending write", stats)
	}

	// A shutdown flush writes the whole map once, with both entries.
	if err := f.flushAllPending(ctx); err != nil {
		t.Fatalf("flushAllPending: %v", err)
	}
	requireKeys(t, readMapDoc(t, rawDir, ""), "a.txt", "b.txt")
	if stats := f.cacheStatsSnapshot(); stats["dirty_maps"] != 0 {
		t.Fatalf("dirty maps after flush = %v", stats["dirty_maps"])
	}
}

func TestDirLayoutRepairSalvagesCorruptMap(t *testing.T) {
	ctx := context.Background()
	f, rawDir := dirLayoutFs(t, nil)
	putFileForTest(t, f, "a.txt", "a")
	putFileForTest(t, f, "b.txt", "b")

	mapPath := filepath.Join(rawDir, defaultSidecarPrefix)
	// Valid document with a wrong checksum: decodeMap rejects it, salvage keeps
	// the entries it can parse.
	damaged := `{"v":2,"crc":"00000000","entries":{"a.txt":{"mode":"0700"},"b.txt":{"mode":"0600"},"bad/name":{}}}` + "\n"
	if err := os.WriteFile(mapPath, []byte(damaged), 0o644); err != nil {
		t.Fatalf("write damaged map: %v", err)
	}

	check, err := f.Command(ctx, "repair", nil, nil)
	if err != nil {
		t.Fatalf("repair check: %v", err)
	}
	report, ok := check.(*repairReport)
	if !ok {
		t.Fatalf("repair returned %T", check)
	}
	if len(report.Corrupt) == 0 || report.WouldRepair != 1 || report.Repaired != 0 {
		t.Fatalf("dry-run report = %+v", report)
	}
	if payload, _ := os.ReadFile(mapPath); string(payload) != damaged {
		t.Fatal("a dry run must not modify the map")
	}

	if _, err := f.Command(ctx, "repair", nil, map[string]string{"dry_run": "false"}); err == nil {
		t.Fatal("repair must refuse to write without confirm=REPAIR")
	}
	done, err := f.Command(ctx, "repair", nil, map[string]string{"dry_run": "false", "confirm": "REPAIR"})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	report, ok = done.(*repairReport)
	if !ok || report.Repaired != 1 || len(report.Quarantined) != 1 {
		t.Fatalf("repair report = %+v", report)
	}
	if _, err := os.Stat(filepath.Join(rawDir, report.Quarantined[0])); err != nil {
		t.Fatalf("quarantine %q missing: %v", report.Quarantined[0], err)
	}

	// The repaired map is valid and keeps the salvageable entries only.
	doc := readMapDoc(t, rawDir, "")
	requireKeys(t, doc, "a.txt", "b.txt")
	if meta := metadataForTest(t, f, "a.txt"); meta["mode"] != "0700" {
		t.Fatalf("salvaged metadata = %v", meta)
	}
}

func TestDirLayoutRepairDropsOrphansAndTemp(t *testing.T) {
	ctx := context.Background()
	f, rawDir := dirLayoutFs(t, nil)
	putFileForTest(t, f, "a.txt", "a")
	setmetaForTest(t, f, "a.txt", "mode", "0700")

	// An orphan entry and a stale staging object.
	if _, err := f.mapApply(ctx, "gone.txt", &sidecarData{Version: sidecarSchemaVersion, Mode: "0644"}, time.Now()); err != nil {
		t.Fatalf("mapApply: %v", err)
	}
	if err := f.mapWrite(ctx, ""); err != nil {
		t.Fatalf("mapWrite: %v", err)
	}
	tmpPath := filepath.Join(rawDir, defaultSidecarPrefix+mapTempSuffix)
	writeFile(t, tmpPath, `{"v":2,"entries":{}}`)

	if _, err := f.Command(ctx, "repair", nil, map[string]string{
		"dry_run":        "false",
		"confirm":        "REPAIR",
		"remove_orphans": "true",
		"min_age":        "0s",
	}); err != nil {
		t.Fatalf("repair: %v", err)
	}
	requireKeys(t, readMapDoc(t, rawDir, ""), "a.txt")
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("staging object still present: %v", err)
	}
}

func TestDirLayoutMapSizeCapFailsClosed(t *testing.T) {
	ctx := context.Background()
	f, rawDir := dirLayoutFs(t, map[string]string{"map_max_bytes": "300b"})

	var failures int
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		putFileForTest(t, f, name, "x")
		if _, err := f.Command(ctx, "setmeta", []string{name}, map[string]string{"mode": "0700"}); err != nil {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("expected the map size cap to reject at least one write")
	}

	// Whatever was rejected, the object on the remote stays a valid document
	// below the cap, and every entry it holds still resolves.
	doc := readMapDoc(t, rawDir, "")
	if payload, err := os.ReadFile(filepath.Join(rawDir, defaultSidecarPrefix)); err != nil || len(payload) > 300 {
		t.Fatalf("map on disk = %d bytes (err %v), want <= 300", len(payload), err)
	}
	if len(doc.Entries) == 0 || len(doc.Entries) >= 20 {
		t.Fatalf("map holds %d entries, want a partial write stopped by the cap", len(doc.Entries))
	}
	for name := range doc.Entries {
		if meta := metadataForTest(t, f, name); meta["mode"] != "0700" {
			t.Fatalf("%s metadata = %v", name, meta)
		}
	}
}

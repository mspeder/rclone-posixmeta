package posixmeta

// The `repair` backend command validates and, when explicitly confirmed, rewrites
// directory maps of a sidecar_layout=dir remote:
//
//   - corrupt maps (unparsable JSON, checksum mismatch, wrong schema, oversize)
//     are quarantined as `<prefix>.corrupt.<RFC3339>` before anything is written,
//     then rebuilt from the longest valid run of entries (salvage) or emptied
//     (reset). A map is never converted to another layout.
//   - invalid entry names/values and, optionally, keys whose payload vanished
//     (remove_orphans) are dropped.
//   - stale `<prefix>.tmp` staging objects older than min_age are removed.
//
// It is a dry run unless `-o dry_run=false -o confirm=REPAIR` is given.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
)

// repairOptions is the parsed form of the command options.
type repairOptions struct {
	dryRun        bool
	salvage       bool
	reset         bool
	quarantine    bool
	removeOrphans bool
	removeTemp    bool
	minAge        time.Duration
}

// repairReport is the JSON structure returned by the repair command.
type repairReport struct {
	Layout      string   `json:"layout"`
	DryRun      bool     `json:"dry_run"`
	Dirs        int      `json:"dirs_scanned"`
	Maps        int      `json:"maps"`
	Corrupt     []string `json:"corrupt,omitempty"`
	InvalidKeys []string `json:"invalid_keys,omitempty"`
	Orphans     int      `json:"orphans_removed"`
	Temps       int      `json:"temp_removed"`
	Quarantined []string `json:"quarantined,omitempty"`
	Repaired    int      `json:"repaired"`
	WouldRepair int      `json:"would_repair"`
	Actions     []string `json:"actions,omitempty"`
	Errors      []string `json:"errors,omitempty"`
}

// reportLimit caps the lists in the report so a badly broken remote cannot
// produce megabytes of JSON.
const reportLimit = 20

func appendLimited(list []string, items ...string) []string {
	for _, item := range items {
		if len(list) >= reportLimit {
			break
		}
		list = append(list, item)
	}
	return list
}

func optBool(opt map[string]string, key string, def bool) bool {
	v, ok := opt[key]
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func optDuration(opt map[string]string, key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(opt[key])
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	return def
}

// repairCommand implements `rclone backend repair`.
func (f *Fs) repairCommand(ctx context.Context, arg []string, opt map[string]string) (any, error) {
	o := repairOptions{
		dryRun:        optBool(opt, "dry_run", true),
		salvage:       optBool(opt, "salvage", true),
		reset:         optBool(opt, "reset", false),
		quarantine:    optBool(opt, "quarantine", true),
		removeOrphans: optBool(opt, "remove_orphans", false),
		removeTemp:    optBool(opt, "remove_temp", true),
		minAge:        optDuration(opt, "min_age", 5*time.Minute),
	}
	if !o.dryRun && opt["confirm"] != "REPAIR" {
		return nil, errors.New("posixmeta: refusing to modify metadata without -o confirm=REPAIR (leave dry_run unset for a check-only run)")
	}

	// Repair must never race unflushed updates or work from a stale cache.
	if err := f.flushAllPending(ctx); err != nil {
		return nil, fmt.Errorf("posixmeta: repair: flush pending metadata first: %w", err)
	}
	start := ""
	if len(arg) > 0 {
		start = strings.Trim(arg[0], "/")
	}
	report := &repairReport{Layout: f.opt.SidecarLayout, DryRun: o.dryRun}
	if err := f.repairWalk(ctx, start, report, o); err != nil {
		report.Errors = append(report.Errors, err.Error())
	}
	f.dropAllMaps()
	return report, nil
}

// repairWalk scans one directory and its subdirectories.
func (f *Fs) repairWalk(ctx context.Context, dir string, report *repairReport, o repairOptions) error {
	entries, err := f.wrapped.List(ctx, dir)
	if err != nil {
		return err
	}
	report.Dirs++

	names := make(map[string]struct{}, len(entries))
	var subdirs []string
	for _, entry := range entries {
		remote := entry.Remote()
		if f.isSidecar(remote) {
			continue
		}
		base := path.Base(strings.TrimSuffix(remote, "/"))
		names[base] = struct{}{}
		if strings.HasSuffix(base, fs.LinkSuffix) {
			// A legacy link payload keeps the link's own key alive.
			names[strings.TrimSuffix(base, fs.LinkSuffix)] = struct{}{}
		}
		if d, ok := entry.(fs.Directory); ok {
			subdirs = append(subdirs, d.Remote())
		}
	}

	if err := f.repairMap(ctx, dir, names, report, o); err != nil {
		report.Errors = appendLimited(report.Errors, fmt.Sprintf("%s: %v", dir, err))
	}
	sort.Strings(subdirs)
	for _, sub := range subdirs {
		if err := f.repairWalk(ctx, sub, report, o); err != nil {
			report.Errors = appendLimited(report.Errors, fmt.Sprintf("%s: %v", sub, err))
		}
	}
	return nil
}

// repairMap validates and, when allowed, rewrites the map of one directory.
func (f *Fs) repairMap(ctx context.Context, dir string, names map[string]struct{}, report *repairReport, o repairOptions) error {
	absDir := f.absRemote(dir)
	mapPath := f.mapPathOf(absDir)

	if o.removeTemp {
		f.repairTemp(ctx, mapPath, o.minAge, report, o.dryRun)
	}

	raw, _, err := readObjectRaw(ctx, f.base, mapPath, int64(f.opt.MapMaxBytes))
	tooLarge := errors.Is(err, errObjectTooLarge)
	if err != nil && !tooLarge {
		return err
	}
	if raw == nil {
		return nil
	}
	report.Maps++

	isRoot := absDir == ""
	var (
		entries map[string]*sidecarData
		actions []string
		corrupt error
	)
	if tooLarge {
		corrupt = fmt.Errorf("larger than map_max_bytes (%d)", int64(f.opt.MapMaxBytes))
	} else {
		entries, corrupt = decodeMap(raw)
	}
	if corrupt != nil {
		report.Corrupt = appendLimited(report.Corrupt, fmt.Sprintf("%s: %v", mapPath, corrupt))
		switch {
		case o.reset:
			entries = map[string]*sidecarData{}
			actions = append(actions, "reset (all metadata dropped)")
		case o.salvage && raw != nil:
			salvaged, ok := salvageEntries(raw)
			if !ok {
				actions = append(actions, "reset (nothing salvageable)")
			} else {
				actions = append(actions, fmt.Sprintf("salvaged %d entries from a corrupt map", len(salvaged)))
			}
			if salvaged == nil {
				salvaged = map[string]*sidecarData{}
			}
			entries = salvaged
		case o.salvage:
			// An oversized map was never loaded, so there is nothing to salvage.
			entries = map[string]*sidecarData{}
			actions = append(actions, "reset (map too large to salvage)")
		}
	} else {
		if bad := invalidMapKeys(entries, isRoot); len(bad) > 0 {
			for _, key := range bad {
				delete(entries, key)
			}
			report.InvalidKeys = appendLimited(report.InvalidKeys, bad...)
			actions = append(actions, fmt.Sprintf("dropped %d invalid entries", len(bad)))
		}
		if o.removeOrphans {
			if orphans := orphanKeys(entries, names, isRoot); len(orphans) > 0 {
				for _, key := range orphans {
					delete(entries, key)
				}
				report.Orphans += len(orphans)
				actions = append(actions, fmt.Sprintf("removed %d orphan entries", len(orphans)))
			}
		}
	}
	if len(actions) == 0 {
		return nil
	}
	for _, action := range actions {
		report.Actions = appendLimited(report.Actions, fmt.Sprintf("%s: %s", mapPath, action))
	}
	if o.dryRun {
		report.WouldRepair++
		return nil
	}
	if o.quarantine && raw != nil {
		qpath, qerr := f.quarantineMap(ctx, absDir, raw)
		if qerr != nil {
			return fmt.Errorf("quarantine %q: %w", mapPath, qerr)
		}
		report.Quarantined = appendLimited(report.Quarantined, qpath)
	}
	if err := f.writeMapDoc(ctx, absDir, entries); err != nil {
		return err
	}
	report.Repaired++
	return nil
}

// repairTemp removes a stale staging object left behind by an interrupted map
// write.
func (f *Fs) repairTemp(ctx context.Context, mapPath string, minAge time.Duration, report *repairReport, dryRun bool) {
	tmpPath := mapPath + mapTempSuffix
	obj, err := f.base.NewObject(ctx, tmpPath)
	if err != nil {
		return
	}
	if time.Since(obj.ModTime(ctx)) < minAge {
		return
	}
	if dryRun {
		report.Temps++
		return
	}
	if err := obj.Remove(ctx); err == nil {
		report.Temps++
	}
}

// quarantineMap copies the unmodified document aside before it is replaced.
func (f *Fs) quarantineMap(ctx context.Context, dir string, raw []byte) (string, error) {
	qpath := fmt.Sprintf("%s.corrupt.%s", f.mapPathOf(dir), time.Now().UTC().Format(time.RFC3339))
	if err := putObjectRaw(ctx, f.base, qpath, raw); err != nil {
		return "", err
	}
	return qpath, nil
}

// invalidMapKeys lists entries that cannot be served: bad names, a "." key
// outside the root, unparsable modes/timestamps or oversized link targets.
func invalidMapKeys(entries map[string]*sidecarData, isRoot bool) []string {
	var bad []string
	for key, sc := range entries {
		invalid := !validMapKey(key) || (key == mapSelfKey && !isRoot) || sc == nil
		if !invalid && sc.Mode != "" {
			_, err := parseOctalMode(sc.Mode, 0)
			invalid = err != nil
		}
		if !invalid && sc.BTime != "" {
			_, err := time.Parse(time.RFC3339Nano, sc.BTime)
			invalid = err != nil
		}
		if !invalid && len(sc.Symlink) > maxLinkTargetSize {
			invalid = true
		}
		if invalid {
			bad = append(bad, key)
		}
	}
	sort.Strings(bad)
	return bad
}

// orphanKeys lists entries without a payload. A proxied symlink legitimately has
// none, and a directory owns the map inside it, so neither is ever an orphan.
func orphanKeys(entries map[string]*sidecarData, names map[string]struct{}, isRoot bool) []string {
	var orphans []string
	for key, sc := range entries {
		if key == mapSelfKey && isRoot {
			continue
		}
		if _, ok := names[key]; ok {
			continue
		}
		if sc != nil && sc.Symlink != "" {
			continue
		}
		orphans = append(orphans, key)
	}
	sort.Strings(orphans)
	return orphans
}

// salvageEntries decodes as many complete entries as possible from a damaged
// document, stopping at the first unrecoverable one. The stored checksum is not
// consulted: the point is to recover entries from a document that fails it.
func salvageEntries(raw []byte) (map[string]*sidecarData, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		if key != "entries" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, false
			}
			continue
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return nil, false
		}
		entries := make(map[string]*sidecarData)
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			name, ok := tok.(string)
			if !ok {
				break
			}
			var entry mapEntry
			if err := dec.Decode(&entry); err != nil {
				break
			}
			if validMapKey(name) {
				entries[name] = entry.sidecar()
			}
		}
		return entries, len(entries) > 0
	}
	return nil, false
}

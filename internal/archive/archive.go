// Package archive reads what ytdl-sub leaves on disk: one ".ytdl-sub-<name>-download-archive.json" per subscription
// next to its videos, holding the entries it still tracks, and the video files themselves. That is the ground truth
// for "kept", last upload, counts and sizes, and a diff of two scans gives added and removed without parsing any log.
//
// The library usually sits on a network share, which charges per round trip rather than per byte, so the scan is
// built around the number of directory listings. Discovery lists root and descends only into folders that hold no
// archive (an intermediate folder such as "tv/", or a folder ytdl-sub never wrote), so a library of N shows costs
// about N listings instead of one per season. Only the subscribed shows' folders are then read and walked for their
// files, several at a time.
package archive

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	workers  = 8 // listings, reads and walks in flight at once; a share is latency-bound, so this is close to linear
	maxDepth = 3 // how far below root discovery looks for archives (root/<show>/ is depth 1)
)

// Entry is one subscription's archive and the files beside it.
type Entry struct {
	Name    string
	Dir     string
	IDs     map[string]struct{}
	Uploads []string // YYYYMMDD, sorted
	Files   int
	Files30 int
	Files90 int
	Bytes   int64
	Bytes30 int64
	Newest  time.Time
	path    string
}

var reArchive = regexp.MustCompile(`^\.ytdl-sub-(.+)-download-archive\.json$`) // exact: Syncthing's .sync-conflict copies do not match
var videoExt = map[string]bool{".mp4": true, ".mkv": true, ".webm": true, ".mov": true, ".m4v": true, ".avi": true}
var digits = regexp.MustCompile(`[^0-9]`)

// Scan finds the archives under root and measures the shows in names, or every show when names is empty. Shows
// without an archive are simply absent from the result. A root that cannot be read is an error, so a wrong MEDIA_DIR
// shows up as a warning rather than as an empty library.
func Scan(root string, names []string, now time.Time) (map[string]*Entry, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	sc := &scanner{sem: make(chan struct{}, workers)}
	sc.discover(root, 0)
	sc.wg.Wait()

	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := map[string]*Entry{}
	byDir := map[string][]*Entry{}
	for _, f := range sc.found {
		if len(want) > 0 && !want[f.name] {
			continue
		}
		e := &Entry{Name: f.name, Dir: f.dir, IDs: map[string]struct{}{}, path: f.path}
		out[e.Name] = e
		byDir[f.dir] = append(byDir[f.dir], e)
	}
	var wg sync.WaitGroup
	for _, es := range byDir {
		wg.Add(1)
		sc.sem <- struct{}{}
		go func(es []*Entry) {
			defer wg.Done()
			defer func() { <-sc.sem }()
			for _, e := range es {
				readArchive(e.path, e)
			}
			measure(es[0], now)
			for _, e := range es[1:] { // two archives in one folder share its files
				e.Files, e.Files30, e.Files90, e.Bytes, e.Bytes30, e.Newest = es[0].Files, es[0].Files30, es[0].Files90, es[0].Bytes, es[0].Bytes30, es[0].Newest
			}
		}(es)
	}
	wg.Wait()
	return out, nil
}

type found struct{ name, path, dir string }

type scanner struct {
	sem   chan struct{}
	wg    sync.WaitGroup
	mu    sync.Mutex
	found []found
}

// discover lists dir and records its archives. It descends into subfolders only from root, from a folder that holds
// no archive, and never below maxDepth, so a show's season folders are not listed here.
func (sc *scanner) discover(dir string, depth int) {
	sc.sem <- struct{}{}
	ents, err := os.ReadDir(dir)
	<-sc.sem
	if err != nil {
		return // an unreadable folder is skipped, not fatal
	}
	var subdirs []string
	here := false
	for _, d := range ents {
		if d.IsDir() {
			subdirs = append(subdirs, d.Name())
			continue
		}
		m := reArchive.FindStringSubmatch(d.Name())
		if m == nil {
			continue
		}
		here = true
		sc.mu.Lock()
		sc.found = append(sc.found, found{name: m[1], path: filepath.Join(dir, d.Name()), dir: dir})
		sc.mu.Unlock()
	}
	if (here && depth > 0) || depth >= maxDepth {
		return
	}
	for _, n := range subdirs {
		p := filepath.Join(dir, n)
		sc.wg.Add(1)
		go func() {
			defer sc.wg.Done()
			sc.discover(p, depth+1)
		}()
	}
}

func readArchive(path string, e *Entry) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	for id, v := range raw {
		e.IDs[id] = struct{}{}
		var rec struct {
			UploadDate string `json:"upload_date"`
		}
		if json.Unmarshal(v, &rec) == nil && rec.UploadDate != "" {
			if d := digits.ReplaceAllString(rec.UploadDate, ""); len(d) == 8 {
				e.Uploads = append(e.Uploads, d)
			}
		}
	}
	sort.Strings(e.Uploads)
}

func measure(e *Entry, now time.Time) {
	filepath.WalkDir(e.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !videoExt[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		e.Files++
		e.Bytes += info.Size()
		age := now.Sub(info.ModTime())
		if age < 30*24*time.Hour {
			e.Files30++
			e.Bytes30 += info.Size()
		}
		if age < 90*24*time.Hour {
			e.Files90++
		}
		if info.ModTime().After(e.Newest) {
			e.Newest = info.ModTime()
		}
		return nil
	})
}

// Delta is what changed for one subscription between two scans.
type Delta struct{ Added, Removed int }

// Diff compares two scans by archive entry: added are IDs new in after, removed the ones gone. Subscriptions missing
// from either side count from an empty set.
func Diff(before, after map[string]*Entry) map[string]Delta {
	out := map[string]Delta{}
	names := map[string]bool{}
	for n := range before {
		names[n] = true
	}
	for n := range after {
		names[n] = true
	}
	for n := range names {
		var b, a map[string]struct{}
		if e := before[n]; e != nil {
			b = e.IDs
		}
		if e := after[n]; e != nil {
			a = e.IDs
		}
		var d Delta
		for id := range a {
			if _, ok := b[id]; !ok {
				d.Added++
			}
		}
		for id := range b {
			if _, ok := a[id]; !ok {
				d.Removed++
			}
		}
		out[n] = d
	}
	return out
}

// LastUpload formats the newest upload date as YYYY-MM-DD.
func (e *Entry) LastUpload() string {
	if e == nil || len(e.Uploads) == 0 {
		return ""
	}
	u := e.Uploads[len(e.Uploads)-1]
	return u[:4] + "-" + u[4:6] + "-" + u[6:8]
}

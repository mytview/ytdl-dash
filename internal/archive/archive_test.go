package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path string, size int) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func archiveJSON(t *testing.T, dir, name, body string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, ".ytdl-sub-"+name+"-download-archive.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A library with shows at depth one and two, a folder of files ytdl-sub never wrote, two archives sharing one folder,
// and a Syncthing conflict copy that must not count as an archive.
func fixture(t *testing.T) string {
	root := t.TempDir()
	fs := filepath.Join(root, "Fireship")
	archiveJSON(t, fs, "Fireship", `{"a1": {"extractor": "youtube", "upload_date": "20261001"}, "b2": {"upload_date": "2026-10-20"}}`)
	os.WriteFile(filepath.Join(fs, ".ytdl-sub-Fireship-download-archive.sync-conflict-20251101-042922-E7EJFVZ.json"), []byte(`{"zz": {}}`), 0o644)
	write(t, filepath.Join(fs, "Season 2026", "old.mp4"), 1000)
	write(t, filepath.Join(fs, "Season 2026", "new.mkv"), 2000)
	write(t, filepath.Join(fs, "Season 2026", "poster.jpg"), 10)
	old := time.Now().Add(-100 * 24 * time.Hour)
	os.Chtimes(filepath.Join(fs, "Season 2026", "old.mp4"), old, old)

	deep := filepath.Join(root, "tv", "Veritasium") // MEDIA_DIR one level above the show folders
	archiveJSON(t, deep, "Veritasium", `{"v1": {"upload_date": "20260901"}}`)
	write(t, filepath.Join(deep, "Season 2026", "v1.webm"), 500)

	shared := filepath.Join(root, "Daily Show") // two subscriptions writing into one folder
	archiveJSON(t, shared, "Daily Show", `{"d1": {}}`)
	archiveJSON(t, shared, "Daily Show Clips", `{"c1": {}, "c2": {}}`)
	write(t, filepath.Join(shared, "Season 2026", "d1.mp4"), 100)
	write(t, filepath.Join(shared, "Season 2026", "c1.mp4"), 100)

	write(t, filepath.Join(root, "Pinchflat Leftover", "Season 2024", "x.mp4"), 5) // no archive anywhere below
	write(t, filepath.Join(root, "Pinchflat Leftover", "Season 2025", "y.mp4"), 5)
	return root
}

func TestScanEverything(t *testing.T) {
	got, err := Scan(fixture(t), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("archives %v", keys(got))
	}
	e := got["Fireship"]
	if e == nil || len(e.IDs) != 2 || e.Files != 2 || e.Files30 != 1 || e.Files90 != 1 || e.Bytes != 3000 || e.Bytes30 != 2000 {
		t.Fatalf("Fireship %+v", e)
	}
	if e.LastUpload() != "2026-10-20" {
		t.Errorf("last upload %q", e.LastUpload())
	}
	if v := got["Veritasium"]; v == nil || v.Files != 1 || v.Bytes != 500 || filepath.Base(filepath.Dir(v.Dir)) != "tv" {
		t.Errorf("depth-two show %+v", v)
	}
	if a, b := got["Daily Show"], got["Daily Show Clips"]; a == nil || b == nil || a.Files != 2 || b.Files != 2 || len(a.IDs) != 1 || len(b.IDs) != 2 {
		t.Errorf("shared folder %+v %+v", a, b)
	}
}

func TestScanOnlyNames(t *testing.T) {
	got, err := Scan(fixture(t), []string{"Fireship", "Daily Show Clips", "Never Downloaded"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["Fireship"] == nil || got["Daily Show Clips"] == nil {
		t.Fatalf("archives %v", keys(got))
	}
	if got["Daily Show Clips"].Files != 2 || len(got["Daily Show Clips"].IDs) != 2 {
		t.Errorf("clips %+v", got["Daily Show Clips"])
	}
}

func TestScanMissingRoot(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "nope"), nil, time.Now()); err == nil {
		t.Fatal("expected an error for a missing root")
	}
}

func TestDiff(t *testing.T) {
	before := map[string]*Entry{"Fireship": {IDs: map[string]struct{}{"a1": {}, "b2": {}}}}
	after := map[string]*Entry{"Fireship": {IDs: map[string]struct{}{"b2": {}, "c3": {}, "d4": {}}}, "New": {IDs: map[string]struct{}{"n1": {}}}}
	d := Diff(before, after)
	if d["Fireship"] != (Delta{Added: 2, Removed: 1}) || d["New"] != (Delta{Added: 1}) {
		t.Errorf("diff %v", d)
	}
}

func keys(m map[string]*Entry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

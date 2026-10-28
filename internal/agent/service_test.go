package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServiceOverSampleLog(t *testing.T) {
	dir := t.TempDir()
	cfgDir, media, state := filepath.Join(dir, "config"), filepath.Join(dir, "media"), filepath.Join(dir, "state")
	os.MkdirAll(cfgDir, 0o755)
	os.WriteFile(filepath.Join(cfgDir, "subscriptions.yaml"), []byte("recent:\n  = Tech | = TV-Y:\n    \"Optimum\": \"https://www.youtube.com/@optimumtech\"\n    \"Fireship\": \"https://www.youtube.com/@Fireship\"\nkeep:\n  \"Dead Channel\": \"https://www.youtube.com/@dead\"\n"), 0o644)
	sample, err := os.ReadFile("../../testdata/cron-sample.log")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfgDir, ".cron.log"), sample, 0o644)
	os.WriteFile(filepath.Join(cfgDir, ".last-success"), nil, 0o644)
	today := time.Now().UTC().Format("20060102")
	for name, ids := range map[string][]string{"Optimum": {"a"}, "Fireship": {"b", "c", "d"}} {
		d := filepath.Join(media, name, "Season 2026")
		os.MkdirAll(d, 0o755)
		var entries []string
		for _, id := range ids {
			entries = append(entries, `"`+id+`": {"upload_date": "`+today+`"}`)
			os.WriteFile(filepath.Join(d, id+".mp4"), make([]byte, 1000), 0o644)
		}
		os.WriteFile(filepath.Join(media, name, ".ytdl-sub-"+name+"-download-archive.json"), []byte("{"+strings.Join(entries, ",")+"}"), 0o644)
	}
	cfg := Config{Instance: "tech", Host: "box", ConfigDir: cfgDir, LogFile: filepath.Join(cfgDir, ".cron.log"), SubsFile: filepath.Join(cfgDir, "subscriptions.yaml"),
		MediaDir: media, StateDir: state, Token: "t", ExitLabel: "own address", Rescan: time.Hour, HealthMaxAge: 6 * time.Hour, Version: "test"}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cs := svc.Channels(); len(cs) == 3 && cs[0].Folder {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	runs := svc.Runs(0)
	var results []string
	for _, r := range runs {
		results = append(results, r.Result)
	}
	if strings.Join(results, ",") != "error,lock,killed,timeout,ok" {
		t.Fatalf("runs %v", results)
	}
	if runs[0].Added == nil || *runs[0].Added != 2 || *runs[0].Removed != 1 || *runs[0].Kept != 13 || runs[0].Source != "summary" || runs[0].Minutes == nil {
		t.Errorf("first run %+v", runs[0])
	}

	byName := map[string]string{}
	for _, c := range svc.Channels() {
		byName[c.Name] = c.Status
		if c.Name == "Fireship" && (c.Kept != 3 || c.Files != 3 || c.Files30 != 3 || c.Check != "ok" || c.Group != "Tech" || c.Rating != "TV-Y" || c.Preset != "recent" || c.Handle != "@Fireship") {
			t.Errorf("Fireship %+v", c)
		}
	}
	if byName["Dead Channel"] != "failed" || byName["Optimum"] != "active" || byName["Fireship"] != "active" {
		t.Errorf("statuses %v", byName)
	}

	inst := svc.Instance()
	if inst.Health != "healthy" || inst.Channels != 3 || inst.Keep != 1 || inst.Videos30 != 4 || inst.Exit != "own address" || inst.Mode != "direct" || inst.LastResult != "ok" || inst.LastMinutes == nil {
		t.Errorf("instance %+v", inst)
	}
	if caps := strings.Join(inst.Capabilities, ","); caps != "rescan" {
		t.Errorf("capabilities %q (no docker, no gluetun)", caps)
	}
	if d := svc.Daily(30); len(d) != 30 || d[29]["tech"] != 4 {
		t.Errorf("daily %v", d[len(d)-1])
	}

	// the HTTP side
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/all")
	var all map[string]json.RawMessage
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&all) != nil || all["instance"] == nil || all["channels"] == nil || all["runs"] == nil || all["daily"] == nil || all["meta"] == nil {
		t.Errorf("/api/all %d %v", resp.StatusCode, all)
	}
	resp, _ = http.Post(srv.URL+"/api/run", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("run without token: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/run", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("run without docker: %d", resp.StatusCode)
	}

	// a run that starts and ends live, with no summary: counted from the archive diff
	f, _ := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("Cron started, running ytdl-sub... (2026-10-27T02:10:00Z)\n")
	f.Close()
	time.Sleep(1200 * time.Millisecond) // the tailer polls every second
	os.WriteFile(filepath.Join(media, "Optimum", ".ytdl-sub-Optimum-download-archive.json"), []byte(`{"a": {"upload_date": "`+today+`"}, "new1": {"upload_date": "`+today+`"}}`), 0o644)
	f, _ = os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("ytdl-sub finished OK (2026-10-27T02:20:00Z)\n")
	f.Close()
	deadline = time.Now().Add(4 * time.Second)
	var last *struct {
		Source string
		Added  int
	}
	for time.Now().Before(deadline) {
		rs := svc.Runs(0)
		if r := rs[len(rs)-1]; r.Result == "ok" && r.Start == "2026-10-27T02:10:00Z" && r.Source != "" {
			last = &struct {
				Source string
				Added  int
			}{r.Source, *r.Added}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if last == nil || last.Source != "archive" || last.Added != 1 {
		t.Fatalf("live run from archives: %+v", last)
	}
	if _, err := os.Stat(filepath.Join(state, "state.json")); err != nil {
		t.Errorf("state not saved: %v", err)
	}
}

func TestShellWords(t *testing.T) {
	got := shellWords(`sh -c 'bash /config/cron >> /config/.cron.log 2>&1'`)
	if len(got) != 3 || got[2] != "bash /config/cron >> /config/.cron.log 2>&1" {
		t.Errorf("%q", got)
	}
	if got := shellWords(`pkill -TERM -f [y]tdl-sub\ -l`); len(got) != 4 || got[3] != "[y]tdl-sub -l" {
		t.Errorf("%q", got)
	}
}

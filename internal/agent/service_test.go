package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeFixture lays out a config folder with the sample cron log and three subscriptions, and a library where two of
// them have an archive and files dated today.
func writeFixture(t *testing.T, dir string) (cfg Config, media, today string) {
	t.Helper()
	cfgDir, state := filepath.Join(dir, "config"), filepath.Join(dir, "state")
	media = filepath.Join(dir, "media")
	os.MkdirAll(cfgDir, 0o755)
	os.WriteFile(filepath.Join(cfgDir, "subscriptions.yaml"), []byte("recent:\n  = Tech | = TV-Y:\n    \"Optimum\": \"https://www.youtube.com/@optimumtech\"\n    \"Fireship\": \"https://www.youtube.com/@Fireship\"\nkeep:\n  \"Dead Channel\": \"https://www.youtube.com/@dead\"\n"), 0o644)
	sample, err := os.ReadFile("../../testdata/cron-sample.log")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfgDir, ".cron.log"), sample, 0o644)
	os.WriteFile(filepath.Join(cfgDir, ".last-success"), nil, 0o644)
	today = time.Now().UTC().Format("20060102")
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
	cfg = Config{Instance: "tech", Host: "box", ConfigDir: cfgDir, LogFile: filepath.Join(cfgDir, ".cron.log"), SubsFile: filepath.Join(cfgDir, "subscriptions.yaml"),
		MediaDir: media, StateDir: state, Token: "t", ExitLabel: "own address", Rescan: time.Hour, HealthMaxAge: 6 * time.Hour, Version: "test"}
	return cfg, media, today
}

func TestServiceOverSampleLog(t *testing.T) {
	cfg, media, today := writeFixture(t, t.TempDir())
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
	if inst.Health != "healthy" || inst.Channels != 3 || inst.Keep != 1 || inst.Videos30 != 4 || inst.Exit != "own address" || inst.Mode != "direct" || inst.LastResult != "ok" || inst.LastMinutes == nil || inst.Host != "box" {
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
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "state.json")); err != nil {
		t.Errorf("state not saved: %v", err)
	}

	// a restart: the saved history and the replayed log overlap. Each run shows once, and the live run keeps the counts
	// the archive diff gave it, which the log never had.
	cancel()
	time.Sleep(100 * time.Millisecond)
	again, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	again.Start(ctx2)
	rs := again.Runs(0)
	if len(rs) != 6 {
		t.Fatalf("after a restart %d runs, want 6: %+v", len(rs), rs)
	}
	if r := rs[5]; r.Start != "2026-10-27T02:10:00Z" || r.Source != "archive" || r.Added == nil || *r.Added != 1 {
		t.Errorf("after a restart the live run lost its counts: %+v", r)
	}
}

// fakeDocker answers the little of the Engine API the agent uses, over a unix socket: ping, info, inspect, exec.
type fakeDocker struct {
	mu      sync.Mutex
	running bool   // the ytdl-sub container
	subs    string // what `cat /config/subscriptions.yaml` prints inside it
	execs   map[string][]string
	cats    int // how many times the subscriptions were read
}

func (f *fakeDocker) serve(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dk") // not t.TempDir: a unix socket path must stay short
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); os.RemoveAll(dir) })
	return sock
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/_ping":
		w.WriteHeader(http.StatusOK)
	case p == "/info":
		json.NewEncoder(w).Encode(map[string]string{"Name": "dockerhost"})
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		status := "exited"
		if f.running {
			status = "running"
		}
		json.NewEncoder(w).Encode(map[string]any{"Config": map[string]string{"Image": "ytdl-sub"},
			"State": map[string]any{"Status": status, "StartedAt": time.Now().UTC().Format(time.RFC3339Nano), "Health": map[string]string{"Status": "healthy"}}})
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/exec"):
		if !f.running {
			http.Error(w, `{"message":"container is not running"}`, http.StatusConflict)
			return
		}
		var req struct{ Cmd []string }
		json.NewDecoder(r.Body).Decode(&req)
		id := fmt.Sprintf("exec%d", len(f.execs)+1)
		f.execs[id] = req.Cmd
		json.NewEncoder(w).Encode(map[string]string{"Id": id})
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/start"):
		out := "ytdl-sub 2.0\n"
		if cmd := f.execs[strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/start")]; len(cmd) > 0 && cmd[0] == "cat" {
			out = f.subs
			f.cats++
		}
		frame := make([]byte, 8) // Docker's multiplexed stream: type, three zero bytes, big-endian length, payload
		frame[0] = 1
		binary.BigEndian.PutUint32(frame[4:], uint32(len(out)))
		w.Write(frame)
		w.Write([]byte(out))
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
		json.NewEncoder(w).Encode(map[string]any{"ExitCode": 0, "Running": false})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDocker) set(running bool) {
	f.mu.Lock()
	f.running = running
	f.mu.Unlock()
}

func (f *fakeDocker) catCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cats
}

// With the socket: the card carries the Docker host's name, and the subscriptions come from inside the container, read
// again each minute while the container is not running yet, which is what the file on disk stands in for meanwhile.
func TestThroughTheSocket(t *testing.T) {
	cfg, _, _ := writeFixture(t, t.TempDir())
	fd := &fakeDocker{execs: map[string][]string{}, subs: "recent:\n  \"Optimum\": \"https://www.youtube.com/@optimumtech\"\n  \"Fireship\": \"https://www.youtube.com/@Fireship\"\n  \"Dead Channel\": \"https://www.youtube.com/@dead\"\n  \"Fourth\": \"https://www.youtube.com/@fourth\"\n"}
	cfg.Host = ""
	cfg.Container, cfg.DockerSocket, cfg.SubsInContainer, cfg.VersionCommand = "ytdlsub-tech", fd.serve(t), "/config/subscriptions.yaml", "ytdl-sub --version"
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if h := svc.Instance().Host; h != "dockerhost" {
		t.Errorf("host %q, want the daemon's name", h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	if n := len(svc.Channels()); n != 3 {
		t.Fatalf("container not running: %d channels from the file, want 3", n)
	}
	fd.set(true)
	svc.tick(ctx) // the minute's work tries the socket again
	if n := len(svc.Channels()); n != 4 {
		t.Fatalf("container running: %d channels, want 4 from inside it", n)
	}
	reads := fd.catCount()
	svc.tick(ctx) // and stops retrying once it has read through the socket
	if fd.catCount() != reads {
		t.Errorf("subscriptions read again after a successful read: %d then %d", reads, fd.catCount())
	}
	if caps := strings.Join(svc.Instance().Capabilities, ","); !strings.Contains(caps, "run") {
		t.Errorf("capabilities %q, want run with the socket", caps)
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

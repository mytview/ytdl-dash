// Package dash is the dashboard: it polls every agent, merges their answers into the five JSON files the page reads,
// serves the page, and forwards commands to the right agent with the shared token.
package dash

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mytview/ytdl-dash/internal/model"
)

//go:embed web/index.html
var web embed.FS

// Agent is one configured agent.
type Agent struct {
	Name string // a label from AGENTS ("name=url") until the agent answers; then the agent's own instance name
	URL  string
}

// Config is the dashboard's environment.
type Config struct {
	Agents  []Agent
	Token   string
	Poll    time.Duration
	Title   string
	Version string
}

type answer struct {
	Instance model.Instance   `json:"instance"`
	Running  bool             `json:"running"`
	Current  string           `json:"current"`
	Channels []model.Channel  `json:"channels"`
	Runs     []model.Run      `json:"runs"`
	Daily    []model.DailyRow `json:"daily"`
	Meta     model.Meta       `json:"meta"`
}

type agentState struct {
	cfg     Agent
	last    *answer
	lastOK  time.Time
	lastErr error
}

// Dash is the service.
type Dash struct {
	cfg    Config
	http   *http.Client
	mu     sync.RWMutex
	agents []*agentState
	files  map[string][]byte // instances.json, channels.json, runs.json, daily.json, meta.json
	page   []byte
}

// New builds the dashboard.
func New(cfg Config) (*Dash, error) {
	if len(cfg.Agents) == 0 {
		return nil, fmt.Errorf("AGENTS is required: a comma-separated list of agent URLs")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = time.Minute
	}
	if cfg.Title == "" {
		cfg.Title = "ytdl-dash"
	}
	raw, err := web.ReadFile("web/index.html")
	if err != nil {
		return nil, err
	}
	page := bytes.ReplaceAll(raw, []byte("{{TITLE}}"), []byte(htmlEscape(cfg.Title)))
	page = bytes.ReplaceAll(page, []byte("{{POLL}}"), []byte(fmt.Sprint(int(cfg.Poll.Seconds()))))
	d := &Dash{cfg: cfg, http: &http.Client{Timeout: 25 * time.Second}, files: map[string][]byte{}, page: page}
	for _, a := range cfg.Agents {
		d.agents = append(d.agents, &agentState{cfg: a})
	}
	return d, nil
}

// ParseAgents reads "name=url,url2,..." into agents.
func ParseAgents(spec string) []Agent {
	var out []Agent
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, url, ok := strings.Cut(item, "=")
		if !ok || strings.HasPrefix(name, "http") {
			name, url = "", item
		}
		out = append(out, Agent{Name: strings.TrimSpace(name), URL: strings.TrimRight(strings.TrimSpace(url), "/")})
	}
	return out
}

// Start polls until ctx ends.
func (d *Dash) Start(ctx context.Context) {
	d.poll(ctx)
	go func() {
		t := time.NewTicker(d.cfg.Poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.poll(ctx)
			}
		}
	}()
}

func (d *Dash) poll(ctx context.Context) {
	t0 := time.Now()
	var wg sync.WaitGroup
	for _, a := range d.agents {
		wg.Add(1)
		go func(a *agentState) {
			defer wg.Done()
			ans, err := d.fetch(ctx, a.cfg.URL)
			d.mu.Lock()
			defer d.mu.Unlock()
			if err != nil {
				a.lastErr = err
				return
			}
			a.last, a.lastErr, a.lastOK = ans, nil, time.Now()
			if a.cfg.Name == "" {
				a.cfg.Name = ans.Instance.Instance
			}
		}(a)
	}
	wg.Wait()
	d.merge(time.Since(t0))
}

func (d *Dash) fetch(ctx context.Context, base string) (*answer, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/all", nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var ans answer
	if err := json.NewDecoder(resp.Body).Decode(&ans); err != nil {
		return nil, fmt.Errorf("bad answer: %w", err)
	}
	return &ans, nil
}

// label names an agent on the page: the agent's own instance name once it has answered, a name given in AGENTS, or
// the host and port of its URL until then.
func (a *agentState) label() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	if u, err := url.Parse(a.cfg.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return a.cfg.URL
}

// merge builds the six files from the agents' last answers, in the configured order.
func (d *Dash) merge(took time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var instances []model.Instance
	var channels []model.Channel
	var runs []model.Run
	daily := map[string]model.DailyRow{}
	meta := model.Meta{Generated: time.Now().UTC().Format("2006-01-02T15:04:05Z"), Seconds: float64(int(took.Seconds()*10)) / 10, Warnings: []string{}}
	names := map[string]bool{}
	for _, a := range d.agents {
		name := a.label()
		meta.Agents = append(meta.Agents, name)
		if a.last == nil {
			instances = append(instances, model.Instance{Instance: name, Health: "unreachable", Exit: "", LastResult: "none"})
			meta.Warnings = append(meta.Warnings, fmt.Sprintf("%s: agent unreachable (%v)", name, a.lastErr))
			continue
		}
		inst := a.last.Instance
		if a.lastErr != nil {
			meta.Warnings = append(meta.Warnings, fmt.Sprintf("%s: agent unreachable since %s (%v), showing its last answer", name, a.lastOK.UTC().Format("15:04"), a.lastErr))
			inst.Health = "unreachable"
		}
		if a.last.Running && a.last.Current != "" {
			inst.LastResult = "running: " + a.last.Current
		}
		if names[inst.Instance] {
			meta.Warnings = append(meta.Warnings, fmt.Sprintf("%s: two agents report this instance name; give each agent its own INSTANCE", inst.Instance))
		}
		instances = append(instances, inst)
		names[inst.Instance] = true
		channels = append(channels, a.last.Channels...)
		runs = append(runs, a.last.Runs...)
		for _, row := range a.last.Daily {
			day, _ := row["day"].(string)
			if day == "" {
				continue
			}
			if daily[day] == nil {
				daily[day] = model.DailyRow{"day": day}
			}
			for k, v := range row {
				if k != "day" {
					daily[day][k] = v
				}
			}
		}
		for _, w := range a.last.Meta.Warnings {
			meta.Warnings = append(meta.Warnings, inst.Instance+": "+w)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Start < runs[j].Start })
	var days []string
	for day := range daily {
		days = append(days, day)
	}
	sort.Strings(days)
	var dailyRows []model.DailyRow
	for _, day := range days {
		row := daily[day]
		for n := range names { // every instance present on every day, so the chart stacks cleanly
			if _, ok := row[n]; !ok {
				row[n] = 0
			}
		}
		dailyRows = append(dailyRows, row)
	}
	if channels == nil {
		channels = []model.Channel{}
	}
	if runs == nil {
		runs = []model.Run{}
	}
	if dailyRows == nil {
		dailyRows = []model.DailyRow{}
	}
	d.files["instances.json"], _ = json.Marshal(instances)
	d.files["channels.json"], _ = json.Marshal(channels)
	d.files["runs.json"], _ = json.Marshal(runs)
	d.files["daily.json"], _ = json.Marshal(dailyRows)
	d.files["meta.json"], _ = json.Marshal(meta)
	d.files["summary.json"], _ = json.Marshal(summarize(instances, meta))
}

// summarize folds the instances into the one-object view a home dashboard shows on a single card.
func summarize(instances []model.Instance, meta model.Meta) model.Summary {
	s := model.Summary{Generated: meta.Generated, Warnings: len(meta.Warnings)}
	for _, i := range instances {
		s.Instances++
		switch i.Health {
		case "healthy":
			s.Healthy++
		case "unreachable":
			s.Unreachable++
		}
		if strings.HasPrefix(i.LastResult, "running") {
			s.Running++
		}
		s.Channels += i.Channels
		s.Silent += i.Silent
		s.Videos30 += i.Videos30
		s.GB += i.GB
		s.Runs24h += i.Runs24h
		s.Failed24h += i.Failed24h
		if i.LastEnd > s.LastEnd {
			s.LastEnd, s.LastResult = i.LastEnd, i.LastResult
		}
	}
	s.GB = float64(int(s.GB*10)) / 10
	return s
}

// Handler serves the page, the data and the command proxy.
func (d *Dash) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(d.page)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /data/{file}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.RLock()
		b, ok := d.files[r.PathValue("file")]
		d.mu.RUnlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	mux.HandleFunc("POST /api/refresh", func(w http.ResponseWriter, r *http.Request) {
		d.poll(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"refreshed"}`))
	})
	mux.HandleFunc("POST /api/{instance}/{cmd}", d.proxy)
	return mux
}

var allowed = map[string]bool{"run": true, "stop": true, "reconnect": true, "rescan": true}

// proxy forwards a command to the agent that owns the instance, adding the token. The browser never sees the token.
func (d *Dash) proxy(w http.ResponseWriter, r *http.Request) {
	inst, cmd := r.PathValue("instance"), r.PathValue("cmd")
	w.Header().Set("Content-Type", "application/json")
	if !allowed[cmd] {
		http.Error(w, `{"error":"unknown command"}`, http.StatusNotFound)
		return
	}
	if d.cfg.Token == "" {
		http.Error(w, `{"error":"commands are off: set TOKEN on the dashboard and the agents"}`, http.StatusForbidden)
		return
	}
	d.mu.RLock()
	var target string
	for _, a := range d.agents {
		if a.cfg.Name == inst || (a.last != nil && a.last.Instance.Instance == inst) {
			target = a.cfg.URL
		}
	}
	d.mu.RUnlock()
	if target == "" {
		http.Error(w, `{"error":"no such instance"}`, http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target+"/api/"+cmd, nil)
	req.Header.Set("Authorization", "Bearer "+d.cfg.Token)
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	log.Printf("%s %s -> %d", inst, cmd, resp.StatusCode)
	go func() { time.Sleep(3 * time.Second); d.poll(context.Background()) }()
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

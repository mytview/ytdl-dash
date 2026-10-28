// Package agent is the service beside one ytdl-sub instance: it follows the cron log, keeps the run history, reads the
// archives and the subscriptions, asks the exit provider where the tunnel goes, fires triggers, and answers over HTTP.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mytview/ytdl-dash/internal/actions"
	"github.com/mytview/ytdl-dash/internal/archive"
	"github.com/mytview/ytdl-dash/internal/dockerx"
	"github.com/mytview/ytdl-dash/internal/gluetun"
	"github.com/mytview/ytdl-dash/internal/model"
	"github.com/mytview/ytdl-dash/internal/runlog"
	"github.com/mytview/ytdl-dash/internal/subs"
)

const timeFmt = "2006-01-02T15:04:05Z"

type verdict struct {
	OK      bool   `json:"ok"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Kept    int    `json:"kept"`
	At      string `json:"at"`
}

type persisted struct {
	Runs     []model.Run        `json:"runs"`
	Verdicts map[string]verdict `json:"verdicts"`
}

// Service is the agent.
type Service struct {
	cfg      Config
	docker   *dockerx.Client
	gluetun  *gluetun.Client
	triggers *actions.Runner
	tailer   *runlog.Tailer

	replaying atomic.Bool
	startedAt time.Time

	mu         sync.RWMutex
	runs       []model.Run
	current    *model.Run
	currentSub string
	rows       []runlog.Event
	verdicts   map[string]verdict
	startSnap  map[string]*archive.Entry
	archives   map[string]*archive.Entry
	scannedAt  time.Time
	scanTook   time.Duration
	subs       []subs.Subscription
	subsErr    string
	exit       gluetun.Exit
	exitErr    error
	ytdlVer    string
	container  *dockerx.State
	containerE error
	warnings   []string
}

// New wires the service from its config.
func New(cfg Config) (*Service, error) {
	s := &Service{cfg: cfg, verdicts: map[string]verdict{}, startedAt: time.Now()}
	if cfg.Container != "" {
		d := &dockerx.Client{Socket: cfg.DockerSocket}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if d.Available(ctx) {
			s.docker = d
		} else {
			log.Printf("docker: socket %s does not answer; health, run, stop and version come without it", cfg.DockerSocket)
		}
		cancel()
	}
	if cfg.GluetunURL != "" {
		s.gluetun = &gluetun.Client{BaseURL: cfg.GluetunURL, APIKey: cfg.GluetunKey, Log: log.Printf}
	}
	triggers, err := actions.ParseEnv()
	if err != nil {
		return nil, err
	}
	hooks := actions.Hooks{Instance: cfg.Instance, Log: log.Printf}
	if s.gluetun != nil {
		hooks.Reconnect = s.gluetun.Reconnect
	}
	if s.docker != nil {
		hooks.Restart = func(ctx context.Context, name string) error { return s.docker.Restart(ctx, name, 30) }
	}
	s.triggers = &actions.Runner{Triggers: triggers, Hooks: hooks}
	for _, t := range triggers {
		log.Printf("trigger: %q => %s", t.Match, t.Action)
	}
	return s, nil
}

// Start loads the state, replays the log, then follows it and keeps the slow data fresh until ctx ends.
func (s *Service) Start(ctx context.Context) {
	s.loadState()
	s.loadSubs()
	s.tailer = &runlog.Tailer{Path: s.cfg.LogFile, Parser: &runlog.Parser{}, Handle: s.handle}
	s.replaying.Store(true)
	if err := s.tailer.Replay(); err != nil {
		log.Printf("log %s: %v (waiting for it)", s.cfg.LogFile, err)
	}
	s.replaying.Store(false)
	s.mu.Lock()
	if s.current != nil { // a run open at the end of the log: still going, or killed with the container; the tail decides
		s.currentSub = ""
	}
	s.mu.Unlock()
	log.Printf("replayed %s: %d runs", s.cfg.LogFile, len(s.runs))
	go s.tailer.Follow()
	go s.refreshSlow(ctx, true)
	go func() {
		t := time.NewTicker(s.cfg.Rescan)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.tailer.Stop()
				return
			case <-t.C:
				s.refreshSlow(ctx, true)
			}
		}
	}()
	go func() { // the cheap things, often
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshFast(ctx)
			}
		}
	}()
}

// handle receives every event the log produces, during replay and live.
func (s *Service) handle(ev runlog.Event) {
	live := !s.replaying.Load()
	switch ev.Kind {
	case runlog.Line:
		if live {
			s.triggers.Feed(ev.Text)
		}
		return
	case runlog.SubscriptionStarted:
		s.mu.Lock()
		s.currentSub = ev.Name
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Kind {
	case runlog.RunStarted:
		if s.current != nil { // a new start while one was open: the container was recreated mid-run
			r := *s.current
			r.Result, r.End = "killed", ev.Time.UTC().Format(timeFmt)
			s.appendRunLocked(r)
		}
		s.current = &model.Run{Instance: s.cfg.Instance, Start: ev.Time.UTC().Format(timeFmt), Result: "running"}
		s.rows, s.currentSub = nil, ""
		if live {
			snap := s.archives
			s.startSnap = snap
		}
	case runlog.SummaryRow:
		s.rows = append(s.rows, ev)
	case runlog.SummaryTotal:
		if s.current != nil {
			a, r, k := ev.Added, ev.Removed, ev.Total
			s.current.Added, s.current.Removed, s.current.Kept, s.current.Summary, s.current.Source = &a, &r, &k, ev.Verdict, "summary"
		}
	case runlog.RunEnded:
		if s.current == nil { // an end without a start (log truncated mid-run): record what we know
			s.current = &model.Run{Instance: s.cfg.Instance, Start: ev.Time.UTC().Format(timeFmt)}
		}
		r := *s.current
		r.End, r.Result = ev.Time.UTC().Format(timeFmt), ev.Result
		if st, err := time.Parse(timeFmt, r.Start); err == nil {
			m := ev.Time.Sub(st).Minutes()
			if m >= 0 {
				m = float64(int(m*10)) / 10
				r.Minutes = &m
			}
		}
		for _, row := range s.rows {
			s.verdicts[row.Name] = verdict{OK: row.OK, Added: row.Added, Removed: row.Removed, Kept: row.Total, At: r.End}
		}
		s.current, s.rows, s.currentSub = nil, nil, ""
		if live && r.Source == "" && r.Result != "lock" {
			go s.countFromArchives(r) // no summary printed: the archive diff fills added and removed
			return
		}
		s.appendRunLocked(r)
		if live {
			go s.afterRun()
		}
	}
}

func (s *Service) appendRunLocked(r model.Run) {
	s.runs = append(s.runs, r)
	if len(s.runs) > 500 {
		s.runs = s.runs[len(s.runs)-500:]
	}
	if !s.replaying.Load() {
		go s.saveState()
	}
}

// countFromArchives rescans and fills a finished run's added and removed from the archive diff against the snapshot
// taken when it started.
func (s *Service) countFromArchives(r model.Run) {
	s.rescan()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startSnap != nil {
		var a, rm int
		for _, d := range archive.Diff(s.startSnap, s.archives) {
			a += d.Added
			rm += d.Removed
		}
		k := 0
		for _, e := range s.archives {
			if s.subscribed(e.Name) {
				k += len(e.IDs)
			}
		}
		r.Added, r.Removed, r.Kept, r.Source = &a, &rm, &k, "archive"
	} else {
		r.Source = "none"
	}
	s.appendRunLocked(r)
	go s.refreshFast(context.Background())
}

func (s *Service) afterRun() {
	s.loadSubs() // first, so a subscription added since the last run is measured by the rescan
	s.rescan()
	s.refreshFast(context.Background())
}

func (s *Service) subscribed(name string) bool {
	for _, sub := range s.subs {
		if sub.Name == name {
			return true
		}
	}
	return false
}

// refreshSlow rescans the archives (seconds on a big library) and reloads the subscriptions.
func (s *Service) refreshSlow(ctx context.Context, scan bool) {
	s.loadSubs()
	if scan {
		s.rescan()
	}
	s.refreshFast(ctx)
}

// refreshFast asks the container and the exit provider, both quick.
func (s *Service) refreshFast(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if s.docker != nil {
		st, err := s.docker.Inspect(ctx, s.cfg.Container)
		s.mu.Lock()
		if err == nil {
			s.container, s.containerE = &st, nil
		} else {
			s.containerE = err
		}
		s.mu.Unlock()
		if s.ytdlVersion() == "" && s.cfg.VersionCommand != "" {
			if out, err := s.docker.ExecOutput(ctx, s.cfg.Container, strings.Fields(s.cfg.VersionCommand)); err == nil {
				v := strings.TrimSpace(out)
				if i := strings.LastIndex(v, " "); i >= 0 && strings.HasPrefix(v, "ytdl-sub") {
					v = v[i+1:]
				}
				s.mu.Lock()
				s.ytdlVer = v
				s.mu.Unlock()
			}
		}
	}
	if s.gluetun != nil {
		e, err := s.gluetun.PublicIP(ctx)
		s.mu.Lock()
		s.exit, s.exitErr = e, err
		s.mu.Unlock()
	}
}

func (s *Service) ytdlVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ytdlVer
}

// rescan reads the archives of the current subscriptions. Several instances may share one library, so only this
// instance's shows are measured, and the diff that counts a run's downloads cannot pick up another instance's.
func (s *Service) rescan() {
	s.mu.RLock()
	names := make([]string, 0, len(s.subs))
	for _, sub := range s.subs {
		names = append(names, sub.Name)
	}
	s.mu.RUnlock()
	t0 := time.Now()
	got, err := archive.Scan(s.cfg.MediaDir, names, time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.setWarning("archives", fmt.Sprintf("archive scan of %s failed: %v", s.cfg.MediaDir, err))
		return
	}
	s.archives, s.scannedAt, s.scanTook = got, time.Now(), time.Since(t0)
	s.clearWarning("archives")
	log.Printf("archives: %d of %d subscriptions under %s in %.1fs", len(got), len(names), s.cfg.MediaDir, s.scanTook.Seconds())
}

// loadSubs reads the subscriptions the way the container sees them when the socket is there (the file may be
// bind-mounted from elsewhere), else from the file in CONFIG_DIR.
func (s *Service) loadSubs() {
	var list []subs.Subscription
	var err error
	source := s.cfg.SubsFile
	if s.docker != nil && s.cfg.Container != "" && s.cfg.SubsInContainer != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, e := s.docker.ExecOutput(ctx, s.cfg.Container, []string{"cat", s.cfg.SubsInContainer})
		cancel()
		if e == nil {
			list, err = subs.Parse([]byte(out))
			source = s.cfg.Container + ":" + s.cfg.SubsInContainer
		} else {
			log.Printf("subscriptions: %v; reading %s instead", e, s.cfg.SubsFile)
		}
	}
	if list == nil && err == nil {
		list, err = subs.ParseFile(s.cfg.SubsFile)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err != nil:
		s.subsErr = err.Error()
		s.setWarning("subscriptions", fmt.Sprintf("subscriptions: %v", err))
		return
	case len(list) == 0:
		s.setWarning("subscriptions", "subscriptions: no entries found in "+source)
	default:
		s.clearWarning("subscriptions")
	}
	if len(list) != len(s.subs) {
		log.Printf("subscriptions: %d from %s", len(list), source)
	}
	s.subs, s.subsErr = list, ""
}

func (s *Service) setWarning(key, msg string) {
	s.clearWarning(key)
	s.warnings = append(s.warnings, key+": "+msg)
}

func (s *Service) clearWarning(key string) {
	var kept []string
	for _, w := range s.warnings {
		if !strings.HasPrefix(w, key+": ") {
			kept = append(kept, w)
		}
	}
	s.warnings = kept
}

func (s *Service) statePath() string { return filepath.Join(s.cfg.StateDir, "state.json") }

func (s *Service) loadState() {
	b, err := os.ReadFile(s.statePath())
	if err != nil {
		return
	}
	var p persisted
	if json.Unmarshal(b, &p) != nil {
		return
	}
	s.mu.Lock()
	s.runs = p.Runs
	if p.Verdicts != nil {
		s.verdicts = p.Verdicts
	}
	s.mu.Unlock()
}

// saveState writes the run history; replayed runs merge with it by start time so a truncated log loses nothing.
func (s *Service) saveState() {
	s.mu.Lock()
	byStart := map[string]model.Run{}
	for _, r := range s.runs {
		byStart[r.Start] = r
	}
	var runs []model.Run
	for _, r := range byStart {
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Start < runs[j].Start })
	if len(runs) > 500 {
		runs = runs[len(runs)-500:]
	}
	s.runs = runs
	p := persisted{Runs: runs, Verdicts: s.verdicts}
	s.mu.Unlock()
	if err := os.MkdirAll(s.cfg.StateDir, 0o755); err != nil {
		return
	}
	b, _ := json.Marshal(p)
	tmp := s.statePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.statePath())
	}
}

// Instance is the card.
func (s *Service) Instance() model.Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := model.Instance{Instance: s.cfg.Instance, Host: s.cfg.Host, Mode: "direct", Agent: s.cfg.Version, YtdlSub: s.ytdlVer}
	if s.gluetun != nil {
		i.Mode = "vpn"
		switch {
		case s.exitErr == nil && s.exit.IP != "":
			i.Exit, i.ExitIP = s.exit.String(), s.exit.IP
			if i.Exit == "" {
				i.Exit = "connected"
			}
		default:
			i.Exit = "not connected"
		}
	} else {
		i.Exit = s.cfg.ExitLabel
	}
	if s.container != nil {
		i.Status, i.Health = s.container.Status, s.container.Health
		if !s.container.StartedAt.IsZero() {
			i.Since = s.container.StartedAt.UTC().Format(timeFmt)
		}
	}
	if i.Health == "" { // no healthcheck or no socket: a run succeeded recently means healthy
		i.Health = s.healthFromMarkerLocked()
	}
	chans := s.channelsLocked()
	for _, c := range chans {
		i.Channels++
		if c.Preset == "keep" || strings.Contains(strings.Join(c.Presets, " "), "keep") {
			i.Keep++
		}
		i.GB += c.GB
		i.GB30 += c.GB30
		i.Videos30 += c.Files30
		if c.Status == "silent" || c.Status == "never" || c.Status == "failed" {
			i.Silent++
		}
	}
	i.GB, i.GB30 = round1(i.GB), round1(i.GB30)
	all := append([]model.Run{}, s.runs...)
	if s.current != nil {
		all = append(all, *s.current)
	}
	dayAgo := time.Now().Add(-24 * time.Hour).UTC().Format(timeFmt)
	var done []model.Run
	for _, r := range all {
		if r.Result == "ok" || r.Result == "error" || r.Result == "timeout" {
			done = append(done, r)
		}
		if r.Start >= dayAgo {
			i.Runs24h++
			if r.Result == "error" || r.Result == "timeout" || r.Result == "killed" {
				i.Failed24h++
			}
		}
	}
	if len(all) > 0 {
		last := all[len(all)-1]
		i.LastStart, i.LastResult = last.Start, last.Result
	} else {
		i.LastResult = "none"
	}
	if len(done) > 0 {
		ld := done[len(done)-1]
		i.LastEnd, i.LastMinutes, i.LastAdded, i.LastRemoved = ld.End, ld.Minutes, ld.Added, ld.Removed
		var sum float64
		var n int
		for _, r := range done {
			if r.Minutes != nil {
				sum += *r.Minutes
				n++
			}
		}
		if n > 0 {
			avg := round1(sum / float64(n))
			i.AvgMinutes = &avg
		}
	}
	i.Capabilities = s.capabilities()
	return i
}

func (s *Service) healthFromMarkerLocked() string {
	st, err := os.Stat(filepath.Join(s.cfg.ConfigDir, ".last-success"))
	switch {
	case err == nil && time.Since(st.ModTime()) < s.cfg.HealthMaxAge:
		return "healthy"
	case err == nil:
		return "unhealthy"
	case len(s.runs) == 0:
		return "unknown"
	default:
		return "unhealthy"
	}
}

func (s *Service) capabilities() []string {
	if s.cfg.Token == "" {
		return nil
	}
	caps := []string{"rescan"}
	if s.docker != nil && s.cfg.Container != "" {
		caps = append(caps, "run", "stop")
	}
	if s.gluetun != nil {
		caps = append(caps, "reconnect")
	}
	return caps
}

// Channels lists the subscriptions with what the disk and the last summary say about each.
func (s *Service) Channels() []model.Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.channelsLocked()
}

func (s *Service) channelsLocked() []model.Channel {
	now := time.Now()
	out := make([]model.Channel, 0, len(s.subs))
	for _, sub := range s.subs {
		c := model.Channel{Name: sub.Name, URL: sub.URL, Instance: s.cfg.Instance, Presets: sub.Presets, Handle: subs.Handle(sub.URL)}
		if len(sub.Presets) > 0 {
			c.Preset = sub.Presets[0]
		}
		if len(sub.Values) > 0 {
			c.Group = sub.Values[0]
		}
		if len(sub.Values) > 1 {
			c.Rating = sub.Values[1]
		}
		if e := s.archives[sub.Name]; e != nil {
			c.Folder, c.Kept, c.LastUpload = true, len(e.IDs), e.LastUpload()
			c.Files, c.Files30, c.Files90 = e.Files, e.Files30, e.Files90
			c.GB, c.GB30 = round2(float64(e.Bytes)/1e9), round2(float64(e.Bytes30)/1e9)
			if !e.Newest.IsZero() {
				c.LastFile = e.Newest.UTC().Format("2006-01-02")
				d := int(now.Sub(e.Newest).Hours() / 24)
				c.DaysSinceFile = &d
			}
		}
		if v, ok := s.verdicts[sub.Name]; ok {
			a, r := v.Added, v.Removed
			c.CheckedAt, c.CheckAdded, c.CheckRemoved = v.At, &a, &r
			if v.OK {
				c.Check = "ok"
			} else {
				c.Check = "failed"
			}
		}
		switch {
		case c.Check == "failed":
			c.Status = "failed"
		case c.Check == "" && len(s.verdicts) > 0:
			c.Status = "unchecked"
		case c.DaysSinceFile == nil:
			c.Status = "never"
		case *c.DaysSinceFile >= 90:
			c.Status = "silent"
		case *c.DaysSinceFile >= 30:
			c.Status = "quiet"
		default:
			c.Status = "active"
		}
		out = append(out, c)
	}
	return out
}

// Runs returns the history, oldest first, the open run last.
func (s *Service) Runs(limit int) []model.Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := append([]model.Run{}, s.runs...)
	if s.current != nil {
		all = append(all, *s.current)
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all
}

// Daily counts the kept videos of the subscribed channels by upload day, for the last n days.
func (s *Service) Daily(days int) []model.DailyRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts := map[string]int{}
	for _, sub := range s.subs {
		if e := s.archives[sub.Name]; e != nil {
			for _, u := range e.Uploads {
				counts[u]++
			}
		}
	}
	today := time.Now().UTC()
	out := make([]model.DailyRow, 0, days)
	for i := days - 1; i >= 0; i-- {
		d := today.AddDate(0, 0, -i)
		out = append(out, model.DailyRow{"day": d.Format("2006-01-02"), s.cfg.Instance: counts[d.Format("20060102")]})
	}
	return out
}

// Status is the agent's own summary.
func (s *Service) Status() model.Status {
	inst := s.Instance()
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := model.Meta{Generated: time.Now().UTC().Format(timeFmt), Seconds: round1(s.scanTook.Seconds()), Warnings: append([]string{}, s.warnings...)}
	if s.containerE != nil {
		m.Warnings = append(m.Warnings, "container: "+s.containerE.Error())
	}
	if s.gluetun != nil && s.exitErr != nil {
		m.Warnings = append(m.Warnings, "gluetun: "+s.exitErr.Error())
	}
	if m.Warnings == nil {
		m.Warnings = []string{}
	}
	return model.Status{Instance: inst, Running: s.current != nil, Meta: m}
}

// Current is the subscription being processed, or "".
func (s *Service) Current() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentSub
}

// RunNow starts a run in the ytdl-sub container.
func (s *Service) RunNow(ctx context.Context) error {
	if s.docker == nil || s.cfg.Container == "" {
		return fmt.Errorf("run needs YTDL_CONTAINER and the docker socket")
	}
	s.mu.RLock()
	running := s.current != nil
	s.mu.RUnlock()
	if running {
		return fmt.Errorf("a run is already going")
	}
	return s.docker.ExecDetached(ctx, s.cfg.Container, shellWords(s.cfg.RunCommand), s.cfg.RunUser)
}

// StopRun ends the running ytdl-sub process.
func (s *Service) StopRun(ctx context.Context) error {
	if s.docker == nil || s.cfg.Container == "" {
		return fmt.Errorf("stop needs YTDL_CONTAINER and the docker socket")
	}
	_, err := s.docker.ExecOutput(ctx, s.cfg.Container, shellWords(s.cfg.StopCommand))
	return err
}

// Reconnect cycles the gluetun tunnel.
func (s *Service) Reconnect(ctx context.Context) (string, error) {
	if s.gluetun == nil {
		return "", fmt.Errorf("no gluetun configured")
	}
	ip, err := s.gluetun.Reconnect(ctx)
	go s.refreshFast(context.Background())
	return ip, err
}

// Rescan rereads the disk now.
func (s *Service) Rescan() { s.refreshSlow(context.Background(), true) }

// LogTail returns the last n plain lines of the cron log.
func (s *Service) LogTail(n int) string {
	b, err := os.ReadFile(s.cfg.LogFile)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		l := strings.TrimRight(stripANSI(lines[i]), "\r")
		if l == "" || strings.HasPrefix(l, "+") {
			continue
		}
		keep = append(keep, l)
	}
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}
	return strings.Join(keep, "\n")
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// shellWords splits a command the way a simple shell would: spaces separate, single or double quotes group.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

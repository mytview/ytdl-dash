package runlog

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func feedFile(t *testing.T, path string) []Event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &Parser{Now: func() time.Time { return time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC) }}
	var all []Event
	for _, l := range strings.SplitAfter(string(b), "\n") {
		all = append(all, p.Feed(l)...)
	}
	return all
}

func TestSample(t *testing.T) {
	evs := feedFile(t, "../../testdata/cron-sample.log")
	var starts, ends, rows, totals, subs, lines int
	var results []string
	var crossed []string
	for _, e := range evs {
		switch e.Kind {
		case RunStarted:
			starts++
		case RunEnded:
			ends++
			results = append(results, e.Result)
			if e.Result == "error" && e.Code != 1 {
				t.Errorf("error code %d", e.Code)
			}
			if e.Time.IsZero() || e.Time.Year() != 2026 {
				t.Errorf("end time %v", e.Time)
			}
		case SummaryRow:
			rows++
			if !e.OK {
				crossed = append(crossed, e.Name)
			}
			if e.Name == "Fireship" && (e.Added != 2 || e.Removed != 1 || e.Total != 12) {
				t.Errorf("Fireship row %+v", e)
			}
		case SummaryTotal:
			totals++
			if e.Verdict == "Error" && (e.Count != 3 || e.Added != 2 || e.Removed != 1 || e.Total != 13) {
				t.Errorf("total %+v", e)
			}
		case SubscriptionStarted:
			subs++
		case Line:
			lines++
			if strings.HasPrefix(e.Text, "+") {
				t.Errorf("trace line leaked: %q", e.Text)
			}
		}
	}
	if starts != 5 || ends != 4 {
		t.Errorf("starts %d ends %d", starts, ends)
	}
	if strings.Join(results, ",") != "error,lock,timeout,ok" {
		t.Errorf("results %v", results)
	}
	if rows != 4 || totals != 2 || subs < 3 {
		t.Errorf("rows %d totals %d subs %d", rows, totals, subs)
	}
	if len(crossed) != 1 || crossed[0] != "Dead Channel" {
		t.Errorf("crossed %v", crossed)
	}
	if lines == 0 {
		t.Error("no plain lines")
	}
	// the trigger string must be visible on its plain line and never on the trace line that quotes it
	var seen bool
	for _, e := range evs {
		if e.Kind == Line && strings.Contains(e.Text, "Sign in to confirm") {
			seen = true
		}
	}
	if !seen {
		t.Error("trigger line not emitted")
	}
}

func TestStartMarkerNamesACheck(t *testing.T) {
	p := &Parser{}
	var starts []Event
	for _, l := range []string{"Cron started, running ytdl-sub... (2026-10-29T10:10:00Z) for \"Jimmy Kimmel\"\n", "Cron started, running ytdl-sub... (2026-10-29T12:10:00Z)\n"} {
		for _, e := range p.Feed(l) {
			if e.Kind == RunStarted {
				starts = append(starts, e)
			}
		}
	}
	if len(starts) != 2 || starts[0].Name != "Jimmy Kimmel" || starts[0].Time.UTC().Format("15:04") != "10:10" || starts[1].Name != "" {
		t.Errorf("%+v", starts)
	}
}

func TestTailerReplayAndFollow(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/cron.log"
	if err := os.WriteFile(path, []byte("Cron started, running ytdl-sub... (2026-10-27T00:10:00Z)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []Event
	events := func() []Event { mu.Lock(); defer mu.Unlock(); return append([]Event(nil), got...) }
	tl := &Tailer{Path: path, Interval: 20 * time.Millisecond, Parser: &Parser{}, Handle: func(e Event) {
		if e.Kind != Line {
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
		}
	}}
	if err := tl.Replay(); err != nil {
		t.Fatal(err)
	}
	if len(events()) != 1 || events()[0].Kind != RunStarted {
		t.Fatalf("replay got %+v", events())
	}
	go tl.Follow()
	defer tl.Stop()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("ytdl-sub finished OK (2026-10-27T00:20:00Z)\n")
	f.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(events()) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(events()) != 2 || events()[1].Kind != RunEnded || events()[1].Result != "ok" {
		t.Fatalf("follow got %+v", events())
	}
	// truncation: a recreated container starts a fresh log
	os.WriteFile(path, []byte("Cron started, running ytdl-sub... (2026-10-27T02:10:00Z)\n"), 0o644)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(events()) == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(events()) != 3 || events()[2].Kind != RunStarted {
		t.Fatalf("after truncation got %+v", events())
	}
}

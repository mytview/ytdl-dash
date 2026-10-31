// Package runlog reads ytdl-sub's cron log (the file the LinuxServer cron mod writes, /config/.cron.log) and turns it into
// events: runs starting and ending, the subscription being processed, the rows of the Download Summary, and every plain
// line for the trigger matcher. The run markers are the ones deploy/instance/cron.sh prints; the summary rows are ytdl-sub's own
// output and the one part that can change with a ytdl-sub release, so the parser is kept in one place and tested against
// real samples in testdata.
package runlog

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind is the type of an Event.
type Kind int

const (
	RunStarted Kind = iota
	RunEnded
	SubscriptionStarted
	SummaryRow
	SummaryTotal
	Line
)

// Event is one thing the log said.
type Event struct {
	Kind    Kind
	Time    time.Time // from the marker's own timestamp when it has one, else the parser's clock
	Result  string    // RunEnded: ok, error, lock, timeout
	Code    int       // RunEnded error: ytdl-sub's exit code
	Name    string    // SubscriptionStarted, SummaryRow; RunStarted: the one subscription a check targeted, else empty
	Added   int       // SummaryRow, SummaryTotal
	Changed int
	Removed int
	Total   int
	OK      bool   // SummaryRow: a tick
	Count   int    // SummaryTotal: subscriptions in the run
	Verdict string // SummaryTotal: Success, Error...
	Text    string // Line: the ANSI-stripped line
}

var (
	ansi         = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	reStarted    = regexp.MustCompile(`^Cron started, running ytdl-sub\.\.\.(?: \((\S+)\))?(?: for "(.+)")?`) // the wrapper names a one-subscription check
	reOK         = regexp.MustCompile(`^ytdl-sub finished OK(?: \((\S+)\))?`)
	reTimeout    = regexp.MustCompile(`^RUN TIMED OUT:.*?(?:\((\S+)\))?`)
	reLock       = regexp.MustCompile(`^ytdl-sub skipped: the previous run still holds the lock(?: \((\S+)\))?`)
	reErrors     = regexp.MustCompile(`^ytdl-sub finished with errors, code (\d+)(?: \((\S+)\))?`)
	reSubStart   = regexp.MustCompile(`^\[ytdl-sub\] Beginning subscription download for (.+?)\.?\s*$`)
	reNoChange   = regexp.MustCompile(`^No files changed for (.+?)\s*$`)
	reSummary    = regexp.MustCompile(`^\[ytdl-sub\] Download Summary:`)
	reRow        = regexp.MustCompile(`^(.*?)\s+([+-]?\d+)\s+(\d+)\s+(-?\d+)\s+(\d+)\s+(✔|✘)\s*$`)
	reTotal      = regexp.MustCompile(`^Total:\s+(\d+)\s+([+-]?\d+)\s+(\d+)\s+(-?\d+)\s+(\d+)\s+(\S+)`)
	reTimestampS = `\((\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)\)`
	reTimestamp  = regexp.MustCompile(reTimestampS)
)

// Parser turns lines into events. Feed lines in order; it keeps the little state a summary needs.
type Parser struct {
	Now       func() time.Time
	inSummary bool
}

// Feed parses one line. Bash trace lines (the cron mod runs the script with -x) are dropped, so a trigger never fires
// on the script quoting its own strings.
func (p *Parser) Feed(raw string) []Event {
	line := strings.TrimRight(ansi.ReplaceAllString(raw, ""), "\r\n\t ")
	if line == "" || strings.HasPrefix(line, "+") {
		return nil
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	at := func(ts string) time.Time {
		if t, err := time.Parse("2006-01-02T15:04:05Z", ts); err == nil {
			return t
		}
		if m := reTimestamp.FindStringSubmatch(line); m != nil {
			if t, err := time.Parse("2006-01-02T15:04:05Z", m[1]); err == nil {
				return t
			}
		}
		return now().UTC()
	}
	var ev []Event
	switch {
	case reStarted.MatchString(line):
		p.inSummary = false
		m := reStarted.FindStringSubmatch(line)
		ev = append(ev, Event{Kind: RunStarted, Time: at(m[1]), Name: m[2]})
	case reOK.MatchString(line):
		p.inSummary = false
		ev = append(ev, Event{Kind: RunEnded, Result: "ok", Time: at(reOK.FindStringSubmatch(line)[1])})
	case reTimeout.MatchString(line):
		p.inSummary = false
		ev = append(ev, Event{Kind: RunEnded, Result: "timeout", Time: at("")})
	case reLock.MatchString(line):
		p.inSummary = false
		ev = append(ev, Event{Kind: RunEnded, Result: "lock", Time: at(reLock.FindStringSubmatch(line)[1])})
	case reErrors.MatchString(line):
		p.inSummary = false
		m := reErrors.FindStringSubmatch(line)
		code, _ := strconv.Atoi(m[1])
		ev = append(ev, Event{Kind: RunEnded, Result: "error", Code: code, Time: at(m[2])})
	case reSummary.MatchString(line):
		p.inSummary = true
	case p.inSummary && reTotal.MatchString(line):
		m := reTotal.FindStringSubmatch(line)
		p.inSummary = false
		ev = append(ev, Event{Kind: SummaryTotal, Time: now().UTC(), Count: atoi(m[1]), Added: atoi(m[2]), Changed: atoi(m[3]), Removed: abs(atoi(m[4])), Total: atoi(m[5]), Verdict: m[6]})
	case p.inSummary && reRow.MatchString(line):
		m := reRow.FindStringSubmatch(line)
		ev = append(ev, Event{Kind: SummaryRow, Time: now().UTC(), Name: strings.TrimSpace(m[1]), Added: atoi(m[2]), Changed: atoi(m[3]), Removed: abs(atoi(m[4])), Total: atoi(m[5]), OK: m[6] == "✔"})
	case reSubStart.MatchString(line):
		ev = append(ev, Event{Kind: SubscriptionStarted, Time: now().UTC(), Name: reSubStart.FindStringSubmatch(line)[1]})
	case reNoChange.MatchString(line):
		ev = append(ev, Event{Kind: SubscriptionStarted, Time: now().UTC(), Name: reNoChange.FindStringSubmatch(line)[1]})
	}
	ev = append(ev, Event{Kind: Line, Time: now().UTC(), Text: line})
	return ev
}

func atoi(s string) int { n, _ := strconv.Atoi(strings.TrimPrefix(s, "+")); return n }
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Tailer follows a log file: it replays what exists, then polls for appended bytes, and starts over when the file is
// truncated or replaced (the cron mod rotates nothing, but a container recreation may). No inotify dependency: a poll
// every second is cheap and works on any mount, NFS included.
type Tailer struct {
	Path     string
	Interval time.Duration
	Parser   *Parser
	Handle   func(Event)

	mu     sync.Mutex
	offset int64
	stop   chan struct{}
}

// Replay reads the whole file once through the parser and returns the offset reached.
func (t *Tailer) Replay() error {
	f, err := os.Open(t.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := t.consume(f)
	t.mu.Lock()
	t.offset = n
	t.mu.Unlock()
	return err
}

func (t *Tailer) consume(r io.Reader) (int64, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var n int64
	for {
		line, err := br.ReadString('\n')
		n += int64(len(line))
		if len(line) > 0 && (err == nil || err == io.EOF && strings.HasSuffix(line, "\n")) {
			for _, ev := range t.Parser.Feed(line) {
				t.Handle(ev)
			}
		} else if len(line) > 0 && err == io.EOF {
			// a partial last line: leave it for the next poll
			n -= int64(len(line))
		}
		if err != nil {
			if err == io.EOF {
				return n, nil
			}
			return n, err
		}
	}
}

// Follow polls until Stop is called. It never returns an error: a missing file is retried, since the log only appears
// after the first cron run.
// stopCh hands out the stop channel, made once under the lock, so Stop may come from another goroutine, before
// Follow, or twice.
func (t *Tailer) stopCh() chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stop == nil {
		t.stop = make(chan struct{})
	}
	return t.stop
}

func (t *Tailer) Follow() {
	stop := t.stopCh()
	iv := t.Interval
	if iv <= 0 {
		iv = time.Second
	}
	tick := time.NewTicker(iv)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			t.poll()
		}
	}
}

func (t *Tailer) poll() {
	st, err := os.Stat(t.Path)
	if err != nil {
		return
	}
	t.mu.Lock()
	off := t.offset
	t.mu.Unlock()
	if st.Size() < off {
		off = 0 // truncated or replaced: start again
	}
	if st.Size() == off {
		return
	}
	f, err := os.Open(t.Path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	n, _ := t.consume(f)
	t.mu.Lock()
	t.offset = off + n
	t.mu.Unlock()
}

// Stop ends Follow.
func (t *Tailer) Stop() {
	stop := t.stopCh()
	select {
	case <-stop:
	default:
		close(stop)
	}
}

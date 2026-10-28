// Package actions is the trigger side of the agent, the part that used to be docker-log-watchdog: a substring seen in
// the log fires an action, no more than once per cooldown. Actions are deliberately generic so no VPN or notifier is
// assumed: gluetun-reconnect, restart:<container>, webhook:<url>, notify:<url>, log.
package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Trigger is one rule.
type Trigger struct {
	Match    string
	Action   string // gluetun-reconnect | restart:<container> | webhook:<url> | notify:<url> | log
	Cooldown time.Duration
	last     time.Time
}

// Hooks are what the actions call into; nil hooks make their action a logged no-op.
type Hooks struct {
	Instance  string
	Reconnect func(ctx context.Context) (string, error)
	Restart   func(ctx context.Context, container string) error
	HTTP      *http.Client
	Log       func(string, ...any)
}

// Runner holds the triggers and fires them.
type Runner struct {
	Triggers []*Trigger
	Hooks    Hooks
	mu       sync.Mutex
	running  map[int]bool
}

// ParseEnv reads triggers from the environment:
//
//	TRIGGERS="Sign in to confirm => gluetun-reconnect; HTTP Error 429 => notify:https://ntfy.sh/mytopic"
//	TRIGGER_COOLDOWN=300
//
// and, for docker-log-watchdog users, the old TRIGGER_STRINGS (comma-separated) with RESTART_TARGET and RESTART_COOLDOWN.
func ParseEnv() ([]*Trigger, error) {
	cooldown := 300 * time.Second
	if v := firstEnv("TRIGGER_COOLDOWN", "RESTART_COOLDOWN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("TRIGGER_COOLDOWN: %w", err)
		}
		cooldown = time.Duration(n) * time.Second
	}
	var out []*Trigger
	if spec := os.Getenv("TRIGGERS"); strings.TrimSpace(spec) != "" {
		for _, item := range strings.Split(spec, ";") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			parts := strings.SplitN(item, "=>", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("TRIGGERS: %q needs 'match => action'", item)
			}
			t := &Trigger{Match: strings.TrimSpace(parts[0]), Action: strings.TrimSpace(parts[1]), Cooldown: cooldown}
			if err := t.validate(); err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, nil
	}
	if legacy := os.Getenv("TRIGGER_STRINGS"); strings.TrimSpace(legacy) != "" {
		action := os.Getenv("RESTART_TARGET")
		switch {
		case action == "" || action == "stack":
			return nil, fmt.Errorf("RESTART_TARGET %q: the agent restarts one named container (restart:<name>) or reconnects gluetun; set TRIGGERS instead", action)
		case action == "gluetun-reconnect":
		default:
			action = "restart:" + action
		}
		for _, m := range strings.Split(legacy, ",") {
			if m = strings.TrimSpace(m); m != "" {
				out = append(out, &Trigger{Match: m, Action: action, Cooldown: cooldown})
			}
		}
	}
	return out, nil
}

func (t *Trigger) validate() error {
	switch {
	case t.Match == "":
		return fmt.Errorf("a trigger needs a match string")
	case t.Action == "gluetun-reconnect", t.Action == "log":
	case strings.HasPrefix(t.Action, "restart:") && len(t.Action) > 8:
	case strings.HasPrefix(t.Action, "webhook:http"), strings.HasPrefix(t.Action, "notify:http"):
	default:
		return fmt.Errorf("trigger %q: unknown action %q", t.Match, t.Action)
	}
	return nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Feed checks one log line against every trigger and fires the matching ones in the background.
func (r *Runner) Feed(line string) {
	for i, t := range r.Triggers {
		if !strings.Contains(line, t.Match) {
			continue
		}
		r.mu.Lock()
		if r.running == nil {
			r.running = map[int]bool{}
		}
		if r.running[i] || (!t.last.IsZero() && time.Since(t.last) < t.Cooldown) {
			r.mu.Unlock()
			continue
		}
		t.last = time.Now() // claimed up front: a failing action must not retry on every matching line
		r.running[i] = true
		r.mu.Unlock()
		go func(i int, t *Trigger, line string) {
			defer func() { r.mu.Lock(); r.running[i] = false; r.mu.Unlock() }()
			r.fire(t, line)
		}(i, t, line)
	}
}

func (r *Runner) fire(t *Trigger, line string) {
	h := r.Hooks
	logf := h.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	logf("trigger %q matched, action %s", t.Match, t.Action)
	switch {
	case t.Action == "log":
	case t.Action == "gluetun-reconnect":
		if h.Reconnect == nil {
			logf("gluetun-reconnect: no gluetun configured (GLUETUN_API_URL)")
			return
		}
		if ip, err := h.Reconnect(ctx); err != nil {
			logf("gluetun-reconnect failed: %v", err)
		} else {
			logf("gluetun-reconnect: new address %s", ip)
		}
	case strings.HasPrefix(t.Action, "restart:"):
		if h.Restart == nil {
			logf("restart: no docker socket configured")
			return
		}
		name := strings.TrimPrefix(t.Action, "restart:")
		if err := h.Restart(ctx, name); err != nil {
			logf("restart %s failed: %v", name, err)
		} else {
			logf("restarted %s", name)
		}
	case strings.HasPrefix(t.Action, "webhook:"), strings.HasPrefix(t.Action, "notify:"):
		r.post(ctx, t, line, logf)
	}
}

func (r *Runner) post(ctx context.Context, t *Trigger, line string, logf func(string, ...any)) {
	client := r.Hooks.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	kind, target, _ := strings.Cut(t.Action, ":")
	var body []byte
	ctype := "application/json"
	text := fmt.Sprintf("ytdl-sub %s: %q matched\n%s", r.Hooks.Instance, t.Match, line)
	switch {
	case kind == "webhook" && !strings.Contains(target, "discord.com/api/webhooks"):
		body, _ = json.Marshal(map[string]string{"instance": r.Hooks.Instance, "trigger": t.Match, "line": line, "time": time.Now().UTC().Format(time.RFC3339)})
	case strings.Contains(target, "discord.com/api/webhooks"):
		body, _ = json.Marshal(map[string]string{"content": text})
	default: // notify: ntfy and friends take plain text
		body, ctype = []byte(text), "text/plain"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		logf("%s: %v", kind, err)
		return
	}
	req.Header.Set("Content-Type", ctype)
	if kind == "notify" {
		req.Header.Set("Title", "ytdl-sub "+r.Hooks.Instance)
	}
	resp, err := client.Do(req)
	if err != nil {
		logf("%s %s failed: %v", kind, target, err)
		return
	}
	resp.Body.Close()
	logf("%s %s: %d", kind, target, resp.StatusCode)
}

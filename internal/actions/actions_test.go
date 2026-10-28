package actions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseEnvNewAndLegacy(t *testing.T) {
	t.Setenv("TRIGGERS", "Sign in to confirm => gluetun-reconnect; HTTP Error 429 => notify:https://ntfy.sh/x ; oops => restart:ytdlsub-main")
	t.Setenv("TRIGGER_COOLDOWN", "60")
	ts, err := ParseEnv()
	if err != nil || len(ts) != 3 || ts[0].Action != "gluetun-reconnect" || ts[1].Match != "HTTP Error 429" || ts[2].Action != "restart:ytdlsub-main" || ts[0].Cooldown != time.Minute {
		t.Fatalf("new syntax: %v %+v", err, ts)
	}
	os.Unsetenv("TRIGGERS")
	t.Setenv("TRIGGER_STRINGS", "Sign in to confirm you, Too Many Requests")
	t.Setenv("RESTART_TARGET", "gluetun-reconnect")
	ts, err = ParseEnv()
	if err != nil || len(ts) != 2 || ts[1].Action != "gluetun-reconnect" {
		t.Fatalf("legacy: %v %+v", err, ts)
	}
	t.Setenv("TRIGGERS", "x => dance")
	if _, err := ParseEnv(); err == nil {
		t.Error("unknown action accepted")
	}
}

func TestFeedFiresOncePerCooldown(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	var reconnects int32
	r := &Runner{Triggers: []*Trigger{{Match: "Sign in", Action: "gluetun-reconnect", Cooldown: time.Hour}, {Match: "Sign in", Action: "notify:" + srv.URL, Cooldown: time.Hour}},
		Hooks: Hooks{Instance: "t", Reconnect: func(context.Context) (string, error) { atomic.AddInt32(&reconnects, 1); return "1.2.3.4", nil }}}
	for i := 0; i < 5; i++ {
		r.Feed("ERROR: Sign in to confirm you're not a bot")
	}
	r.Feed("nothing here")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (atomic.LoadInt32(&hits) < 1 || atomic.LoadInt32(&reconnects) < 1) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&reconnects) != 1 || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("reconnects %d notifies %d", reconnects, hits)
	}
}

package dash

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mytview/ytdl-dash/internal/model"
)

// fakeAgent answers /api/all with one instance and, with the right bearer, the rescan command.
func fakeAgent(t *testing.T, instance string, got *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/all", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(answer{Instance: model.Instance{Instance: instance, Health: "healthy"},
			Channels: []model.Channel{{Name: "A", Instance: instance, Status: "active"}},
			Runs:     []model.Run{{Instance: instance, Start: "2026-10-29T10:10:00Z", Result: "ok"}},
			Daily:    []model.DailyRow{{"day": "2026-10-29", instance: 2}}})
	})
	mux.HandleFunc("POST /api/{cmd}", func(w http.ResponseWriter, r *http.Request) {
		*got = append(*got, instance+" "+r.PathValue("cmd")+" "+r.URL.RawQuery+" "+r.Header.Get("Authorization"))
		w.Write([]byte(`{"result":"rescanning"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestParseAgents(t *testing.T) {
	got := ParseAgents(" http://a:9100/, named=http://b:9101 ,, http://c:9102")
	if len(got) != 3 || got[0].Name != "" || got[0].URL != "http://a:9100" || got[1].Name != "named" || got[1].URL != "http://b:9101" || got[2].URL != "http://c:9102" {
		t.Errorf("%+v", got)
	}
}

// Bare URLs are enough: each card takes the agent's own instance name, an agent that never answered is labelled by its
// host and port, two agents with one instance name draw a warning, and a command finds its agent by instance name.
func TestMergeNamesAndProxy(t *testing.T) {
	var cmds []string
	alpha, beta, twin := fakeAgent(t, "alpha", &cmds), fakeAgent(t, "beta", &cmds), fakeAgent(t, "alpha", &cmds)
	d, err := New(Config{Agents: ParseAgents(alpha.URL + "," + beta.URL + ",http://127.0.0.1:1," + twin.URL), Token: "t", Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	d.poll(context.Background())

	var instances []model.Instance
	json.Unmarshal(d.files["instances.json"], &instances)
	var names []string
	for _, i := range instances {
		names = append(names, i.Instance+"/"+i.Health)
	}
	if strings.Join(names, ",") != "alpha/healthy,beta/healthy,127.0.0.1:1/unreachable,alpha/healthy" {
		t.Errorf("instances %v", names)
	}
	var meta model.Meta
	json.Unmarshal(d.files["meta.json"], &meta)
	if len(meta.Warnings) != 2 || !strings.HasPrefix(meta.Warnings[0], "127.0.0.1:1: agent unreachable") || !strings.HasPrefix(meta.Warnings[1], "alpha: two agents report this instance name") {
		t.Errorf("warnings %v", meta.Warnings)
	}
	if strings.Join(meta.Agents, ",") != "alpha,beta,127.0.0.1:1,alpha" {
		t.Errorf("agents %v", meta.Agents)
	}
	var daily []model.DailyRow
	json.Unmarshal(d.files["daily.json"], &daily)
	if len(daily) != 1 || daily[0]["alpha"] == nil || daily[0]["beta"] == nil {
		t.Errorf("daily %v", daily)
	}

	var sum model.Summary
	json.Unmarshal(d.files["summary.json"], &sum)
	if sum.Instances != 4 || sum.Healthy != 3 || sum.Unreachable != 1 || sum.Warnings != 2 {
		t.Errorf("summary %+v", sum)
	}

	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/api/beta/rescan", "", nil)
	if resp.StatusCode != http.StatusOK || len(cmds) != 1 || cmds[0] != "beta rescan  Bearer t" {
		t.Errorf("proxy: %d %v", resp.StatusCode, cmds)
	}
	resp, _ = http.Post(srv.URL+"/api/beta/run?sub=A%20B", "", nil) // a check of one subscription: the query goes through
	if resp.StatusCode != http.StatusOK || len(cmds) != 2 || cmds[1] != "beta run sub=A%20B Bearer t" {
		t.Errorf("proxy with a query: %d %v", resp.StatusCode, cmds)
	}
	resp, _ = http.Post(srv.URL+"/api/nobody/rescan", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown instance: %d", resp.StatusCode)
	}
}

func TestCommandsOffWithoutToken(t *testing.T) {
	var cmds []string
	a := fakeAgent(t, "alpha", &cmds)
	d, _ := New(Config{Agents: ParseAgents(a.URL), Poll: time.Hour})
	d.poll(context.Background())
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/api/alpha/rescan", "", nil)
	if resp.StatusCode != http.StatusForbidden || len(cmds) != 0 {
		t.Errorf("without a token: %d %v", resp.StatusCode, cmds)
	}
}

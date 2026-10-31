package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mytview/ytdl-dash/internal/model"
)

// Handler is the agent's HTTP API.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Status()) })
	mux.HandleFunc("GET /api/instance", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Instance()) })
	mux.HandleFunc("GET /api/channels", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Channels()) })
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Runs(intParam(r, "limit", 200))) })
	mux.HandleFunc("GET /api/daily", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Daily(intParam(r, "days", 30))) })
	mux.HandleFunc("GET /api/all", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.All()) })
	mux.HandleFunc("GET /api/log", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(s.LogTail(intParam(r, "tail", 200)) + "\n"))
	})
	mux.HandleFunc("POST /api/run", s.command(func(ctx context.Context, r *http.Request) (string, error) { // ?sub=NAME checks one subscription
		if sub := strings.TrimSpace(r.URL.Query().Get("sub")); sub != "" {
			return "check started for " + sub, s.RunNow(ctx, sub)
		}
		return "run started", s.RunNow(ctx, "")
	}))
	mux.HandleFunc("POST /api/stop", s.command(func(ctx context.Context, _ *http.Request) (string, error) { return "stop sent", s.StopRun(ctx) }))
	mux.HandleFunc("POST /api/reconnect", s.command(func(ctx context.Context, _ *http.Request) (string, error) {
		ip, err := s.Reconnect(ctx)
		return "reconnected, new address " + ip, err
	}))
	mux.HandleFunc("POST /api/rescan", s.command(func(ctx context.Context, _ *http.Request) (string, error) { s.Rescan(); return "rescanned", nil }))
	return cors(mux)
}

// All is one answer with everything the dashboard needs.
func (s *Service) All() map[string]any {
	st := s.Status()
	return map[string]any{"instance": st.Instance, "running": st.Running, "current": s.Current(), "channels": s.Channels(), "runs": s.Runs(200), "daily": s.Daily(30), "meta": st.Meta}
}

func (s *Service) command(fn func(ctx context.Context, r *http.Request) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" {
			http.Error(w, `{"error":"commands are off: set TOKEN on the agent"}`, http.StatusForbidden)
			return
		}
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != s.cfg.Token {
			http.Error(w, `{"error":"bad token"}`, http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		msg, err := fn(ctx, r)
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]string{"result": msg})
	}
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func intParam(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if v == nil {
		v = []model.Run{}
	}
	json.NewEncoder(w).Encode(v)
}

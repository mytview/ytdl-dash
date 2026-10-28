// The ytdl-dash dashboard: polls the agents and serves one page over all of them.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mytview/ytdl-dash/internal/dash"
)

var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	poll, err := time.ParseDuration(env("POLL", "60s"))
	if err != nil {
		log.Fatalf("POLL: %v", err)
	}
	d, err := dash.New(dash.Config{Agents: dash.ParseAgents(os.Getenv("AGENTS")), Token: strings.TrimSpace(os.Getenv("TOKEN")), Poll: poll, Title: strings.TrimSpace(os.Getenv("TITLE")), Version: version})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d.Start(ctx)
	listen := env("LISTEN", ":8089")
	srv := &http.Server{Addr: listen, Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("ytdl-sub dashboard %s listening on %s, polling every %s", version, listen, poll)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

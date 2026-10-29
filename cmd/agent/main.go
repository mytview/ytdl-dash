// The ytdl-sub agent: runs beside one ytdl-sub container, follows its cron log, reads its archives, fires triggers and
// answers the dashboard over HTTP. Configuration is by environment; see internal/agent/config.go and the README.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/mytview/ytdl-dash/internal/agent"
	"github.com/mytview/ytdl-dash/internal/archive"
	"github.com/mytview/ytdl-dash/internal/subs"
)

var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "scan":
			os.Exit(scanCmd(os.Args[2:]))
		case "version", "-version", "--version":
			fmt.Println("ytdl-agent " + version)
			return
		}
	}
	cfg, err := agent.ConfigFromEnv(version)
	if err != nil {
		log.Fatal(err)
	}
	svc, err := agent.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	svc.Start(ctx)
	srv := &http.Server{Addr: cfg.Listen, Handler: svc.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("ytdl-dash agent %s for instance %s listening on %s (log %s, media %s)", version, cfg.Instance, cfg.Listen, cfg.LogFile, cfg.MediaDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	os.Exit(0)
}

// scanCmd is "ytdl-agent scan [-subs FILE] ROOT": what the agent finds under ROOT and how long the share takes to
// answer, for checking MEDIA_DIR and the mount before running the agent. With a subscriptions file it measures only
// those shows and names the ones that have no archive yet.
func scanCmd(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	subsFile := fs.String("subs", "", "subscriptions.yaml: measure only its subscriptions and list the ones without an archive")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ytdl-agent scan [-subs subscriptions.yaml] MEDIA_DIR")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		if err == nil {
			fs.Usage()
		}
		return 2
	}
	var names []string
	if *subsFile != "" {
		list, err := subs.ParseFile(*subsFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, s := range list {
			names = append(names, s.Name)
		}
	}
	t0 := time.Now()
	got, err := archive.Scan(fs.Arg(0), names, time.Now())
	took := time.Since(t0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("%-40s %5s %5s %6s %7s  %s\n", "SUBSCRIPTION", "KEPT", "FILES", "LAST30", "GB", "NEWEST FILE")
	for _, k := range keys {
		e := got[k]
		newest := "-"
		if !e.Newest.IsZero() {
			newest = e.Newest.Format("2006-01-02")
		}
		fmt.Printf("%-40.40s %5d %5d %6d %7.2f  %s\n", k, len(e.IDs), e.Files, e.Files30, float64(e.Bytes)/1e9, newest)
	}
	missing := 0
	for _, n := range names {
		if got[n] == nil {
			missing++
			fmt.Printf("%-40.40s  no archive under %s\n", n, fs.Arg(0))
		}
	}
	if len(names) > 0 {
		fmt.Printf("%d archives for %d subscriptions (%d without) in %.1fs\n", len(got), len(names), missing, took.Seconds())
	} else {
		fmt.Printf("%d archives in %.1fs\n", len(got), took.Seconds())
	}
	return 0
}

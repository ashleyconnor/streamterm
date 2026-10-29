// Command streamterm serves a read-only, streaming view of a bash script's
// output over Server-Sent Events.
//
// The pipeline, end to end:
//
//	bash (pipes, forced color)
//	  -> single merged fd
//	  -> batched byte chunks           internal/run/runner.go
//	  -> terminal-to-html Screen       internal/run/render.go
//	       -> finalized lines (immutable, numbered)
//	       -> live window   (mutable viewport)
//	  -> JSON envelopes                internal/run/hub.go
//	  -> SSE, resumable by sequence    internal/httpapi/server.go
//	  -> DOM                           web/app.js
package main

import (
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	streamterm "github.com/ashleyconnor/streamterm"
	"github.com/ashleyconnor/streamterm/internal/httpapi"
	"github.com/ashleyconnor/streamterm/internal/run"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	scriptDir := flag.String("scripts", "scripts", "directory of runnable .sh scripts")
	cols := flag.Int("cols", 160, "emulated screen width")
	rows := flag.Int("rows", 24, "emulated screen height; also the depth of the mutable region")
	flush := flag.Duration("flush", 60*time.Millisecond, "output batching interval")
	maxMB := flag.Int64("max-mb", 32, "per-run output cap in megabytes")
	flag.Parse()

	if _, err := os.Stat(*scriptDir); err != nil {
		log.Fatalf("script directory %q is not readable: %v", *scriptDir, err)
	}

	cfg := run.DefaultConfig()
	cfg.Cols, cfg.Rows = *cols, *rows
	cfg.FlushInterval = *flush
	cfg.MaxBytes = *maxMB * 1024 * 1024

	static, err := fs.Sub(streamterm.WebFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: httpapi.New(run.NewManager(*scriptDir, cfg), static),
		// No WriteTimeout: it would guillotine long-lived SSE connections.
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("streamterm listening on http://%s (scripts: %s, screen: %dx%d)",
		*addr, *scriptDir, *cols, *rows)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

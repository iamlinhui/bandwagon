// Command bandwagon runs a multi-instance BandwagonHost (搬瓦工) VPS dashboard.
//
// It polls the KiwiVM API for each configured VPS and serves a self-contained
// card-grid web UI. The whole thing compiles to a single binary.
//
// Usage:
//
//	bandwagon -config config.json
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"bandwagon/internal/dashboard"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config.json")
	flag.Parse()

	cfg, err := dashboard.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store := dashboard.NewStore(cfg.Instances, cfg.PollSeconds, cfg.Title)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Background polling runs for the lifetime of the process.
	go store.Run(ctx)

	srv := dashboard.NewServer(store, cfg.Password)
	if err := srv.ListenAndServe(ctx, cfg.Listen); err != nil {
		log.Fatalf("server: %v", err)
	}
	log.Print("shutdown complete")
}

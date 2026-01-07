package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/geCarlo/gobelisk/internal/application"
	"github.com/geCarlo/gobelisk/internal/config"
)

// version is set at build time by:
//
//	-ldflags "-X main.version=0.1.0"
var version = "dev"

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "/etc/gobelisk/gobelisk.yaml", "path to config file")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)
	logger.Printf("gobelisk starting (version=%s)", version)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		logger.Fatalf("config load failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C and systemd stop (SIGTERM).
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigc
		logger.Printf("signal received: %v; shutting down", s)
		cancel()
	}()

	if err := application.Run(ctx, cfg, logger); err != nil {
		logger.Fatalf("fatal: %v", err)
	}

	logger.Printf("gobelisk exited cleanly")
}

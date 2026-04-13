package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg, err := ParseConfig()
	if err != nil {
		os.Exit(1)
	}
	cfg.LogConfig()

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}

	// Start discovery
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	discovery, err := NewDiscovery(cfg.HAFDatabaseURL, cfg.AppTag, cfg.RefreshInterval)
	if err != nil {
		log.Fatalf("initializing discovery: %v", err)
	}

	// Auto-pin rules
	autopin, err := NewAutoPinManager(cfg.DataDir)
	if err != nil {
		log.Fatalf("initializing autopin: %v", err)
	}

	// Create IPFS backend
	var backend IPFSBackend
	switch cfg.IPFSMode {
	case "embedded":
		backend, err = NewEmbeddedNode(cfg.DataDir, cfg.GatewayPort)
		if err != nil {
			log.Fatalf("initializing embedded IPFS node: %v", err)
		}
		log.Printf("embedded IPFS node started, gateway on :%s", cfg.GatewayPort)
	case "pinata":
		backend = NewPinataBackend(cfg.PinataAPIKey, cfg.PinataSecretKey)
		log.Printf("using Pinata backend")
	}

	// Wire auto-pin: after each discovery refresh, pin CIDs matching rules
	discovery.SetOnRefresh(func(items []DiscoveredItem) {
		cids := autopin.MatchingCIDs(items)
		if len(cids) == 0 {
			return
		}
		pinned := 0
		for _, cid := range cids {
			already, _ := backend.IsPinned(ctx, cid)
			if already {
				continue
			}
			if err := backend.Pin(ctx, cid); err != nil {
				log.Printf("[autopin] failed to pin %s: %v", cid, err)
			} else {
				pinned++
			}
		}
		if pinned > 0 {
			log.Printf("[autopin] pinned %d new CIDs (%d matched rules)", pinned, len(cids))
		}
	})

	discovery.Start(ctx)

	// Start management server
	srv := NewServer(discovery, backend, autopin, cfg.RefreshInterval)
	httpServer := &http.Server{
		Addr:              "127.0.0.1:" + cfg.Port,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("management UI listening on http://localhost:%s", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	fmt.Printf("\nreceived %s, shutting down...\n", sig)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	if err := backend.Close(); err != nil {
		log.Printf("IPFS backend close error: %v", err)
	}

	discovery.Stop()
	log.Printf("shutdown complete")
}

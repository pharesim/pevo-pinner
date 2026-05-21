package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	gateway "github.com/ipfs/boxo/gateway"
)

// startGateway wires boxo's HTTP gateway handler against the offline
// blockservice (so HTTP traffic NEVER triggers a bitswap fetch — only
// already-imported content is served, matching the brainstorm decision on
// gateway scope). Path-style only; subdomain gateway out of scope.
func (n *EmbeddedNode) startGateway() error {
	backend, err := gateway.NewBlocksBackend(n.offlineSvc)
	if err != nil {
		return fmt.Errorf("gateway backend: %w", err)
	}

	// PublicGateways is only consulted by gateway.WithHostname; direct path
	// requests use the global config. We don't wrap with WithHostname (the
	// pinner is path-style only — subdomain gateway is out of scope), so a
	// minimal config suffices. DeserializedResponses=true lets the gateway
	// reassemble UnixFS files for normal browser fetches.
	cfg := gateway.Config{
		NoDNSLink:             true,
		DeserializedResponses: true,
	}
	boxoHandler := gateway.NewHandler(cfg, backend)

	mux := http.NewServeMux()
	mux.Handle("/ipfs/", n.gatewayGuard(boxoHandler))
	mux.Handle("/ipns/", n.gatewayGuard(boxoHandler))

	n.server = &http.Server{
		Addr:              ":" + n.gatewayPort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", n.server.Addr)
	if err != nil {
		return fmt.Errorf("gateway listen: %w", err)
	}
	go func() {
		log.Printf("[ipfs] gateway listening on :%s", n.gatewayPort)
		if err := n.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[ipfs] gateway error: %v", err)
		}
	}()
	return nil
}

// gatewayGuard rejects malformed CIDs at the route boundary before they reach
// boxo's handler. ValidateCID is the cheap defense-in-depth guard kept from
// the prior implementation: shape validation rejects garbage inputs without
// loading any DAG state.
func (n *EmbeddedNode) gatewayGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		var raw string
		switch {
		case strings.HasPrefix(path, "/ipfs/"):
			raw = path[len("/ipfs/"):]
		case strings.HasPrefix(path, "/ipns/"):
			// /ipns is best left to boxo's handler entirely — it knows how
			// to resolve names. Don't pre-validate.
			next.ServeHTTP(w, r)
			return
		default:
			next.ServeHTTP(w, r)
			return
		}
		// Strip any subpath; ValidateCID expects the bare CID component.
		if idx := strings.Index(raw, "/"); idx != -1 {
			raw = raw[:idx]
		}
		if raw == "" {
			http.Error(w, "missing CID", http.StatusBadRequest)
			return
		}
		if err := ValidateCID(raw); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

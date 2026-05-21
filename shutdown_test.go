package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestEmbeddedNodePinRejectedAfterDrain proves a closed pinner refuses fresh
// work outright. The drain gate is independent of the fetch mechanism, so it
// is testable without a real network: Pin's gate check fires before any
// bitswap session is opened.
func TestEmbeddedNodePinRejectedAfterDrain(t *testing.T) {
	node := newTestEmbeddedNode(t)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
	defer drainCancel()
	if err := node.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Any well-formed CID is fine here: the gate fires before the bitswap
	// session is opened, so the network is never touched.
	err := node.Pin(context.Background(), "QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7")
	if !errors.Is(err, ErrPinnerShuttingDown) {
		t.Errorf("Pin err = %v, want ErrPinnerShuttingDown", err)
	}
}

// TestEmbeddedNodePinRejectsMalformedCID proves ValidateCID is the first
// guard inside Pin: malformed input is rejected before the gate, before
// any bitswap session is opened, and before any disk write.
func TestEmbeddedNodePinRejectsMalformedCID(t *testing.T) {
	node := newTestEmbeddedNode(t)
	t.Cleanup(func() { _ = node.Close() })

	if err := node.Pin(context.Background(), "../../etc/passwd"); err == nil {
		t.Error("Pin err = nil, want validation error")
	}
}

// newTestEmbeddedNode constructs an EmbeddedNode with a temp data dir, a
// random gateway port, and no PEvO main anchor — minimal real libp2p host
// suitable for tests that don't exercise actual fetch behavior.
func newTestEmbeddedNode(t *testing.T) *EmbeddedNode {
	t.Helper()
	node, err := NewEmbeddedNode(IPFSNodeOptions{
		DataDir:     t.TempDir(),
		GatewayBind: "127.0.0.1:0",
		// Localhost-only TCP keeps the test fast and quiet — libp2p's
		// defaults bind to every interface on the box (slow on machines
		// with Docker bridges, noisy in logs). The drain gate is the
		// behavior under test; the host just needs to be alive.
		Libp2pListen:   []string{"/ip4/127.0.0.1/tcp/0"},
		BitswapTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	return node
}

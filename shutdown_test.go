package main

import (
	"context"
	"errors"
	"sync"
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

	if err := node.Pin(context.Background(), "../../etc/passwd"); err == nil {
		t.Error("Pin err = nil, want validation error")
	}
}

// TestEmbeddedNodeDrainForceCancelsInFlightPin proves Drain's deadline
// actually unwinds an in-flight Pin: when the drain ctx expires, every
// registered Pin's pinCtx is cancelled, the in-flight goroutine returns,
// and Drain reports DeadlineExceeded rather than blocking. Without the
// force-cancel branch in Drain the Pin (which has no provider in this
// isolated test) would only return when its 100ms BitswapTimeout fires.
func TestEmbeddedNodeDrainForceCancelsInFlightPin(t *testing.T) {
	node := newTestEmbeddedNode(t)
	// Generous bitswap timeout so the Pin would block on its own much
	// longer than the Drain deadline — proving cancellation is what
	// unwinds it.
	node.bitswapTimeout = 10 * time.Second
	// Empty fallback chain so CAR-fetch fails immediately and the Pin
	// goroutine is parked entirely in bitswap.
	node.fallbackGateways = nil

	const cidStr = "QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7"
	var wg sync.WaitGroup
	wg.Add(1)
	var pinErr error
	go func() {
		defer wg.Done()
		pinErr = node.Pin(context.Background(), cidStr)
	}()

	// Wait for the Pin goroutine to register itself in node.cancels.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		node.drainMu.Lock()
		registered := len(node.cancels) > 0
		node.drainMu.Unlock()
		if registered {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer drainCancel()
	drainStart := time.Now()
	err := node.Drain(drainCtx)
	drainElapsed := time.Since(drainStart)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Drain err = %v, want DeadlineExceeded", err)
	}
	if drainElapsed > 5*time.Second {
		t.Errorf("Drain elapsed = %s, want <5s (bitswap should be force-cancelled, not waited out)", drainElapsed)
	}

	wg.Wait()
	if pinErr == nil {
		t.Error("Pin returned nil; expected cancellation error")
	}
}

// newTestEmbeddedNode constructs an EmbeddedNode with a temp data dir, a
// random gateway port, and no PEvO main anchor — minimal real libp2p host
// suitable for tests that don't exercise actual fetch behavior. Close is
// registered as a t.Cleanup so individual test files don't have to wire it.
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
	t.Cleanup(func() { _ = node.Close() })
	return node
}

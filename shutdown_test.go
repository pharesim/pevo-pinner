package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEmbeddedNodeDrainWaitsForInFlightPin proves Drain blocks until an
// in-flight Pin call returns. Without this guarantee, the autopin callback
// can still be mid-`io.Copy` when main.go reaches backend.Close, leaving a
// partial block file on disk and a "writing block" log against a closed
// backend.
func TestEmbeddedNodeDrainWaitsForInFlightPin(t *testing.T) {
	content := []byte("authentic in-flight payload")
	expectedCID := cidForContent(t, content)

	// The handler blocks until the test releases it. This puts the
	// in-flight Pin in a deterministic mid-stream state.
	proceed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-proceed
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	withGatewaysSet(t, []string{srv.URL})

	tmp := t.TempDir()
	node, err := NewEmbeddedNode(tmp, "0", 1<<20)
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	pinDone := make(chan error, 1)
	go func() {
		pinDone <- node.Pin(context.Background(), expectedCID)
	}()

	// Let Pin enter the gateway request and block on `proceed`.
	time.Sleep(50 * time.Millisecond)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()

	drainDone := make(chan error, 1)
	go func() {
		drainDone <- node.Drain(drainCtx)
	}()

	// Drain must still be blocked: in-flight Pin hasn't completed.
	select {
	case err := <-drainDone:
		t.Fatalf("Drain returned %v before Pin completed", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Releasing the handler completes Pin; Drain should then return.
	close(proceed)

	if err := <-pinDone; err != nil {
		t.Fatalf("Pin returned %v, want nil", err)
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("Drain returned %v, want nil", err)
	}
}

// TestEmbeddedNodeDrainTimesOutOnHungPin proves Drain enforces the caller's
// deadline rather than waiting forever on a stuck gateway fetch. After the
// deadline, the process is expected to exit; the leaked Pin goroutine is
// reaped by the OS.
func TestEmbeddedNodeDrainTimesOutOnHungPin(t *testing.T) {
	expectedCID := cidForContent(t, []byte("never-arrives payload"))

	// Handler hangs until closeForever is signalled by cleanup.
	stopHandler := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stopHandler
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stopHandler) })
	withGatewaysSet(t, []string{srv.URL})

	tmp := t.TempDir()
	node, err := NewEmbeddedNode(tmp, "0", 1<<20)
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	pinCtx, pinCancel := context.WithCancel(context.Background())
	t.Cleanup(pinCancel)

	go func() {
		_ = node.Pin(pinCtx, expectedCID)
	}()

	time.Sleep(50 * time.Millisecond)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer drainCancel()

	start := time.Now()
	err = node.Drain(drainCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Drain err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Errorf("Drain took %v, want under 1s", elapsed)
	}
}

// TestEmbeddedNodePinRejectedAfterDrain proves a closed pinner refuses fresh
// work outright. Without this, a stray autopin callback firing after Drain
// completes could still reach backend.Pin and write against a gateway server
// that's about to be shut down.
func TestEmbeddedNodePinRejectedAfterDrain(t *testing.T) {
	tmp := t.TempDir()
	node, err := NewEmbeddedNode(tmp, "0", 1<<20)
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
	defer drainCancel()
	if err := node.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	err = node.Pin(context.Background(), cidForContent(t, []byte("post-drain payload")))
	if !errors.Is(err, ErrPinnerShuttingDown) {
		t.Errorf("Pin err = %v, want ErrPinnerShuttingDown", err)
	}
}

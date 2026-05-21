package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestEmbeddedNodeDrainCancelsPinAndCleansPartialFiles proves Drain's hard
// deadline force-cancels in-flight Pin ctxs so io.Copy unwinds, the tmp
// file is removed by the per-Pin cleanup, and nothing remains at either the
// canonical block path or the .tmp path. Without ctx propagation through
// Pin, io.Copy would stay blocked on the underlying gateway read until the
// http.Client timeout (2 min) or process exit; the deferred cleanup would
// never run; the partial file would persist and be promoted to fully-pinned
// on the next startup via the os.Stat short-circuit.
func TestEmbeddedNodeDrainCancelsPinAndCleansPartialFiles(t *testing.T) {
	expectedCID := cidForContent(t, []byte("payload the gateway will never finish sending"))

	// Gateway sends a 200 + a partial body chunk, then hangs. The partial
	// chunk lands in the .tmp file so the post-drain cleanup is verified
	// to actually remove a file with content, not just an empty placeholder.
	stopHandler := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial-bytes-on-the-wire "))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
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

	pinDone := make(chan struct{})
	go func() {
		_ = node.Pin(context.Background(), expectedCID)
		close(pinDone)
	}()

	// Wait for Pin to reach the io.Copy and write the partial chunk into
	// the .tmp file before triggering drain.
	tmpFile := filepath.Join(tmp, "blocks", expectedCID+".tmp")
	if err := waitForFile(tmpFile, time.Second); err != nil {
		t.Fatalf("tmp file never appeared: %v", err)
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer drainCancel()
	if err := node.Drain(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain err = %v, want DeadlineExceeded", err)
	}

	// Wait for Pin's deferred cleanup to fully run. Drain's post-cancel
	// grace usually returns only after this completes, but guard against
	// timing drift so the assertions below don't race.
	select {
	case <-pinDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Pin did not unwind within 2s after drain force-cancel")
	}

	blockFile := filepath.Join(tmp, "blocks", expectedCID)
	if _, err := os.Stat(blockFile); !os.IsNotExist(err) {
		t.Errorf("canonical block file at %s after drain timeout (stat err: %v)", blockFile, err)
	}
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Errorf("tmp file at %s after drain timeout (stat err: %v)", tmpFile, err)
	}
}

// waitForFile polls until the named file appears or the deadline elapses.
// Used to synchronize tests on the moment the production code reaches a
// disk write, avoiding a hardcoded sleep that races on slow CI.
func waitForFile(path string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("file did not appear before deadline")
		}
		time.Sleep(10 * time.Millisecond)
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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A well-formed CIDv0 — content doesn't matter; we never let the body land.
const sizeCapTestCID = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"

// withGatewaysSet swaps the package-level publicGateways for the test and
// restores it on cleanup. The list is small and unexported, so direct mutation
// is the simplest injection point.
func withGatewaysSet(t *testing.T, gws []string) {
	t.Helper()
	saved := publicGateways
	publicGateways = gws
	t.Cleanup(func() { publicGateways = saved })
}

func TestEmbeddedNodePinRejectsOversizedGatewayResponse(t *testing.T) {
	const cap = int64(64)

	// Server streams more than `cap` bytes — exactly one byte over the +1
	// probe is enough to drain the LimitedReader's budget.
	body := strings.Repeat("A", int(cap)+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	withGatewaysSet(t, []string{srv.URL})

	tmp := t.TempDir()
	node, err := NewEmbeddedNode(tmp, "0", cap)
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	err = node.Pin(context.Background(), sizeCapTestCID)
	if err == nil {
		t.Fatalf("Pin returned nil, want size-cap error")
	}
	if !strings.Contains(err.Error(), "size cap") {
		t.Errorf("Pin error = %v, want it to mention size cap", err)
	}

	// The partial block file must be removed.
	blockFile := filepath.Join(tmp, "blocks", sizeCapTestCID)
	if _, statErr := os.Stat(blockFile); !os.IsNotExist(statErr) {
		t.Errorf("block file at %s still exists after rejected pin (stat err: %v)", blockFile, statErr)
	}

	// No pin should have been recorded.
	if pinned, _ := node.IsPinned(context.Background(), sizeCapTestCID); pinned {
		t.Errorf("IsPinned = true after rejected pin")
	}
}

func TestEmbeddedNodePinAcceptsUnderCapResponse(t *testing.T) {
	const cap = int64(1024)

	body := strings.Repeat("B", 100) // well under cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	withGatewaysSet(t, []string{srv.URL})

	tmp := t.TempDir()
	node, err := NewEmbeddedNode(tmp, "0", cap)
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	if err := node.Pin(context.Background(), sizeCapTestCID); err != nil {
		t.Fatalf("Pin returned %v, want nil", err)
	}

	written, err := os.ReadFile(filepath.Join(tmp, "blocks", sizeCapTestCID))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(written) != body {
		t.Errorf("written body length = %d, want %d", len(written), len(body))
	}
}

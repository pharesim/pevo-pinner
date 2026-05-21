package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cid "github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// cidForContent computes the canonical CIDv0 (sha2-256 multihash) for the
// given bytes. Used by tests to construct content/CID pairs that the pinner's
// hash-verifier accepts (or to derive a CID whose expected digest does NOT
// match a tampered body the test will serve).
func cidForContent(t *testing.T, content []byte) string {
	t.Helper()
	digest, err := mh.Sum(content, mh.SHA2_256, -1)
	if err != nil {
		t.Fatalf("multihash.Sum: %v", err)
	}
	return cid.NewCidV0(digest).String()
}

func TestEmbeddedNodePinRejectsHashMismatch(t *testing.T) {
	expectedCID := cidForContent(t, []byte("authentic payload"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tampered payload"))
	}))
	t.Cleanup(srv.Close)

	withGatewaysSet(t, []string{srv.URL})

	tmp := t.TempDir()
	node, err := NewEmbeddedNode(tmp, "0", 1<<20)
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	err = node.Pin(context.Background(), expectedCID)
	if err == nil {
		t.Fatalf("Pin returned nil, want hash-mismatch error")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("Pin error = %v, want hash-mismatch message", err)
	}

	blockFile := filepath.Join(tmp, "blocks", expectedCID)
	if _, statErr := os.Stat(blockFile); !os.IsNotExist(statErr) {
		t.Errorf("block file at %s still exists after rejected pin (stat err: %v)", blockFile, statErr)
	}

	if pinned, _ := node.IsPinned(context.Background(), expectedCID); pinned {
		t.Errorf("IsPinned = true after rejected pin")
	}
}

func TestEmbeddedNodePinAcceptsMatchingHash(t *testing.T) {
	content := []byte("authentic payload for matching CID test")
	expectedCID := cidForContent(t, content)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	if err := node.Pin(context.Background(), expectedCID); err != nil {
		t.Fatalf("Pin returned %v, want nil", err)
	}
	if pinned, _ := node.IsPinned(context.Background(), expectedCID); !pinned {
		t.Errorf("IsPinned = false after successful pin")
	}

	written, err := os.ReadFile(filepath.Join(tmp, "blocks", expectedCID))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(written) != string(content) {
		t.Errorf("written body differs from served content")
	}
}

// TestEmbeddedNodePinAdvancesPastBadGateway proves the multi-gateway loop
// surfaces — if gateway A returns tampered bytes, gateway B's authentic bytes
// must still land. This guards the contract that one compromised gateway
// cannot deny service for content the rest of the network still has.
func TestEmbeddedNodePinAdvancesPastBadGateway(t *testing.T) {
	content := []byte("authentic payload across multiple gateways")
	expectedCID := cidForContent(t, content)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("garbage from a malicious gateway"))
	}))
	t.Cleanup(bad.Close)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(good.Close)

	withGatewaysSet(t, []string{bad.URL, good.URL})

	tmp := t.TempDir()
	node, err := NewEmbeddedNode(tmp, "0", 1<<20)
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	if err := node.Pin(context.Background(), expectedCID); err != nil {
		t.Fatalf("Pin returned %v, want nil after fallback to good gateway", err)
	}
	if pinned, _ := node.IsPinned(context.Background(), expectedCID); !pinned {
		t.Errorf("IsPinned = false after pin via fallback gateway")
	}
}

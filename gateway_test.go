package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGatewayRejectsMalformedCID exercises the route-boundary shape check:
// any /ipfs/ path whose leading component does not parse as a CID is
// rejected with 400 before boxo's handler is reached.
func TestGatewayRejectsMalformedCID(t *testing.T) {
	node := newTestEmbeddedNode(t)
	srv := httptest.NewServer(node.gatewayGuard(passthroughHandler()))
	t.Cleanup(srv.Close)

	for _, path := range []string{"/ipfs/../../etc/passwd", "/ipfs/not-a-cid", "/ipfs/"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, resp.StatusCode)
		}
	}
}

// TestGatewayDeniesUnpinnedCID is the core pinned-only contract: a request
// for a CID that is not in n.pins returns 404 even when the CID itself is
// shape-valid. Without this guard the offline blockservice would serve any
// block in the blockstore — bitswap-session-bleed leaks, partial-import
// orphans — that the operator never consented to publish.
func TestGatewayDeniesUnpinnedCID(t *testing.T) {
	node := newTestEmbeddedNode(t)
	srv := httptest.NewServer(node.gatewayGuard(passthroughHandler()))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ipfs/QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestGatewayAllowsPinnedCID proves the guard is not over-restrictive: once
// the operator pins a CID, the gateway lets the request through to boxo's
// handler. We assert reach-through with a passthrough handler — the actual
// boxo serve path is exercised by the integration test.
func TestGatewayAllowsPinnedCID(t *testing.T) {
	node := newTestEmbeddedNode(t)
	const cidStr = "QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7"
	node.markPinned(cidStr)

	reached := false
	srv := httptest.NewServer(node.gatewayGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ipfs/" + cidStr)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if !reached {
		t.Error("guard did not delegate to next handler for pinned CID")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestGatewayAllowsSubpathOfPinnedRoot proves /ipfs/<pinned-root>/sub is
// permitted: the guard only validates the leading CID, leaving boxo to
// resolve sub-paths inside the pinned DAG.
func TestGatewayAllowsSubpathOfPinnedRoot(t *testing.T) {
	node := newTestEmbeddedNode(t)
	const cidStr = "QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7"
	node.markPinned(cidStr)

	reached := false
	srv := httptest.NewServer(node.gatewayGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ipfs/" + cidStr + "/some/sub/path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if !reached {
		t.Error("guard did not delegate for sub-path of pinned root")
	}
}

// TestGatewayPassesIPNSThrough proves /ipns/ requests are left to boxo
// untouched (the guard pre-validates only /ipfs/ paths).
func TestGatewayPassesIPNSThrough(t *testing.T) {
	node := newTestEmbeddedNode(t)
	reached := false
	srv := httptest.NewServer(node.gatewayGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ipns/whatever-name")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if !reached {
		t.Error("guard short-circuited /ipns/ instead of passing through")
	}
}

// passthroughHandler returns an http.Handler that writes 200 with a fixed
// marker body. Tests use it to assert the guard delegated to the next
// handler rather than blocking the request.
func passthroughHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, bytes.NewReader([]byte("ok")))
	})
}

// helpers to silence unused-import warnings if a test file reorganises.
var (
	_ = context.Background
	_ = strings.HasPrefix
)

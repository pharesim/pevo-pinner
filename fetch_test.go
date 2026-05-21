package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cid "github.com/ipfs/go-cid"
)

// TestFetchViaCARRejectsHostileGateway proves the CAR-fetch fallback rejects
// a gateway that returns garbage instead of a real CAR stream. Trustless
// verification relies on the CAR parser's per-block hash check; a body
// without CAR framing must error at the parser, not silently land bytes.
func TestFetchViaCARRejectsHostileGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.ipld.car" {
			t.Errorf("Accept header = %q, want application/vnd.ipld.car", got)
		}
		_, _ = w.Write([]byte("not a CAR stream"))
	}))
	t.Cleanup(srv.Close)

	node := newTestEmbeddedNode(t)
	t.Cleanup(func() { _ = node.Close() })
	// Replace the fallback chain with only the hostile server so the test
	// doesn't reach out to the public-gateway defaults.
	node.fallbackGateways = []string{srv.URL}

	c, err := cid.Decode("QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7")
	if err != nil {
		t.Fatalf("cid.Decode: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = node.fetchViaCAR(ctx, c)
	if err == nil {
		t.Fatal("fetchViaCAR err = nil, want CAR parse error")
	}
	if !strings.Contains(err.Error(), "all CAR-fetch gateways failed") {
		t.Errorf("error = %v, want 'all CAR-fetch gateways failed' wrapper", err)
	}
}

// TestFetchViaCARFailsClosedWithNoGateways proves the fallback chain returns
// a clear error when no gateways are configured rather than appearing to
// succeed. Defense against operator misconfiguration that would silently
// disable the trustless fallback.
func TestFetchViaCARFailsClosedWithNoGateways(t *testing.T) {
	node := newTestEmbeddedNode(t)
	t.Cleanup(func() { _ = node.Close() })
	node.fallbackGateways = nil

	c, err := cid.Decode("QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7")
	if err != nil {
		t.Fatalf("cid.Decode: %v", err)
	}

	err = node.fetchViaCAR(context.Background(), c)
	if err == nil {
		t.Fatal("fetchViaCAR err = nil, want 'no fallback gateways' error")
	}
}

// TestFetchViaCARTriesGatewaysInOrder proves the chain is consulted in the
// declared order: a 500 from the first gateway must fall through to the
// second, not short-circuit. The brainstorm-stated ordering (PEvO main →
// mesh → public) only holds if the chain actually walks past failures.
func TestFetchViaCARTriesGatewaysInOrder(t *testing.T) {
	var order []string
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "first")
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "second")
		_, _ = w.Write([]byte("also not a CAR"))
	}))
	t.Cleanup(second.Close)

	node := newTestEmbeddedNode(t)
	t.Cleanup(func() { _ = node.Close() })
	node.fallbackGateways = []string{first.URL, second.URL}

	c, err := cid.Decode("QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7")
	if err != nil {
		t.Fatalf("cid.Decode: %v", err)
	}
	_ = node.fetchViaCAR(context.Background(), c)

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("gateway visit order = %v, want [first second]", order)
	}
}


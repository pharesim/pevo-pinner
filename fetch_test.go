package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cid "github.com/ipfs/go-cid"
	peer "github.com/libp2p/go-libp2p/core/peer"
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
// second, not short-circuit. The declared ordering (PEvO main → operator
// extras → mesh → public defaults — see assembleFallbackChain) only holds
// if the chain actually walks past failures.
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
	err = node.fetchViaCAR(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "all CAR-fetch gateways failed") {
		t.Errorf("err = %v, want 'all CAR-fetch gateways failed' wrapper", err)
	}

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("gateway visit order = %v, want [first second]", order)
	}
}

// TestFetchViaCARRejectsHashMismatch is the central trustless guarantee:
// even with valid CAR framing, a block whose payload bytes do not hash to
// the declared CID is rejected by carv2's BlockReader at the parser. A
// hostile gateway returning swapped block bytes cannot land content under
// a CID it does not own.
func TestFetchViaCARRejectsHashMismatch(t *testing.T) {
	realBlock := newRawBlock(t, []byte("legitimate-block-bytes"))
	garbage := []byte("attacker-supplied-replacement-bytes-of-different-length")
	carBytes := buildCARv1(t, realBlock.Cid(), []carEntry{
		{cid: realBlock.Cid(), data: garbage},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		_, _ = w.Write(carBytes)
	}))
	t.Cleanup(srv.Close)

	node := newTestEmbeddedNode(t)
	node.fallbackGateways = []string{srv.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := node.fetchViaCAR(ctx, realBlock.Cid())
	if err == nil {
		t.Fatal("fetchViaCAR err = nil, want hash-mismatch rejection")
	}
	if !strings.Contains(err.Error(), "all CAR-fetch gateways failed") {
		t.Errorf("error = %v, want chain-failure wrapper", err)
	}
}

// TestFetchCARFromGatewayEnforcesMaxPinBytes proves the disk-fill guard
// actually trips: a CAR whose body exceeds the cap is rejected mid-stream
// with a clear error, not silently truncated and reported as success.
func TestFetchCARFromGatewayEnforcesMaxPinBytes(t *testing.T) {
	// Construct a real CAR with two blocks. We'll cap at a value smaller than
	// the second block so the import aborts partway through.
	first := newRawBlock(t, []byte("first-block-payload-bytes"))
	second := newRawBlock(t, bytes.Repeat([]byte("x"), 256))
	carBytes := buildCARv1(t, first.Cid(), []carEntry{
		{cid: first.Cid(), data: first.RawData()},
		{cid: second.Cid(), data: second.RawData()},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		_, _ = w.Write(carBytes)
	}))
	t.Cleanup(srv.Close)

	node := newTestEmbeddedNode(t)
	node.maxPinBytes = 64 // smaller than the second block payload

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := node.fetchCARFromGateway(ctx, srv.URL, first.Cid())
	if err == nil {
		t.Fatal("fetchCARFromGateway err = nil, want MAX_PIN_BYTES rejection")
	}
	if !strings.Contains(err.Error(), "MAX_PIN_BYTES") {
		t.Errorf("error = %v, want MAX_PIN_BYTES message", err)
	}
}

// TestFetchCARFromGatewayRejectsEmptyCAR exercises the imported==0 guard so
// a gateway returning a valid CAR header with no blocks at all is treated
// as a failure (the chain falls through), not silently as success.
func TestFetchCARFromGatewayRejectsEmptyCAR(t *testing.T) {
	// Use any well-formed CID as the root; we never import anything so the
	// CID's contents don't matter.
	root := newRawBlock(t, []byte("root-marker"))
	carBytes := buildCARv1(t, root.Cid(), nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		_, _ = w.Write(carBytes)
	}))
	t.Cleanup(srv.Close)

	node := newTestEmbeddedNode(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := node.fetchCARFromGateway(ctx, srv.URL, root.Cid())
	if err == nil {
		t.Fatal("fetchCARFromGateway err = nil, want 'no blocks' rejection")
	}
	if !strings.Contains(err.Error(), "no blocks") {
		t.Errorf("error = %v, want 'no blocks' message", err)
	}
}

// TestFetchViaCARPerGatewayTimeout proves a stalled gateway in position 1
// does not hold the chain for the full http.Client.Timeout (5 min). Once the
// per-gateway 90s budget expires the chain falls through; here we use a
// short-circuited timeout (rewriting perGatewayFetchTimeout is brittle, so
// instead we use a fast-failing second gateway and a long-sleep first
// gateway and bound the wall-clock test budget at 2s).
func TestFetchViaCARPerGatewayTimeout(t *testing.T) {
	// Test the contract from the caller's side: when ctx expires inside the
	// first gateway, fetchViaCAR returns; it does not block on a successful
	// follow-up. This proves the fetchCARFromGateway context honoring is
	// wired through correctly.
	stalledHits := int32(0)
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stalledHits, 1)
		<-r.Context().Done()
	}))
	t.Cleanup(stalled.Close)

	node := newTestEmbeddedNode(t)
	node.fallbackGateways = []string{stalled.URL}

	c, err := cid.Decode("QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7")
	if err != nil {
		t.Fatalf("cid.Decode: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = node.fetchViaCAR(ctx, c)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("fetchViaCAR err = nil, want failure")
	}
	if elapsed > 2*time.Second {
		t.Errorf("fetchViaCAR elapsed = %s, want <2s (ctx-cancel should propagate to gateway request)", elapsed)
	}
	if atomic.LoadInt32(&stalledHits) == 0 {
		t.Error("stalled gateway was never reached")
	}
}

// TestAssembleFallbackChainSplicesMeshPeers proves mesh entries land between
// the operator-configured head and the public-default tail — the contract
// assembleFallbackChain promises.
func TestAssembleFallbackChainSplicesMeshPeers(t *testing.T) {
	node := newTestEmbeddedNode(t)
	node.fallbackGateways = []string{
		"https://pevo-main.example",
		"https://op-extra.example",
		"https://ipfs.io",
		"https://dweb.link",
	}
	node.staticHeadLen = 2 // pevo-main + op-extra
	node.mesh = &meshManager{
		peers: map[peer.ID]*meshPeer{
			"mesh-peer-1": {gatewayURL: "https://mesh1.example"},
		},
	}

	got := node.assembleFallbackChain()
	want := []string{
		"https://pevo-main.example",
		"https://op-extra.example",
		"https://mesh1.example",
		"https://ipfs.io",
		"https://dweb.link",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

// TestAssembleFallbackChainDeduplicatesURLVariants proves the normalized
// dedupe key collapses trailing-slash / case / default-port variants so a
// mesh peer cannot bias the chain by republishing an existing URL with
// cosmetic differences.
func TestAssembleFallbackChainDeduplicatesURLVariants(t *testing.T) {
	node := newTestEmbeddedNode(t)
	node.fallbackGateways = []string{"https://ipfs.io"}
	node.staticHeadLen = 0
	node.mesh = &meshManager{
		peers: map[peer.ID]*meshPeer{
			"a": {gatewayURL: "https://IPFS.IO/"},
			"b": {gatewayURL: "https://ipfs.io"},
		},
	}

	got := node.assembleFallbackChain()
	if len(got) != 1 {
		t.Errorf("chain length = %d, want 1 (variants deduped); got %v", len(got), got)
	}
}



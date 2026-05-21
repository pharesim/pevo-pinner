package main

import (
	"context"
	"sort"
	"testing"
	"time"
)

// TestEmbeddedNodePinStatePersistsAcrossRestart proves pins.json survives
// Close → re-open round-trips. The boxo pinset is the eventual mechanism;
// this test guards the current pins.json-backed implementation. Without
// this coverage a regression that loses pinned state on restart would only
// surface in production.
func TestEmbeddedNodePinStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	opts := IPFSNodeOptions{
		DataDir:        dir,
		GatewayBind:    "127.0.0.1:0",
		Libp2pListen:   []string{"/ip4/127.0.0.1/tcp/0"},
		BitswapTimeout: 100 * time.Millisecond,
	}

	first, err := NewEmbeddedNode(opts)
	if err != nil {
		t.Fatalf("NewEmbeddedNode #1: %v", err)
	}
	const cidA = "QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7"
	const cidB = "QmTzQ1Nj5UfXq7mZpwULm8mF44sBhgmtoVRDmihrtoqCEv"
	first.markPinned(cidA)
	first.markPinned(cidB)
	if err := first.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}

	second, err := NewEmbeddedNode(opts)
	if err != nil {
		t.Fatalf("NewEmbeddedNode #2: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	for _, c := range []string{cidA, cidB} {
		got, err := second.IsPinned(context.Background(), c)
		if err != nil {
			t.Fatalf("IsPinned(%s): %v", c, err)
		}
		if !got {
			t.Errorf("IsPinned(%s) = false after restart, want true", c)
		}
	}
}

// TestEmbeddedNodePinnedCIDsListsAllPinned proves PinnedCIDs reflects the
// full in-memory pin set, regardless of insertion order.
func TestEmbeddedNodePinnedCIDsListsAllPinned(t *testing.T) {
	node := newTestEmbeddedNode(t)
	want := []string{
		"QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7",
		"QmTzQ1Nj5UfXq7mZpwULm8mF44sBhgmtoVRDmihrtoqCEv",
	}
	for _, c := range want {
		node.markPinned(c)
	}

	got, err := node.PinnedCIDs(context.Background())
	if err != nil {
		t.Fatalf("PinnedCIDs: %v", err)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("PinnedCIDs len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("PinnedCIDs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestEmbeddedNodeUnpinRemovesFromList proves Unpin actually clears the
// entry — independently observable through both IsPinned and PinnedCIDs.
func TestEmbeddedNodeUnpinRemovesFromList(t *testing.T) {
	node := newTestEmbeddedNode(t)
	const cidStr = "QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7"
	node.markPinned(cidStr)

	ctx := context.Background()
	if err := node.Unpin(ctx, cidStr); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	pinned, err := node.IsPinned(ctx, cidStr)
	if err != nil {
		t.Fatalf("IsPinned: %v", err)
	}
	if pinned {
		t.Error("IsPinned after Unpin = true, want false")
	}
	cids, err := node.PinnedCIDs(ctx)
	if err != nil {
		t.Fatalf("PinnedCIDs: %v", err)
	}
	for _, c := range cids {
		if c == cidStr {
			t.Errorf("PinnedCIDs still contains %s after Unpin", c)
		}
	}
}

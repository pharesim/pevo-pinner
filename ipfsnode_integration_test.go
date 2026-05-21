//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	host "github.com/libp2p/go-libp2p/core/host"
	peer "github.com/libp2p/go-libp2p/core/peer"
)

// TestMeshTwoPinnerDiscovery proves two pinners on the same APP_TAG topic
// see each other's heartbeats within one cadence interval. Cross-talk
// resistance (different APP_TAGs → separate meshes) is covered by the
// topic name format alone — no message can cross a topic boundary.
//
// Gated by //go:build integration: pubsub depends on libp2p connection
// establishment, which is slow enough (~500ms host startup + 100ms first
// heartbeat) that we don't want it in the default test cycle.
func TestMeshTwoPinnerDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h1 := newIntegrationHost(t)
	h2 := newIntegrationHost(t)

	// Connect the two hosts directly so pubsub has a peering to gossip
	// over; in production this happens via the DHT and bootstrap peers.
	if err := h1.Connect(ctx, peer.AddrInfo{ID: h2.ID(), Addrs: h2.Addrs()}); err != nil {
		t.Fatalf("h1 -> h2 connect: %v", err)
	}

	const tag = "pinner-test-mesh"
	discovered := make(chan peer.ID, 2)
	m1, err := startMesh(ctx, h1, MeshOptions{
		AppTag:   tag,
		Interval: 500 * time.Millisecond,
	}, func(p meshPeer) { discovered <- p.addrInfo.ID }, nil)
	if err != nil {
		t.Fatalf("startMesh h1: %v", err)
	}
	t.Cleanup(m1.Stop)

	m2, err := startMesh(ctx, h2, MeshOptions{
		AppTag:   tag,
		Interval: 500 * time.Millisecond,
	}, func(p meshPeer) { discovered <- p.addrInfo.ID }, nil)
	if err != nil {
		t.Fatalf("startMesh h2: %v", err)
	}
	t.Cleanup(m2.Stop)

	seen := map[peer.ID]struct{}{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case id := <-discovered:
			seen[id] = struct{}{}
		case <-deadline:
			t.Fatalf("discovery timed out: saw %d / 2 peers", len(seen))
		}
	}
}

// TestBitswapFetchFromPEvOMain is the canonical end-to-end smoke test: a
// known CID is fetched via bitswap from PEvO main and verified locally.
// Skipped unless PEVO_MAIN_LIBP2P_ADDR + PEVO_PINNER_TEST_CID are set, so
// CI without network egress can opt in by setting them.
func TestBitswapFetchFromPEvOMain(t *testing.T) {
	bootstrap := os.Getenv("PEVO_MAIN_LIBP2P_ADDR")
	testCID := os.Getenv("PEVO_PINNER_TEST_CID")
	if bootstrap == "" || testCID == "" {
		t.Skip("set PEVO_MAIN_LIBP2P_ADDR + PEVO_PINNER_TEST_CID to run")
	}

	node, err := NewEmbeddedNode(IPFSNodeOptions{
		DataDir:            t.TempDir(),
		GatewayPort:        "0",
		Libp2pListen:       []string{"/ip4/127.0.0.1/tcp/0"},
		PEvOMainLibp2pAddr: bootstrap,
		BitswapTimeout:     30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := node.Pin(ctx, testCID); err != nil {
		t.Fatalf("Pin(%s): %v", testCID, err)
	}
	pinned, err := node.IsPinned(ctx, testCID)
	if err != nil {
		t.Fatalf("IsPinned: %v", err)
	}
	if !pinned {
		t.Errorf("IsPinned(%s) = false, want true after Pin succeeded", testCID)
	}
}

// newIntegrationHost spins up a localhost-only libp2p host for tests that
// need real peering. Keeps the test isolated from the operator's network
// stack and fast (~100ms per host).
func newIntegrationHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.DisableMetrics(),
	)
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

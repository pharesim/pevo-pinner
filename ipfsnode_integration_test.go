//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	host "github.com/libp2p/go-libp2p/core/host"
	peer "github.com/libp2p/go-libp2p/core/peer"
)

// TestMeshTwoPinnerDiscovery proves two pinners on the same APP_TAG topic
// see each other's heartbeats within one cadence interval. The
// pollKnownPeers helper observes the public KnownPeers() snapshot
// directly so the test does not depend on internal callbacks.
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
	m1, err := startMesh(ctx, h1, MeshOptions{AppTag: tag, Interval: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("startMesh h1: %v", err)
	}
	t.Cleanup(m1.Stop)

	m2, err := startMesh(ctx, h2, MeshOptions{AppTag: tag, Interval: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("startMesh h2: %v", err)
	}
	t.Cleanup(m2.Stop)

	if !waitForPeer(m1, h2.ID(), 5*time.Second) {
		t.Errorf("m1 did not discover h2 within 5s")
	}
	if !waitForPeer(m2, h1.ID(), 5*time.Second) {
		t.Errorf("m2 did not discover h1 within 5s")
	}
}

// TestMeshCrossAppTagIsolation proves the topic-name namespace (per APP_TAG)
// actually isolates meshes: two pairs of pinners on different tags never
// see each other's heartbeats. Documented in TestMeshTwoPinnerDiscovery as
// "covered by the topic name format alone" — this test executes the claim.
func TestMeshCrossAppTagIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hA1 := newIntegrationHost(t)
	hA2 := newIntegrationHost(t)
	hB1 := newIntegrationHost(t)

	// Fully connect all three hosts so pubsub *could* gossip across them —
	// only the topic-name boundary should prevent it.
	for _, hSrc := range []host.Host{hA1, hA2, hB1} {
		for _, hDst := range []host.Host{hA1, hA2, hB1} {
			if hSrc.ID() == hDst.ID() {
				continue
			}
			_ = hSrc.Connect(ctx, peer.AddrInfo{ID: hDst.ID(), Addrs: hDst.Addrs()})
		}
	}

	mA1, err := startMesh(ctx, hA1, MeshOptions{AppTag: "tag-a", Interval: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("startMesh A1: %v", err)
	}
	t.Cleanup(mA1.Stop)
	mA2, err := startMesh(ctx, hA2, MeshOptions{AppTag: "tag-a", Interval: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("startMesh A2: %v", err)
	}
	t.Cleanup(mA2.Stop)
	mB1, err := startMesh(ctx, hB1, MeshOptions{AppTag: "tag-b", Interval: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("startMesh B1: %v", err)
	}
	t.Cleanup(mB1.Stop)

	// Same-tag pairs see each other.
	if !waitForPeer(mA1, hA2.ID(), 5*time.Second) {
		t.Errorf("mA1 did not discover hA2 on the same tag")
	}
	// Cross-tag should remain invisible even after the same-tag discovery
	// window has comfortably passed — wait twice the interval and assert.
	time.Sleep(2 * time.Second)
	for _, p := range mA1.KnownPeers() {
		if p.addrInfo.ID == hB1.ID() {
			t.Errorf("mA1 saw hB1 across APP_TAG boundary")
		}
	}
	for _, p := range mB1.KnownPeers() {
		if p.addrInfo.ID == hA1.ID() || p.addrInfo.ID == hA2.ID() {
			t.Errorf("mB1 saw a tag-a peer across APP_TAG boundary")
		}
	}
}

// TestMeshAdvertiseOffSuppressesPublish proves MESH_ADVERTISE_DISABLED
// (passed as AdvertiseOff=true to startMesh) actually stops the advertise
// loop: a peer with AdvertiseOff=true publishes no heartbeats even after
// the interval elapses, so its sibling never discovers it.
func TestMeshAdvertiseOffSuppressesPublish(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hQuiet := newIntegrationHost(t)
	hListener := newIntegrationHost(t)

	if err := hQuiet.Connect(ctx, peer.AddrInfo{ID: hListener.ID(), Addrs: hListener.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	const tag = "pinner-test-advoff"
	mQuiet, err := startMesh(ctx, hQuiet, MeshOptions{AppTag: tag, Interval: 200 * time.Millisecond, AdvertiseOff: true})
	if err != nil {
		t.Fatalf("startMesh quiet: %v", err)
	}
	t.Cleanup(mQuiet.Stop)
	mListener, err := startMesh(ctx, hListener, MeshOptions{AppTag: tag, Interval: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("startMesh listener: %v", err)
	}
	t.Cleanup(mListener.Stop)

	// Generously wait past several heartbeat intervals; the quiet peer
	// should never be observed.
	time.Sleep(2 * time.Second)
	for _, p := range mListener.KnownPeers() {
		if p.addrInfo.ID == hQuiet.ID() {
			t.Errorf("listener observed AdvertiseOff peer %s — heartbeat suppression failed", hQuiet.ID())
		}
	}
}

// waitForPeer polls m.KnownPeers() up to budget for an entry with the given
// peer ID. Returns true if seen, false on timeout.
func waitForPeer(m *meshManager, id peer.ID, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		for _, p := range m.KnownPeers() {
			if p.addrInfo.ID == id {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestBitswapFetchFromPEvOMain is the canonical end-to-end smoke test: a
// known CID is fetched via bitswap from PEvO main, pinned, and served back
// through the gateway. Set PEVO_PINNER_TEST_CID_SHA256 to additionally
// assert the served bytes match a known content hash — this proves the
// gateway reassembly path, not just the pin record.
// Skipped unless PEVO_MAIN_LIBP2P_ADDR + PEVO_PINNER_TEST_CID are set.
func TestBitswapFetchFromPEvOMain(t *testing.T) {
	bootstrap := os.Getenv("PEVO_MAIN_LIBP2P_ADDR")
	testCID := os.Getenv("PEVO_PINNER_TEST_CID")
	if bootstrap == "" || testCID == "" {
		t.Skip("set PEVO_MAIN_LIBP2P_ADDR + PEVO_PINNER_TEST_CID to run")
	}

	node, err := NewEmbeddedNode(IPFSNodeOptions{
		DataDir:            t.TempDir(),
		GatewayBind:        "127.0.0.1:0",
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

	// Reassembly check: fetch the bytes through the gateway and (optionally)
	// compare them against an expected SHA-256. The Pin succeeded if and
	// only if every block landed; this proves the gateway serves them.
	resp, err := http.Get("http://" + node.gatewayBind + "/ipfs/" + testCID)
	if err != nil {
		t.Fatalf("gateway GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("gateway read: %v", err)
	}
	if len(body) == 0 {
		t.Error("gateway returned empty body for pinned CID")
	}
	if want := os.Getenv("PEVO_PINNER_TEST_CID_SHA256"); want != "" {
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if got != want {
			t.Errorf("gateway body sha256 = %s, want %s", got, want)
		}
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

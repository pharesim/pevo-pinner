package main

import (
	"encoding/json"
	"testing"
	"time"

	peer "github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// TestHeartbeatRoundTripsJSON proves the on-the-wire shape is stable —
// marshal/unmarshal recovers the same fields. The mesh layer trusts JSON to
// preserve peer IDs, multiaddrs, and the optional gateway URL across pubsub
// boundaries; this guards against silent breaking changes to the struct.
func TestHeartbeatRoundTripsJSON(t *testing.T) {
	hb := heartbeat{
		PeerID:     "12D3KooWLwk6jRGrmeT4zcq5Chk7xC5g7sXMdqxTVXgj3NrVm1nd",
		Multiaddrs: []string{"/ip4/127.0.0.1/tcp/4001", "/ip6/::1/udp/4001/quic-v1"},
		GatewayURL: "https://gateway.example.invalid",
		Sent:       time.Now().Unix(),
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back heartbeat
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.PeerID != hb.PeerID || back.GatewayURL != hb.GatewayURL || back.Sent != hb.Sent {
		t.Errorf("roundtrip lost field(s): got %+v, want %+v", back, hb)
	}
	if len(back.Multiaddrs) != len(hb.Multiaddrs) {
		t.Errorf("multiaddr count = %d, want %d", len(back.Multiaddrs), len(hb.Multiaddrs))
	}
}

// TestMeshEvictExpiredDropsStalePeers proves the cache eviction logic
// honors the configured TTL — peers whose lastSeen is past the cutoff are
// dropped, fresher peers are kept. The TTL is sized larger than the
// heartbeat cadence so a one-time dropped heartbeat does not evict a
// healthy peer.
func TestMeshEvictExpiredDropsStalePeers(t *testing.T) {
	m := &meshManager{
		ttl:   30 * time.Second,
		peers: make(map[peer.ID]*meshPeer),
	}
	addr := mustMultiaddr(t, "/ip4/127.0.0.1/tcp/4001")
	fresh := peer.AddrInfo{ID: "12D3KooWFresh", Addrs: []ma.Multiaddr{addr}}
	stale := peer.AddrInfo{ID: "12D3KooWStale", Addrs: []ma.Multiaddr{addr}}
	m.peers[fresh.ID] = &meshPeer{addrInfo: fresh, lastSeen: time.Now()}
	m.peers[stale.ID] = &meshPeer{addrInfo: stale, lastSeen: time.Now().Add(-1 * time.Hour)}

	m.evictExpired(time.Now())

	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.peers[fresh.ID]; !ok {
		t.Error("fresh peer was evicted")
	}
	if _, ok := m.peers[stale.ID]; ok {
		t.Error("stale peer was not evicted")
	}
}

func mustMultiaddr(t *testing.T, s string) ma.Multiaddr {
	t.Helper()
	a, err := ma.NewMultiaddr(s)
	if err != nil {
		t.Fatalf("multiaddr %q: %v", s, err)
	}
	return a
}

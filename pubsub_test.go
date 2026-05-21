package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	crypto "github.com/libp2p/go-libp2p/core/crypto"
	peer "github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// genPeerID produces a real ed25519-backed peer.ID for tests that exercise
// applyHeartbeat's identity decode + compare path. Manufactured string IDs
// fail peer.Decode and would silently skip the very paths we're testing.
func genPeerID(t *testing.T) peer.ID {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("IDFromPrivateKey: %v", err)
	}
	return id
}

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

// fakeHostID stands in for libp2p host.Host's ID() so applyHeartbeat tests
// can run without spinning up a real libp2p host. Only ID() is exercised
// by the code paths under test (sender = self short-circuit and message
// authentication).
const fakeHostID peer.ID = "12D3KooWMyHostID"

// newApplyHeartbeatMesh builds a minimal meshManager wired with whatever
// fields applyHeartbeat needs to run in isolation. host is not exercised
// (the dial goroutine path requires len(addrs)>0 which the impersonation
// tests deliberately avoid).
func newApplyHeartbeatMesh(allowPrivate bool) *meshManager {
	return &meshManager{
		allowPrivateGateways: allowPrivate,
		peers:                make(map[peer.ID]*meshPeer),
	}
}

// TestApplyHeartbeatRejectsImpersonation proves the authoritative peer ID
// is the gossipsub-signed sender, not the in-payload hb.PeerID. A heartbeat
// whose payload claims a victim peer-ID but is published by an attacker is
// dropped — otherwise m.peers[victim] would be overwritten with attacker
// multiaddrs and the attacker's gateway URL would enter the CAR-fetch chain.
func TestApplyHeartbeatRejectsImpersonation(t *testing.T) {
	m := newApplyHeartbeatMesh(false)
	attacker := genPeerID(t)
	victim := genPeerID(t)
	hb := heartbeat{
		PeerID:     victim.String(),
		Multiaddrs: []string{"/ip4/127.0.0.1/tcp/4001"},
	}
	m.applyHeartbeat(context.Background(), attacker, hb)

	if _, ok := m.peers[victim]; ok {
		t.Error("victim peer-ID was inserted from a spoofed heartbeat")
	}
	if _, ok := m.peers[attacker]; ok {
		t.Error("attacker peer-ID was inserted from a heartbeat with mismatched payload PeerID")
	}
}

// TestApplyHeartbeatAcceptsAuthenticatedSender proves the happy path: a
// heartbeat whose payload PeerID matches msg.GetFrom() is cached under the
// sender ID with the advertised metadata.
func TestApplyHeartbeatAcceptsAuthenticatedSender(t *testing.T) {
	m := newApplyHeartbeatMesh(false)
	sender := genPeerID(t)
	// MeshAllowPrivate keeps the test independent of DNS — the SSRF guard
	// resolves hostnames and would otherwise fail on synthetic fixtures.
	m.allowPrivateGateways = true
	hb := heartbeat{
		PeerID:     sender.String(),
		GatewayURL: "https://gateway.example.invalid",
	}
	m.applyHeartbeat(context.Background(), sender, hb)

	cached, ok := m.peers[sender]
	if !ok {
		t.Fatal("authenticated heartbeat was not cached")
	}
	if cached.gatewayURL != "https://gateway.example.invalid" {
		t.Errorf("gatewayURL = %q, want https://gateway.example.invalid", cached.gatewayURL)
	}
}

// TestApplyHeartbeatStripsUnsafeGatewayURL proves the SSRF guard fires
// without dropping the peer's libp2p AddrInfo. The peer is still tracked
// (bitswap is hash-verified end to end), but its gateway URL is wiped so
// it cannot enter the CAR-fetch fallback chain.
func TestApplyHeartbeatStripsUnsafeGatewayURL(t *testing.T) {
	m := newApplyHeartbeatMesh(false)
	sender := genPeerID(t)
	for _, bad := range []string{
		"http://localhost:5432",
		"http://127.0.0.1/internal",
		"http://169.254.169.254/latest/meta-data",
		"file:///etc/passwd",
		"ftp://elsewhere",
	} {
		t.Run(bad, func(t *testing.T) {
			m.peers = make(map[peer.ID]*meshPeer)
			hb := heartbeat{PeerID: sender.String(), GatewayURL: bad}
			m.applyHeartbeat(context.Background(), sender, hb)
			cached, ok := m.peers[sender]
			if !ok {
				t.Fatalf("peer dropped entirely for unsafe URL %q", bad)
			}
			if cached.gatewayURL != "" {
				t.Errorf("gatewayURL = %q, want empty (SSRF guard should strip)", cached.gatewayURL)
			}
		})
	}
}

// TestApplyHeartbeatAllowsPrivateWhenOpted proves MeshAllowPrivate opens the
// SSRF guard for trusted-network meshes.
func TestApplyHeartbeatAllowsPrivateWhenOpted(t *testing.T) {
	m := newApplyHeartbeatMesh(true)
	sender := genPeerID(t)
	hb := heartbeat{PeerID: sender.String(), GatewayURL: "http://10.0.0.5:8080"}
	m.applyHeartbeat(context.Background(), sender, hb)
	cached, ok := m.peers[sender]
	if !ok || cached.gatewayURL == "" {
		t.Errorf("private URL rejected despite MeshAllowPrivate; cached=%+v ok=%v", cached, ok)
	}
}

// TestApplyHeartbeatCapsMultiaddrs proves the per-peer multiaddr list is
// truncated at meshMaxMultiaddrs so an attacker cannot store an unbounded
// dial-table in m.peers.
func TestApplyHeartbeatCapsMultiaddrs(t *testing.T) {
	m := newApplyHeartbeatMesh(false)
	sender := genPeerID(t)
	addrs := make([]string, 100)
	for i := range addrs {
		addrs[i] = "/ip4/198.51.100.1/tcp/" + itoa(4000+i)
	}
	hb := heartbeat{PeerID: sender.String(), Multiaddrs: addrs}
	m.applyHeartbeat(context.Background(), sender, hb)

	cached, ok := m.peers[sender]
	if !ok {
		t.Fatal("peer dropped")
	}
	if got, want := len(cached.addrInfo.Addrs), meshMaxMultiaddrs; got > want {
		t.Errorf("stored multiaddrs = %d, want <= %d", got, want)
	}
}

// TestApplyHeartbeatCapsPeerCount proves a peer-flood attack cannot grow the
// cache past meshMaxPeers. New peers past the cap are dropped (TTL eviction
// is the intended path; this is the safety belt).
func TestApplyHeartbeatCapsPeerCount(t *testing.T) {
	m := newApplyHeartbeatMesh(false)
	// Fill the cache with placeholder entries; rejection of new peers checks
	// len(m.peers) only, so the placeholder IDs need not be real peer IDs.
	for i := 0; i < meshMaxPeers; i++ {
		pid := peer.ID("placeholder-" + itoa(i))
		m.peers[pid] = &meshPeer{lastSeen: time.Now()}
	}
	newcomer := genPeerID(t)
	hb := heartbeat{PeerID: newcomer.String()}
	m.applyHeartbeat(context.Background(), newcomer, hb)

	if _, ok := m.peers[newcomer]; ok {
		t.Error("newcomer was accepted past meshMaxPeers cap")
	}
}

// TestValidateMeshGatewayURL covers the scheme + host accept/reject matrix.
// Literal IPs avoid making the test depend on DNS resolution at run time;
// the DNS-lookup branch is intentionally exercised by allowPrivate=true on
// synthetic hostnames (which would otherwise resolve to a public IP and pass).
func TestValidateMeshGatewayURL(t *testing.T) {
	cases := []struct {
		in           string
		allowPrivate bool
		wantOK       bool
	}{
		{"https://1.1.1.1", false, true},
		{"http://8.8.8.8/", false, true},
		{"HTTPS://1.1.1.1/path/", false, true},
		{"ftp://1.1.1.1", false, false},
		{"file:///etc/passwd", false, false},
		{"http://localhost", false, false},
		{"http://127.0.0.1", false, false},
		{"http://10.0.0.1", false, false},
		{"http://169.254.169.254", false, false},
		{"http://10.0.0.1", true, true},
		{"http://userinfo@1.1.1.1", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		_, err := validateMeshGatewayURL(c.in, c.allowPrivate)
		gotOK := err == nil
		if gotOK != c.wantOK {
			t.Errorf("validateMeshGatewayURL(%q, allowPrivate=%v) ok=%v err=%v, want ok=%v",
				c.in, c.allowPrivate, gotOK, err, c.wantOK)
		}
	}
}

// TestValidateMeshGatewayURLNormalizes proves the normalized form is what
// downstream dedupe compares against — trailing slash, case, query, fragment.
// allowPrivate avoids the DNS lookup branch for this fixture host.
func TestValidateMeshGatewayURLNormalizes(t *testing.T) {
	got, err := validateMeshGatewayURL("https://Example.COM/path/?q=1#frag", true)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	want := "https://example.com/path"
	if got != want {
		t.Errorf("normalized = %q, want %q", got, want)
	}
}

// TestMeshStopIsIdempotent proves the sync.Once gate on Stop: calling Stop
// twice (or N times) is safe even though the second invocation would
// otherwise double-Cancel the subscription and double-Close the topic.
func TestMeshStopIsIdempotent(t *testing.T) {
	m := &meshManager{
		peers: make(map[peer.ID]*meshPeer),
	}
	// Should be a no-op (nothing initialised), but must not panic on
	// repeat invocation.
	m.Stop()
	m.Stop()
}

// itoa is strconv.Itoa shorthand for table-driven tests without pulling
// the dependency name into every line.
func itoa(n int) string {
	return strings.TrimSpace((func() string {
		var b [20]byte
		i := len(b)
		neg := n < 0
		if neg {
			n = -n
		}
		for n > 0 || i == len(b) {
			i--
			b[i] = byte('0' + n%10)
			n /= 10
		}
		if neg {
			i--
			b[i] = '-'
		}
		return string(b[i:])
	}()))
}

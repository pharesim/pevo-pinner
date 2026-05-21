package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	host "github.com/libp2p/go-libp2p/core/host"
	peer "github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// meshTopicFormat namespaces the pubsub topic per APP_TAG so the `pevo` and
// `pevotest` meshes stay isolated even when they share the same wider
// libp2p network.
const meshTopicFormat = "/pevo-pinners/%s/heartbeat/1.0.0"

// MeshOptions configures the community pinner mesh layer.
type MeshOptions struct {
	AppTag       string
	GatewayURL   string        // optional: published in heartbeats so other pinners can add us to their CAR-fetch fallback
	Interval     time.Duration // heartbeat cadence (default 30s)
	CacheTTL     time.Duration // received-heartbeat eviction window (default 5min)
	AdvertiseOff bool          // true = subscribe-only (consume mesh, don't publish)

	// AllowPrivateGateways relaxes the SSRF guard on heartbeat-advertised
	// gateway URLs so loopback / RFC1918 hosts are accepted. Off by default;
	// only operators running a private mesh on a trusted network should set
	// this. Without it, a malicious mesh subscriber publishing
	// GatewayURL=http://169.254.169.254/ or http://localhost:5432/ would
	// otherwise let every pinner pivot into the operator's intranet.
	AllowPrivateGateways bool
}

// defaults for mesh tunables — picked so a single dropped heartbeat does not
// evict a healthy peer (TTL >> interval), and so newly-online pinners are
// discovered within a minute.
const (
	defaultMeshInterval = 30 * time.Second
	defaultMeshTTL      = 5 * time.Minute

	// meshMaxMultiaddrs bounds the per-heartbeat multiaddr list a single
	// peer can store; legitimate hosts advertise a handful, so anything
	// above this is either misconfigured or attacker-controlled.
	meshMaxMultiaddrs = 8

	// meshMaxPeers bounds the known-peers cache. A bounded cache stops a
	// peer-flood attack from exhausting memory; under steady state legitimate
	// fleets are far smaller than this.
	meshMaxPeers = 1024

	// meshPubsubMaxMessageSize caps the on-wire heartbeat. Heartbeats are a
	// few hundred bytes; 16 KiB leaves room for multiaddr lists at the cap
	// above while preventing 1 MiB GossipSub default floods.
	meshPubsubMaxMessageSize = 16 * 1024
)

// heartbeat is the on-the-wire shape pinners publish on the mesh topic.
// JSON keeps the wire format inspectable; the payload is small (a few hundred
// bytes per pinner) so encoding overhead is negligible.
//
// PeerID is informational only — the receiver authoritatively uses the
// gossipsub-authenticated msg.GetFrom() instead of trusting this field.
type heartbeat struct {
	PeerID     string   `json:"peer_id"`
	Multiaddrs []string `json:"multiaddrs"`
	GatewayURL string   `json:"gateway_url,omitempty"`
	Sent       int64    `json:"sent_unix"`
}

// meshPeer is the known-peers cache entry. lastSeen drives TTL eviction; the
// AddrInfo + GatewayURL are what feed bitswap providers and the CAR-fetch
// fallback chain respectively.
type meshPeer struct {
	addrInfo   peer.AddrInfo
	gatewayURL string
	lastSeen   time.Time
}

// meshManager owns the pubsub subscription, the periodic advertise loop, and
// the known-peers cache. Tied to EmbeddedNode's lifecycle: started by
// NewEmbeddedNode (if opts.AppTag is non-empty), stopped by Drain/Close via
// its cancel-and-Wait pair.
type meshManager struct {
	host                 host.Host
	topic                *pubsub.Topic
	sub                  *pubsub.Subscription
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
	stopOnce             sync.Once
	interval             time.Duration
	ttl                  time.Duration
	advertiseOff         bool
	allowPrivateGateways bool
	gatewayURL           string

	mu    sync.RWMutex
	peers map[peer.ID]*meshPeer
}

// startMesh constructs a meshManager on host h and starts the subscribe /
// advertise / TTL-evict goroutines. The returned manager's Stop method is
// idempotent.
func startMesh(parent context.Context, h host.Host, opts MeshOptions) (*meshManager, error) {
	if opts.AppTag == "" {
		return nil, errors.New("mesh: AppTag is required")
	}
	if opts.Interval == 0 {
		opts.Interval = defaultMeshInterval
	}
	if opts.CacheTTL == 0 {
		opts.CacheTTL = defaultMeshTTL
	}

	ps, err := pubsub.NewGossipSub(parent, h, pubsub.WithMaxMessageSize(meshPubsubMaxMessageSize))
	if err != nil {
		return nil, fmt.Errorf("pubsub: %w", err)
	}
	topic, err := ps.Join(fmt.Sprintf(meshTopicFormat, opts.AppTag))
	if err != nil {
		return nil, fmt.Errorf("pubsub join: %w", err)
	}
	sub, err := topic.Subscribe()
	if err != nil {
		_ = topic.Close()
		return nil, fmt.Errorf("pubsub subscribe: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	m := &meshManager{
		host:                 h,
		topic:                topic,
		sub:                  sub,
		cancel:               cancel,
		interval:             opts.Interval,
		ttl:                  opts.CacheTTL,
		advertiseOff:         opts.AdvertiseOff,
		allowPrivateGateways: opts.AllowPrivateGateways,
		gatewayURL:           opts.GatewayURL,
		peers:                make(map[peer.ID]*meshPeer),
	}

	m.wg.Add(1)
	go m.recvLoop(ctx)
	m.wg.Add(1)
	go m.evictLoop(ctx)
	if !opts.AdvertiseOff {
		m.wg.Add(1)
		go m.advertiseLoop(ctx)
	}

	log.Printf("[ipfs] mesh subscribed to %s", topic.String())
	return m, nil
}

// Stop signals all loops to exit, closes the subscription + topic, and
// blocks until the goroutines unwind. Safe to call more than once.
func (m *meshManager) Stop() {
	m.stopOnce.Do(func() {
		m.cancel()
		m.sub.Cancel()
		m.wg.Wait()
		_ = m.topic.Close()
	})
}

// KnownPeers snapshots the current peer cache. Used by the fetch chain to
// compose the dynamic CAR-fetch fallback list.
func (m *meshManager) KnownPeers() []meshPeer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]meshPeer, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, *p)
	}
	return out
}

func (m *meshManager) recvLoop(ctx context.Context) {
	defer m.wg.Done()
	for {
		msg, err := m.sub.Next(ctx)
		if err != nil {
			return // ctx cancelled or subscription closed
		}
		sender := msg.GetFrom()
		if sender == m.host.ID() {
			continue // don't process our own heartbeats
		}
		var hb heartbeat
		if err := json.Unmarshal(msg.Data, &hb); err != nil {
			log.Printf("[ipfs] mesh: malformed heartbeat from %s: %v", sender, err)
			continue
		}
		m.applyHeartbeat(ctx, sender, hb)
	}
}

// applyHeartbeat authenticates and ingests a heartbeat. The authoritative
// peer ID is the gossipsub-signed sender, not the in-payload hb.PeerID — a
// malicious publisher otherwise overwrites m.peers[<victim>] with attacker
// multiaddrs and gateway URL. hb.GatewayURL is scheme/host-validated to
// prevent SSRF when assembleFallbackChain later splices it into HTTP fetches.
func (m *meshManager) applyHeartbeat(ctx context.Context, sender peer.ID, hb heartbeat) {
	if sender == m.host.ID() {
		return
	}
	// Reject impersonation outright: if the publisher claimed a peer ID at
	// all, it must agree with the authenticated sender.
	if hb.PeerID != "" {
		claimed, err := peer.Decode(hb.PeerID)
		if err != nil || claimed != sender {
			return
		}
	}
	if len(hb.Multiaddrs) > meshMaxMultiaddrs {
		hb.Multiaddrs = hb.Multiaddrs[:meshMaxMultiaddrs]
	}
	addrs := make([]ma.Multiaddr, 0, len(hb.Multiaddrs))
	for _, s := range hb.Multiaddrs {
		a, err := ma.NewMultiaddr(s)
		if err != nil {
			continue
		}
		addrs = append(addrs, a)
	}

	gatewayURL := ""
	if hb.GatewayURL != "" {
		normalized, err := validateMeshGatewayURL(hb.GatewayURL, m.allowPrivateGateways)
		if err != nil {
			log.Printf("[ipfs] mesh: rejecting heartbeat from %s: invalid gateway URL %q: %v", sender, hb.GatewayURL, err)
			// Still ingest the libp2p AddrInfo — bitswap is hash-verified —
			// just drop the gateway URL so it can't enter the CAR-fetch chain.
		} else {
			gatewayURL = normalized
		}
	}

	mp := &meshPeer{
		addrInfo:   peer.AddrInfo{ID: sender, Addrs: addrs},
		gatewayURL: gatewayURL,
		lastSeen:   time.Now(),
	}

	m.mu.Lock()
	_, known := m.peers[sender]
	if !known && len(m.peers) >= meshMaxPeers {
		// Cache full and this peer is new — drop the heartbeat rather than
		// evict something legitimate. Eviction by TTL is the intended path.
		m.mu.Unlock()
		return
	}
	m.peers[sender] = mp
	m.mu.Unlock()

	if !known {
		log.Printf("[ipfs] mesh discovered pinner %s (gateway=%s)", sender, fallbackURLLabel(gatewayURL))
		// Best-effort dial: feed bitswap a fresh provider. Tracked in wg so
		// Stop waits for the dial to unwind rather than leaving a goroutine
		// dereferencing m.host concurrently with h.Close.
		if len(addrs) > 0 {
			m.wg.Add(1)
			dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			go func() {
				defer m.wg.Done()
				defer cancel()
				_ = m.host.Connect(dialCtx, mp.addrInfo)
			}()
		}
	}
}

// validateMeshGatewayURL parses and SSRF-guards a gateway URL advertised by
// an untrusted mesh subscriber. Returns the normalized URL on success.
// Loopback, link-local, multicast, and private-network hosts are rejected
// unless allowPrivate is true (operator opt-in for trusted-network meshes).
func validateMeshGatewayURL(raw string, allowPrivate bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("empty host")
	}
	if u.User != nil {
		return "", errors.New("userinfo not allowed")
	}
	if !allowPrivate {
		if isUnsafeHost(host) {
			return "", fmt.Errorf("host %q not allowed", host)
		}
	}
	// Reconstruct without trailing slash so dedupe is consistent regardless of
	// publisher formatting. Lowercase host so case variants don't bypass dedupe.
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.Fragment = ""
	u.RawQuery = ""
	return u.String(), nil
}

// isUnsafeHost reports true if host names or resolves to a non-routable /
// internal address. Conservative: any literal-IP match against
// loopback/private/link-local/multicast counts; for hostnames we resolve and
// reject if ANY answer is unsafe. Resolution failures count as unsafe so a
// malicious DNS rebinding can't bypass the guard.
func isUnsafeHost(host string) bool {
	// Reject literal "localhost" before DNS lookup; some resolvers point it
	// elsewhere depending on /etc/hosts.
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isUnsafeIP(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return true
	}
	for _, ip := range ips {
		if isUnsafeIP(ip) {
			return true
		}
	}
	return false
}

func isUnsafeIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	return false
}

func (m *meshManager) advertiseLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	// Publish immediately so newly-online pinners don't wait a full interval.
	m.publishHeartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.publishHeartbeat(ctx)
		}
	}
}

func (m *meshManager) publishHeartbeat(ctx context.Context) {
	addrs := m.host.Addrs()
	multiaddrs := make([]string, 0, len(addrs))
	for _, a := range addrs {
		multiaddrs = append(multiaddrs, a.String())
	}
	hb := heartbeat{
		PeerID:     m.host.ID().String(),
		Multiaddrs: multiaddrs,
		GatewayURL: m.gatewayURL,
		Sent:       time.Now().Unix(),
	}
	data, err := json.Marshal(hb)
	if err != nil {
		log.Printf("[ipfs] mesh: marshal heartbeat: %v", err)
		return
	}
	if err := m.topic.Publish(ctx, data); err != nil && ctx.Err() == nil {
		log.Printf("[ipfs] mesh: publish heartbeat: %v", err)
	}
}

func (m *meshManager) evictLoop(ctx context.Context) {
	defer m.wg.Done()
	// Evict on a cadence proportionate to interval — half the heartbeat
	// interval keeps the cache fresh without burning CPU on idle pinners.
	tick := m.interval / 2
	if tick <= 0 {
		tick = 15 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.evictExpired(now)
		}
	}
}

func (m *meshManager) evictExpired(now time.Time) {
	cutoff := now.Add(-m.ttl)
	m.mu.Lock()
	var expired []*meshPeer
	for pid, p := range m.peers {
		if p.lastSeen.Before(cutoff) {
			expired = append(expired, p)
			delete(m.peers, pid)
		}
	}
	m.mu.Unlock()
	for _, p := range expired {
		log.Printf("[ipfs] mesh evicted stale pinner %s (last seen %s ago)", p.addrInfo.ID, now.Sub(p.lastSeen).Truncate(time.Second))
	}
}

func fallbackURLLabel(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

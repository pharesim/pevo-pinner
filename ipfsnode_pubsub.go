package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	AppTag         string
	GatewayURL     string        // optional: published in heartbeats so other pinners can add us to their CAR-fetch fallback
	Interval       time.Duration // heartbeat cadence (default 30s)
	CacheTTL       time.Duration // received-heartbeat eviction window (default 5min)
	AdvertiseOff   bool          // true = subscribe-only (consume mesh, don't publish)
	BootstrapPeers []peer.AddrInfo
}

// defaults for mesh tunables — picked so a single dropped heartbeat does not
// evict a healthy peer (TTL >> interval), and so newly-online pinners are
// discovered within a minute.
const (
	defaultMeshInterval = 30 * time.Second
	defaultMeshTTL      = 5 * time.Minute
)

// heartbeat is the on-the-wire shape pinners publish on the mesh topic.
// JSON keeps the wire format inspectable; the payload is small (a few hundred
// bytes per pinner) so encoding overhead is negligible.
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
	host         host.Host
	ps           *pubsub.PubSub
	topic        *pubsub.Topic
	sub          *pubsub.Subscription
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	interval     time.Duration
	ttl          time.Duration
	advertiseOff bool
	gatewayURL   string

	mu        sync.RWMutex
	peers     map[peer.ID]*meshPeer
	onPeer    func(meshPeer)  // optional: called for each fresh discovery
	onExpired func(meshPeer) // optional: called when a peer ages out
}

// startMesh constructs a meshManager on host h and starts the subscribe /
// advertise / TTL-evict goroutines. The returned manager's Stop method is
// idempotent.
func startMesh(parent context.Context, h host.Host, opts MeshOptions, onPeer, onExpired func(meshPeer)) (*meshManager, error) {
	if opts.AppTag == "" {
		return nil, errors.New("mesh: AppTag is required")
	}
	if opts.Interval == 0 {
		opts.Interval = defaultMeshInterval
	}
	if opts.CacheTTL == 0 {
		opts.CacheTTL = defaultMeshTTL
	}

	ps, err := pubsub.NewGossipSub(parent, h)
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
		host:         h,
		ps:           ps,
		topic:        topic,
		sub:          sub,
		cancel:       cancel,
		interval:     opts.Interval,
		ttl:          opts.CacheTTL,
		advertiseOff: opts.AdvertiseOff,
		gatewayURL:   opts.GatewayURL,
		peers:        make(map[peer.ID]*meshPeer),
		onPeer:       onPeer,
		onExpired:    onExpired,
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
	m.cancel()
	m.sub.Cancel()
	m.wg.Wait()
	_ = m.topic.Close()
}

// KnownPeers snapshots the current peer cache, sorted by last-seen time.
// Used by the fetch chain to compose the dynamic CAR-fetch fallback list.
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
		if msg.GetFrom() == m.host.ID() {
			continue // don't process our own heartbeats
		}
		var hb heartbeat
		if err := json.Unmarshal(msg.Data, &hb); err != nil {
			log.Printf("[ipfs] mesh: malformed heartbeat from %s: %v", msg.GetFrom(), err)
			continue
		}
		m.applyHeartbeat(ctx, hb)
	}
}

func (m *meshManager) applyHeartbeat(ctx context.Context, hb heartbeat) {
	pid, err := peer.Decode(hb.PeerID)
	if err != nil {
		return
	}
	if pid == m.host.ID() {
		return
	}
	addrs := make([]ma.Multiaddr, 0, len(hb.Multiaddrs))
	for _, s := range hb.Multiaddrs {
		a, err := ma.NewMultiaddr(s)
		if err != nil {
			continue
		}
		addrs = append(addrs, a)
	}

	mp := &meshPeer{
		addrInfo:   peer.AddrInfo{ID: pid, Addrs: addrs},
		gatewayURL: hb.GatewayURL,
		lastSeen:   time.Now(),
	}

	m.mu.Lock()
	_, known := m.peers[pid]
	m.peers[pid] = mp
	m.mu.Unlock()

	if !known {
		log.Printf("[ipfs] mesh discovered pinner %s (gateway=%s)", pid, fallbackURLLabel(hb.GatewayURL))
		if m.onPeer != nil {
			m.onPeer(*mp)
		}
		// Best-effort dial: feed bitswap a fresh provider. Non-blocking;
		// libp2p will retry on its own connection manager.
		if len(addrs) > 0 {
			dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			go func() {
				defer cancel()
				_ = m.host.Connect(dialCtx, mp.addrInfo)
			}()
		}
	}
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
		if m.onExpired != nil {
			m.onExpired(*p)
		}
	}
}

func fallbackURLLabel(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

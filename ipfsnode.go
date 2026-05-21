package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	blockservice "github.com/ipfs/boxo/blockservice"
	bsblockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange"
	offlinexchange "github.com/ipfs/boxo/exchange/offline"
	merkledag "github.com/ipfs/boxo/ipld/merkledag"
	cid "github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
	flatfs "github.com/ipfs/go-ds-flatfs"
	host "github.com/libp2p/go-libp2p/core/host"
	routing "github.com/libp2p/go-libp2p/core/routing"
)

// ErrPinnerShuttingDown is returned by Pin once Drain or Close has been
// invoked. Permanent for the process lifetime; not retryable.
var ErrPinnerShuttingDown = errors.New("pinner: shutting down")

// EmbeddedNode is the in-process IPFS node: libp2p host, DHT-backed routing,
// flatfs blockstore, bitswap exchange, and a boxo HTTP gateway serving
// pinned-only content. Pinned content is fetched via bitswap (block-level
// hash-verified by construction); the offline blockservice used by the
// gateway guarantees the HTTP path never triggers a network fetch.
type EmbeddedNode struct {
	dataDir     string
	gatewayPort string

	// libp2p + routing.
	host    host.Host
	dht     routing.Routing
	dhtStop func() error

	// Blockstore + exchanges.
	rawDatastore ds.Batching
	blockstore   bsblockstore.Blockstore
	bitswap      exchange.Interface
	bsBlockSvc   blockservice.BlockService // online: bitswap-backed, used for Pin DAG walks
	offlineSvc   blockservice.BlockService // offline: pinned-only, used for the HTTP gateway

	// HTTP gateway (boxo handler).
	server *http.Server

	// Recursive-pin set persisted as pins.json. Maintained alongside the
	// blockstore — keeps the data shape simple and the migration story
	// trivial (no boxo pinset / GC interactions yet).
	mu      sync.RWMutex
	pins    map[string]bool
	pinFile string

	// Per-Pin DAG-walk timeout (bitswap session). On timeout, the CAR-fetch
	// fallback chain runs in order: fallbackGateways (PEvO main first if
	// configured via the IPFSNodeOptions PEvOMainGatewayURL knob, then any
	// operator-supplied FALLBACK_GATEWAYS, then defaultFallbackGateways).
	// The mesh layer (if enabled) appends discovered pinners' gateway URLs
	// to a per-Pin chain via meshGatewayURLs.
	bitswapTimeout   time.Duration
	fallbackGateways []string
	httpClient       *http.Client

	// Community pinner mesh. nil when MeshAppTag was empty.
	mesh *meshManager

	// Drain coordination. drainMu serializes the gate check + WaitGroup.Add
	// in Pin with the close + Wait in Drain. done is closed exactly once via
	// doneOnce; inFlight tracks in-flight Pins so Drain can wait. cancels
	// holds a CancelFunc per in-flight Pin so Drain can force-cancel the
	// bitswap session past its deadline.
	drainMu   sync.Mutex
	done      chan struct{}
	doneOnce  sync.Once
	inFlight  sync.WaitGroup
	cancels   map[uint64]context.CancelFunc
	nextPinID uint64
}

// IPFSNodeOptions configures EmbeddedNode construction. Defaults are sane for
// the common operator (random libp2p port, ~/.pevo-pinner repo, no anchor
// peer). Explicit values override.
type IPFSNodeOptions struct {
	DataDir     string
	GatewayPort string

	// libp2p listen multiaddrs. Empty means libp2p's default (random port).
	Libp2pListen []string

	// PEvO main libp2p multiaddr (bootstrap peer). Empty means no anchor —
	// the pinner relies on the DHT alone to find providers, which is
	// fine but slower at startup.
	PEvOMainLibp2pAddr string

	// Per-Pin bitswap session timeout. Zero falls back to the default below.
	BitswapTimeout time.Duration

	// PEvO main HTTP gateway URL (trustless CAR endpoint). Empty means
	// no PEvO-main entry in the fallback chain — the chain falls back to
	// operator-configured FALLBACK_GATEWAYS plus the default public set.
	PEvOMainGatewayURL string

	// Operator-supplied additional CAR-fetch gateway URLs. Inserted into
	// the chain after PEvO main, before the default public gateways.
	FallbackGateways []string

	// Community pinner mesh (libp2p pubsub) knobs. AppTag enables the mesh
	// (typically the same APP_TAG used by HAF discovery). PublicGatewayURL
	// is what other pinners learn from our heartbeats so they can chain
	// us into their CAR-fetch fallback list — leave empty if the pinner's
	// gateway port is not reachable from the public internet.
	MeshAppTag            string
	MeshPublicGatewayURL  string
	MeshHeartbeatInterval time.Duration
	MeshCacheTTL          time.Duration
	MeshAdvertiseDisabled bool
}

// defaultBitswapTimeout: brainstorm-stated default. Tunable via BITSWAP_TIMEOUT.
const defaultBitswapTimeout = 60 * time.Second

// NewEmbeddedNode constructs the in-process IPFS node: opens the flatfs repo,
// constructs the libp2p host + DHT, wires bitswap, and starts the HTTP gateway.
func NewEmbeddedNode(opts IPFSNodeOptions) (*EmbeddedNode, error) {
	if opts.DataDir == "" {
		return nil, errors.New("ipfs: DataDir is required")
	}
	if opts.GatewayPort == "" {
		return nil, errors.New("ipfs: GatewayPort is required")
	}
	if opts.BitswapTimeout == 0 {
		opts.BitswapTimeout = defaultBitswapTimeout
	}

	repoDir := filepath.Join(opts.DataDir, "ipfs-repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating ipfs repo dir: %w", err)
	}

	// Datastore + blockstore.
	dstore, bs, err := openRepo(repoDir)
	if err != nil {
		return nil, fmt.Errorf("opening repo: %w", err)
	}

	// libp2p host + DHT. Bootstrap dial to PEvO main runs as a non-blocking
	// task; failure logs a warning but does not block startup.
	ctx := context.Background()
	h, dht, dhtStop, err := newLibp2pHost(ctx, repoDir, opts.Libp2pListen, opts.PEvOMainLibp2pAddr)
	if err != nil {
		_ = dstore.Close()
		return nil, fmt.Errorf("libp2p host: %w", err)
	}

	// Bitswap (online) + offline exchange (gateway).
	bsw, err := newBitswap(ctx, h, dht, bs)
	if err != nil {
		_ = h.Close()
		_ = dstore.Close()
		return nil, fmt.Errorf("bitswap: %w", err)
	}

	onlineSvc := blockservice.New(bs, bsw)
	offlineSvc := blockservice.New(bs, offlinexchange.Exchange(bs))

	// Compose the fallback chain: PEvO main (if set) → operator extras →
	// defaults. Deterministic ordering preserves the brainstorm-stated
	// preference for the trusted anchor before generic public endpoints.
	fallback := make([]string, 0, 1+len(opts.FallbackGateways)+len(defaultFallbackGateways))
	if opts.PEvOMainGatewayURL != "" {
		fallback = append(fallback, opts.PEvOMainGatewayURL)
	}
	fallback = append(fallback, opts.FallbackGateways...)
	fallback = append(fallback, defaultFallbackGateways...)

	node := &EmbeddedNode{
		dataDir:          opts.DataDir,
		gatewayPort:      opts.GatewayPort,
		host:             h,
		dht:              dht,
		dhtStop:          dhtStop,
		rawDatastore:     dstore,
		blockstore:       bs,
		bitswap:          bsw,
		bsBlockSvc:       onlineSvc,
		offlineSvc:       offlineSvc,
		pins:             make(map[string]bool),
		pinFile:          filepath.Join(opts.DataDir, "pins.json"),
		bitswapTimeout:   opts.BitswapTimeout,
		fallbackGateways: fallback,
		httpClient:       newHTTPClient(),
		done:             make(chan struct{}),
		cancels:          make(map[uint64]context.CancelFunc),
	}

	if err := node.loadPins(); err != nil && !os.IsNotExist(err) {
		log.Printf("[ipfs] failed to load pin state: %v", err)
	}

	if err := node.startGateway(); err != nil {
		_ = node.shutdownInternals()
		return nil, fmt.Errorf("starting gateway: %w", err)
	}

	// Community pinner mesh is opt-in via MeshAppTag — empty tag = mesh off.
	if opts.MeshAppTag != "" {
		mesh, err := startMesh(ctx, h, MeshOptions{
			AppTag:       opts.MeshAppTag,
			GatewayURL:   opts.MeshPublicGatewayURL,
			Interval:     opts.MeshHeartbeatInterval,
			CacheTTL:     opts.MeshCacheTTL,
			AdvertiseOff: opts.MeshAdvertiseDisabled,
		}, nil, nil)
		if err != nil {
			log.Printf("[ipfs] mesh disabled: %v", err)
		} else {
			node.mesh = mesh
		}
	}

	log.Printf("[ipfs] libp2p host started: %s", h.ID())
	for _, addr := range h.Addrs() {
		log.Printf("[ipfs]   listen: %s/p2p/%s", addr, h.ID())
	}
	log.Printf("[ipfs] bitswap timeout: %s", opts.BitswapTimeout)

	return node, nil
}

// Pin fetches the DAG rooted at cidStr via bitswap, recording the CID as a
// recursive pin once every block has been imported (and therefore
// hash-verified by bitswap). On Drain or Close, returns ErrPinnerShuttingDown
// immediately.
func (n *EmbeddedNode) Pin(ctx context.Context, cidStr string) error {
	// Gate + register cancel under drainMu. Mirrors the prior contract:
	// once Drain closes done, no fresh Pin can Add to inFlight, so Wait
	// observes only the strictly-pre-Drain in-flight set.
	n.drainMu.Lock()
	select {
	case <-n.done:
		n.drainMu.Unlock()
		return ErrPinnerShuttingDown
	default:
	}
	n.inFlight.Add(1)
	n.nextPinID++
	pinID := n.nextPinID
	pinCtx, pinCancel := context.WithCancel(ctx)
	n.cancels[pinID] = pinCancel
	n.drainMu.Unlock()
	defer func() {
		n.drainMu.Lock()
		delete(n.cancels, pinID)
		n.drainMu.Unlock()
		pinCancel()
		n.inFlight.Done()
	}()

	if err := ValidateCID(cidStr); err != nil {
		return err
	}

	c, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("decoding CID %s: %w", cidStr, err)
	}

	// Tier 1: bitswap. merkledag.FetchGraph walks the dag-pb / UnixFS DAG
	// and pulls every referenced block; bitswap hash-verifies each block
	// against its CID on receipt. Per-Pin timeout bounds the bitswap leg.
	bswCtx, bswCancel := context.WithTimeout(pinCtx, n.bitswapTimeout)
	dag := merkledag.NewDAGService(n.bsBlockSvc)
	bswErr := merkledag.FetchGraph(bswCtx, c, dag)
	bswCancel()
	if bswErr == nil {
		n.markPinned(cidStr)
		log.Printf("[ipfs] pinned %s (bitswap)", cidStr)
		return nil
	}

	// Tier 2/3: trustless CAR-fetch fallback chain. Each gateway is tried
	// in order; boxo CAR import hash-verifies every block on read so a
	// hostile gateway returning bad bytes is rejected at the parser.
	// pinCtx (not bswCtx) bounds the fallback so the operator's outer
	// ctx still applies, but the bitswap timeout no longer cuts us short.
	if carErr := n.fetchViaCAR(pinCtx, c); carErr != nil {
		return fmt.Errorf("fetching DAG for %s: bitswap: %w; CAR fallback: %v", cidStr, bswErr, carErr)
	}
	// CAR import populates the blockstore. Re-walk locally to confirm the
	// graph is complete (CAR streams might be truncated; the second walk
	// is offline-only and cheap when blocks are already on disk).
	walkCtx, walkCancel := context.WithTimeout(pinCtx, 30*time.Second)
	defer walkCancel()
	if err := merkledag.FetchGraph(walkCtx, c, merkledag.NewDAGService(n.offlineSvc)); err != nil {
		return fmt.Errorf("CAR-fetched DAG incomplete for %s: %w", cidStr, err)
	}
	n.markPinned(cidStr)
	log.Printf("[ipfs] pinned %s (CAR fallback)", cidStr)
	return nil
}

func (n *EmbeddedNode) markPinned(cidStr string) {
	n.mu.Lock()
	n.pins[cidStr] = true
	n.mu.Unlock()
	n.savePins()
}

func (n *EmbeddedNode) Unpin(_ context.Context, cidStr string) error {
	if err := ValidateCID(cidStr); err != nil {
		return err
	}
	n.mu.Lock()
	delete(n.pins, cidStr)
	n.mu.Unlock()
	n.savePins()
	// Blocks remain in the blockstore until GC runs (not implemented yet);
	// removing the pin alone is sufficient for the IPFSBackend contract.
	log.Printf("[ipfs] unpinned %s", cidStr)
	return nil
}

func (n *EmbeddedNode) IsPinned(_ context.Context, cidStr string) (bool, error) {
	if err := ValidateCID(cidStr); err != nil {
		return false, err
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.pins[cidStr], nil
}

func (n *EmbeddedNode) PinnedCIDs(_ context.Context) ([]string, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	cids := make([]string, 0, len(n.pins))
	for c := range n.pins {
		cids = append(cids, c)
	}
	return cids, nil
}

// Drain stops accepting new Pin calls and blocks until in-flight ones finish
// or ctx expires. On hard-deadline expiry every in-flight Pin's child ctx is
// force-cancelled so the bitswap session unwinds; a brief grace window lets
// any deferred cleanup complete.
func (n *EmbeddedNode) Drain(ctx context.Context) error {
	n.drainMu.Lock()
	n.signalDone()
	n.drainMu.Unlock()

	finished := make(chan struct{})
	go func() {
		n.inFlight.Wait()
		close(finished)
	}()

	select {
	case <-finished:
		log.Printf("[ipfs] drain complete")
		return nil
	case <-ctx.Done():
		n.drainMu.Lock()
		pending := len(n.cancels)
		for _, cancel := range n.cancels {
			cancel()
		}
		n.drainMu.Unlock()
		if pending > 0 {
			log.Printf("[ipfs] drain deadline hit, force-cancelled %d in-flight pin(s): %v", pending, ctx.Err())
		} else {
			log.Printf("[ipfs] drain timed out with no in-flight pins to cancel: %v", ctx.Err())
		}
		select {
		case <-finished:
			log.Printf("[ipfs] post-cancel cleanup complete")
		case <-time.After(2 * time.Second):
			log.Printf("[ipfs] post-cancel cleanup grace expired")
		}
		return ctx.Err()
	}
}

// Close tears down the gateway server, blockstore, libp2p host, and DHT. Drain
// should be called first; Close on its own does not wait for in-flight pins.
func (n *EmbeddedNode) Close() error {
	n.signalDone()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var firstErr error
	if n.server != nil {
		if err := n.server.Shutdown(shutdownCtx); err != nil {
			firstErr = err
		}
	}
	if err := n.shutdownInternals(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (n *EmbeddedNode) shutdownInternals() error {
	var firstErr error
	if n.mesh != nil {
		n.mesh.Stop()
	}
	if closer, ok := n.bitswap.(interface{ Close() error }); ok && closer != nil {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if n.dhtStop != nil {
		if err := n.dhtStop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if n.host != nil {
		if err := n.host.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if n.rawDatastore != nil {
		// flatfs's Close on *Datastore implements ds.Datastore.Close().
		type closer interface{ Close() error }
		if c, ok := n.rawDatastore.(closer); ok {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (n *EmbeddedNode) signalDone() {
	n.doneOnce.Do(func() {
		close(n.done)
		log.Printf("[ipfs] pin acceptance stopped")
	})
}

func (n *EmbeddedNode) loadPins() error {
	data, err := os.ReadFile(n.pinFile)
	if err != nil {
		return err
	}
	var cids []string
	if err := json.Unmarshal(data, &cids); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range cids {
		n.pins[c] = true
	}
	return nil
}

func (n *EmbeddedNode) savePins() {
	n.mu.RLock()
	cids := make([]string, 0, len(n.pins))
	for c := range n.pins {
		cids = append(cids, c)
	}
	n.mu.RUnlock()

	data, err := json.MarshalIndent(cids, "", "  ")
	if err != nil {
		log.Printf("[ipfs] failed to marshal pins: %v", err)
		return
	}
	if err := atomicWriteFile(n.pinFile, data, 0o644); err != nil {
		log.Printf("[ipfs] failed to save pins: %v", err)
	}
}

// flatfs.Datastore implements ds.Batching at the value level. The assertion
// keeps the field's concrete shape near the type definition where it can be
// audited alongside the schema sharding parameter.
var _ ds.Batching = (*flatfs.Datastore)(nil)

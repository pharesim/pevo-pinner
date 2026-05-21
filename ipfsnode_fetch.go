package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	bitswap "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	bsblockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange"
	blocks "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	carv2 "github.com/ipld/go-car/v2"
	host "github.com/libp2p/go-libp2p/core/host"
	routing "github.com/libp2p/go-libp2p/core/routing"
)

// defaultFallbackGateways carries forward the public-gateway set used by the
// prior HTTP-cache implementation. They serve as the tail of the CAR-fetch
// fallback chain — only consulted after bitswap has timed out and any
// operator-configured PEvO-main gateway / mesh-discovered pinner gateways
// have failed. Trustless CAR transfer + boxo CAR import preserve the
// block-level hash-verification property even against an arbitrary gateway.
var defaultFallbackGateways = []string{
	"https://ipfs.io",
	"https://dweb.link",
	"https://cloudflare-ipfs.com",
	"https://gateway.pinata.cloud",
}

// perGatewayFetchTimeout bounds wall-clock per fallback-chain entry so a
// gateway that accepts the TCP connection and then trickles bytes does not
// hold the chain for the full http.Client.Timeout. The chain still respects
// the caller's pinCtx as an outer bound.
const perGatewayFetchTimeout = 90 * time.Second

// defaultMaxPinBytes caps the total bytes accepted from any single CAR-fetch
// response. The CAR parser hash-verifies every block so content authority is
// preserved, but a hostile gateway can still attempt to fill disk with valid
// blocks belonging to unrelated DAGs; this is the disk-fill DoS guard.
const defaultMaxPinBytes = int64(1 << 30) // 1 GiB

// carImportBatchSize is the number of CAR blocks accumulated before a single
// PutMany call to flatfs. flatfs treats PutMany as a batched transaction so
// fsync-per-block storms on large DAGs collapse to a single fsync per batch.
const carImportBatchSize = 256

// newBitswap wires bitswap onto the libp2p host with the DHT as its content
// discovery layer and the local blockstore as its block store.
func newBitswap(ctx context.Context, h host.Host, dht routing.Routing, bs bsblockstore.Blockstore) (exchange.Interface, error) {
	net := bsnet.NewFromIpfsHost(h)
	bsw := bitswap.New(ctx, net, dht, bs)
	return bsw, nil
}

// fetchViaCAR fetches the DAG rooted at c from each gateway in order until
// one returns a CAR stream that imports without error. Each block read from
// the CAR is hash-verified by go-car/v2's BlockReader (the CID in the CAR
// must match the digest of the block bytes), then written to the blockstore.
// Returns nil on the first success, or a joined error if every gateway in
// the chain fails.
//
// The chain order is: PEvO main → operator-supplied FALLBACK_GATEWAYS →
// mesh-discovered pinners' gateway URLs → public defaults. Mesh entries are
// recomputed per-call (the cache lives behind meshManager) so newly-online
// pinners can be tried mid-session without restart.
func (n *EmbeddedNode) fetchViaCAR(ctx context.Context, c cid.Cid) error {
	chain := n.assembleFallbackChain()
	if len(chain) == 0 {
		return errors.New("no fallback gateways configured")
	}
	var errs []error
	for _, gw := range chain {
		gwCtx, gwCancel := context.WithTimeout(ctx, perGatewayFetchTimeout)
		err := n.fetchCARFromGateway(gwCtx, gw, c)
		gwCancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", gw, err))
			log.Printf("[ipfs] CAR fallback %s failed: %v", gw, err)
			continue
		}
		log.Printf("[ipfs] CAR fallback %s imported %s", gw, c)
		return nil
	}
	return fmt.Errorf("all CAR-fetch gateways failed: %w", errors.Join(errs...))
}

// assembleFallbackChain merges the static configured chain with any
// mesh-discovered pinner gateway URLs. Mesh entries are inserted between
// the operator-supplied FALLBACK_GATEWAYS and the public defaults, so the
// declared order remains: PEvO main → operator extras → mesh → public.
//
// The split point staticHeadLen is recorded at construction (it is the count
// of non-default entries the operator supplied, PEvO main included) so this
// function is O(N) on chain length and not vulnerable to a hostile operator
// FALLBACK_GATEWAYS entry that happens to equal a default URL.
func (n *EmbeddedNode) assembleFallbackChain() []string {
	if n.mesh == nil {
		return n.fallbackGateways
	}
	peers := n.mesh.KnownPeers()
	if len(peers) == 0 {
		return n.fallbackGateways
	}
	headEnd := n.staticHeadLen
	if headEnd > len(n.fallbackGateways) {
		headEnd = len(n.fallbackGateways)
	}

	seen := make(map[string]struct{}, len(n.fallbackGateways)+len(peers))
	out := make([]string, 0, len(n.fallbackGateways)+len(peers))
	add := func(u string) {
		k := normalizeFallbackURL(u)
		if k == "" {
			return
		}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, u)
	}
	for _, u := range n.fallbackGateways[:headEnd] {
		add(u)
	}
	for _, p := range peers {
		add(p.gatewayURL)
	}
	for _, u := range n.fallbackGateways[headEnd:] {
		add(u)
	}
	return out
}

// normalizeFallbackURL collapses trailing-slash / case / default-port URL
// variants to a single key for dedupe. Non-URL inputs return "" (treated as
// a no-op by the caller).
func normalizeFallbackURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.Fragment = ""
	u.RawQuery = ""
	return u.String()
}

// fetchCARFromGateway streams a trustless CAR response from gw and writes
// every block to the blockstore. The Accept header is the contract; servers
// that ignore it and return reassembled UnixFS bytes are rejected at the CAR
// parser. Hash verification per block is built into carv2's BlockReader.
//
// The response body is wrapped in a byte-counted limit reader bounded by
// maxPinBytes+1 — exceeding the cap aborts the import with a clear error
// rather than silently filling disk. Blocks are accumulated and flushed via
// blockstore.PutMany to collapse per-block fsync storms on flatfs.
func (n *EmbeddedNode) fetchCARFromGateway(ctx context.Context, gw string, c cid.Cid) error {
	url := strings.TrimRight(gw, "/") + "/ipfs/" + c.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.ipld.car")
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain the body up to a small cap so the connection can be reused;
		// we don't care about the content past status. The trustless gateway
		// spec returns the CAR body even on errors sometimes, but a
		// non-200 means we should fall through to the next gateway anyway.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	counter := &byteCountReader{r: io.LimitReader(resp.Body, n.maxPinBytes+1)}
	br, err := carv2.NewBlockReader(counter)
	if err != nil {
		return fmt.Errorf("CAR header: %w", err)
	}
	var imported int
	batch := make([]blocks.Block, 0, carImportBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := n.blockstore.PutMany(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for {
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("CAR block %d: %w", imported, err)
		}
		if counter.n > n.maxPinBytes {
			return fmt.Errorf("CAR stream exceeded MAX_PIN_BYTES (%d)", n.maxPinBytes)
		}
		batch = append(batch, blk)
		imported++
		if len(batch) >= carImportBatchSize {
			if err := flush(); err != nil {
				return fmt.Errorf("blockstore put-many: %w", err)
			}
		}
	}
	if err := flush(); err != nil {
		return fmt.Errorf("blockstore put-many: %w", err)
	}
	if imported == 0 {
		return errors.New("CAR stream contained no blocks")
	}
	return nil
}

// byteCountReader counts bytes read off the wrapped reader so the CAR import
// can detect a stream that has exceeded its byte cap without relying on the
// CAR parser to surface a clean error.
type byteCountReader struct {
	r io.Reader
	n int64
}

func (b *byteCountReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	return n, err
}

// newHTTPClient builds the HTTP client used for the CAR-fetch fallback. The
// timeout is intentionally generous: CAR streams for large papers can take
// tens of seconds; the per-Pin BITSWAP_TIMEOUT bounds the bitswap leg, and
// the ctx passed to each call bounds the overall Pin.
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

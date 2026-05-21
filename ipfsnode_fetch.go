package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	bitswap "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	bsblockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange"
	cid "github.com/ipfs/go-cid"
	carv2 "github.com/ipld/go-car/v2"
	host "github.com/libp2p/go-libp2p/core/host"
	routing "github.com/libp2p/go-libp2p/core/routing"
)

// defaultFallbackGateways carries forward the public-gateway set used by the
// prior HTTP-cache implementation. They serve as the tail of the CAR-fetch
// fallback chain — only consulted after bitswap has timed out and any
// operator-configured PEvO-main gateway / mesh peers (U5) have failed.
// Trustless CAR transfer + boxo CAR import preserve the block-level
// hash-verification property even against an arbitrary gateway.
var defaultFallbackGateways = []string{
	"https://ipfs.io",
	"https://dweb.link",
	"https://cloudflare-ipfs.com",
	"https://gateway.pinata.cloud",
}

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
func (n *EmbeddedNode) fetchViaCAR(ctx context.Context, c cid.Cid) error {
	if len(n.fallbackGateways) == 0 {
		return errors.New("no fallback gateways configured")
	}
	var errs []error
	for _, gw := range n.fallbackGateways {
		if err := n.fetchCARFromGateway(ctx, gw, c); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", gw, err))
			log.Printf("[ipfs] CAR fallback %s failed: %v", gw, err)
			continue
		}
		log.Printf("[ipfs] CAR fallback %s imported %s", gw, c)
		return nil
	}
	return fmt.Errorf("all CAR-fetch gateways failed: %w", errors.Join(errs...))
}

// fetchCARFromGateway streams a trustless CAR response from gw and writes
// every block to the blockstore. The Accept header is the contract; servers
// that ignore it and return reassembled UnixFS bytes are rejected at the CAR
// parser. Hash verification per block is built into carv2's BlockReader.
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

	br, err := carv2.NewBlockReader(resp.Body)
	if err != nil {
		return fmt.Errorf("CAR header: %w", err)
	}
	var imported int
	for {
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("CAR block %d: %w", imported, err)
		}
		if err := n.blockstore.Put(ctx, blk); err != nil {
			return fmt.Errorf("blockstore put %s: %w", blk.Cid(), err)
		}
		imported++
	}
	if imported == 0 {
		return errors.New("CAR stream contained no blocks")
	}
	return nil
}

// newHTTPClient builds the HTTP client used for the CAR-fetch fallback. The
// timeout is intentionally generous: CAR streams for large papers can take
// tens of seconds; the per-Pin BITSWAP_TIMEOUT bounds the bitswap leg, and
// the ctx passed to each call bounds the overall Pin.
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

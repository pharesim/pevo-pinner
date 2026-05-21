package main

import (
	"context"

	bitswap "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	bsblockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange"
	host "github.com/libp2p/go-libp2p/core/host"
	routing "github.com/libp2p/go-libp2p/core/routing"
)

// newBitswap wires bitswap onto the libp2p host with the DHT as its content
// discovery layer and the local blockstore as its block store. Returns an
// exchange.Interface so callers (the online blockservice in EmbeddedNode)
// treat it generically; the concrete *Bitswap also exposes Close()/Stat()
// when needed.
func newBitswap(ctx context.Context, h host.Host, dht routing.Routing, bs bsblockstore.Blockstore) (exchange.Interface, error) {
	net := bsnet.NewFromIpfsHost(h)
	bsw := bitswap.New(ctx, net, dht, bs)
	return bsw, nil
}

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	crypto "github.com/libp2p/go-libp2p/core/crypto"
	host "github.com/libp2p/go-libp2p/core/host"
	peer "github.com/libp2p/go-libp2p/core/peer"
	routing "github.com/libp2p/go-libp2p/core/routing"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	ma "github.com/multiformats/go-multiaddr"
)

// newLibp2pHost constructs a libp2p host with a persistent peer identity, the
// caller-supplied listen multiaddrs (or libp2p defaults if empty), NAT
// traversal (UPnP/NAT-PMP, AutoNAT, hole punching, relay client), and a
// kad-dht client-mode routing layer. Returns the host, the DHT (as a generic
// routing.Routing for bitswap consumption), and a stop func that closes the
// DHT.
func newLibp2pHost(ctx context.Context, repoDir string, listenAddrs []string, bootstrapAddr string) (host.Host, routing.Routing, func() error, error) {
	priv, err := loadOrCreatePeerKey(filepath.Join(repoDir, "peer.key"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("peer identity: %w", err)
	}

	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.NATPortMap(),
		libp2p.EnableNATService(),
		libp2p.EnableHolePunching(),
		libp2p.EnableRelay(),
	}
	if len(listenAddrs) > 0 {
		opts = append(opts, libp2p.ListenAddrStrings(listenAddrs...))
	}

	// The DHT must be constructed with the host, but libp2p.New wants the
	// Routing option as a constructor. We capture the DHT via the constructor
	// closure so the caller can stop it on shutdown and so bitswap can use it
	// as its provider finder.
	var kadDHT *dht.IpfsDHT
	opts = append(opts, libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
		d, err := dht.New(ctx, h, dht.Mode(dht.ModeClient))
		if err != nil {
			return nil, err
		}
		kadDHT = d
		return d, nil
	}))

	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, nil, nil, err
	}
	if kadDHT == nil {
		_ = h.Close()
		return nil, nil, nil, errors.New("DHT was not constructed by Routing option")
	}

	// Bootstrap dial to PEvO main. Non-blocking: if PEvO main is offline the
	// pinner still starts; the DHT bootstrap below + any future mesh peers
	// can substitute. A 30-second deadline keeps an unreachable bootstrap
	// peer from holding the goroutine and a file descriptor for the kernel's
	// full SYN-retry window (~2 minutes on Linux defaults).
	if bootstrapAddr != "" {
		go func() {
			ai, err := addrInfoFromString(bootstrapAddr)
			if err != nil {
				log.Printf("[ipfs] invalid PEVO_MAIN_LIBP2P_ADDR %q: %v", bootstrapAddr, err)
				return
			}
			dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := h.Connect(dialCtx, *ai); err != nil {
				log.Printf("[ipfs] bootstrap dial failed: %v", err)
				return
			}
			log.Printf("[ipfs] bootstrap connected: %s", ai.ID)
		}()
	}

	// Kick the DHT's own bootstrap (queries hardcoded seed peers; safe to
	// fail silently in test environments without network egress).
	if err := kadDHT.Bootstrap(ctx); err != nil {
		log.Printf("[ipfs] dht bootstrap warning: %v", err)
	}

	return h, kadDHT, kadDHT.Close, nil
}

// loadOrCreatePeerKey reads a persistent Ed25519 identity from path, or
// generates and persists a new one if absent. A persistent identity keeps the
// pinner's peer ID stable across restarts so bootstrap peers / DHT records
// remain useful.
func loadOrCreatePeerKey(path string) (crypto.PrivKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return crypto.UnmarshalPrivateKey(data)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading peer key: %w", err)
	}
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating peer key: %w", err)
	}
	out, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshaling peer key: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("persisting peer key: %w", err)
	}
	return priv, nil
}

// addrInfoFromString parses a /p2p/<id>-tail multiaddr into a peer.AddrInfo.
func addrInfoFromString(s string) (*peer.AddrInfo, error) {
	addr, err := ma.NewMultiaddr(s)
	if err != nil {
		return nil, err
	}
	return peer.AddrInfoFromP2pAddr(addr)
}

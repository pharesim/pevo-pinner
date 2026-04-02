package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Public IPFS gateways used to fetch content when pinning.
var publicGateways = []string{
	"https://ipfs.io",
	"https://dweb.link",
	"https://cloudflare-ipfs.com",
	"https://gateway.pinata.cloud",
}

// EmbeddedNode implements IPFSBackend using local file storage and public gateways.
// It stores pinned content as files on disk and serves them via an HTTP gateway.
type EmbeddedNode struct {
	dataDir     string
	gatewayPort string

	mu      sync.RWMutex
	pins    map[string]bool // CID -> pinned
	pinFile string
	server  *http.Server
	client  *http.Client
}

// NewEmbeddedNode creates an embedded IPFS node with local storage.
func NewEmbeddedNode(dataDir, gatewayPort string) (*EmbeddedNode, error) {
	blocksDir := filepath.Join(dataDir, "blocks")
	if err := os.MkdirAll(blocksDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating blocks dir: %w", err)
	}

	node := &EmbeddedNode{
		dataDir:     dataDir,
		gatewayPort: gatewayPort,
		pins:        make(map[string]bool),
		pinFile:     filepath.Join(dataDir, "pins.json"),
		client: &http.Client{
			Timeout: 2 * time.Minute,
		},
	}

	// Load existing pins (missing file on first run is expected)
	if err := node.loadPins(); err != nil && !os.IsNotExist(err) {
		log.Printf("[ipfs] failed to load pin state: %v", err)
	}

	// Start gateway server
	mux := http.NewServeMux()
	mux.HandleFunc("/ipfs/", node.handleGateway)

	node.server = &http.Server{
		Addr:              ":" + gatewayPort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		ln, err := net.Listen("tcp", node.server.Addr)
		if err != nil {
			log.Printf("[ipfs] gateway listen error: %v", err)
			return
		}
		log.Printf("[ipfs] gateway listening on :%s", gatewayPort)
		if err := node.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[ipfs] gateway error: %v", err)
		}
	}()

	return node, nil
}

func (n *EmbeddedNode) blockPath(cid string) string {
	return filepath.Join(n.dataDir, "blocks", cid)
}

func (n *EmbeddedNode) Pin(ctx context.Context, cid string) error {
	// Check if already pinned and content exists
	path := n.blockPath(cid)
	if _, err := os.Stat(path); err == nil {
		n.mu.Lock()
		n.pins[cid] = true
		n.mu.Unlock()
		n.savePins()
		return nil
	}

	// Fetch from public gateways
	var lastErr error
	for _, gw := range publicGateways {
		url := fmt.Sprintf("%s/ipfs/%s", gw, cid)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}

		resp, err := n.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("gateway %s returned %d", gw, resp.StatusCode)
			continue
		}

		// Write to local storage
		f, err := os.Create(path)
		if err != nil {
			resp.Body.Close()
			return fmt.Errorf("creating block file: %w", err)
		}

		_, err = io.Copy(f, resp.Body)
		resp.Body.Close()
		f.Close()
		if err != nil {
			os.Remove(path)
			lastErr = fmt.Errorf("writing block: %w", err)
			continue
		}

		n.mu.Lock()
		n.pins[cid] = true
		n.mu.Unlock()
		n.savePins()
		log.Printf("[ipfs] pinned %s (fetched from %s)", cid, gw)
		return nil
	}

	return fmt.Errorf("failed to fetch CID %s from any gateway: %w", cid, lastErr)
}

func (n *EmbeddedNode) Unpin(_ context.Context, cid string) error {
	n.mu.Lock()
	delete(n.pins, cid)
	n.mu.Unlock()
	n.savePins()

	path := n.blockPath(cid)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing block: %w", err)
	}
	log.Printf("[ipfs] unpinned %s", cid)
	return nil
}

func (n *EmbeddedNode) IsPinned(_ context.Context, cid string) (bool, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.pins[cid], nil
}

func (n *EmbeddedNode) PinnedCIDs(_ context.Context) ([]string, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	cids := make([]string, 0, len(n.pins))
	for cid := range n.pins {
		cids = append(cids, cid)
	}
	return cids, nil
}

func (n *EmbeddedNode) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return n.server.Shutdown(ctx)
}

func (n *EmbeddedNode) handleGateway(w http.ResponseWriter, r *http.Request) {
	// Extract CID from /ipfs/<cid>
	cid := r.URL.Path[len("/ipfs/"):]
	if cid == "" {
		http.Error(w, "missing CID", http.StatusBadRequest)
		return
	}
	if err := ValidateCID(cid); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	path := n.blockPath(cid)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.ServeContent(w, r, cid, stat.ModTime(), f)
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
	for _, cid := range cids {
		n.pins[cid] = true
	}
	return nil
}

func (n *EmbeddedNode) savePins() {
	n.mu.RLock()
	cids := make([]string, 0, len(n.pins))
	for cid := range n.pins {
		cids = append(cids, cid)
	}
	n.mu.RUnlock()

	data, err := json.MarshalIndent(cids, "", "  ")
	if err != nil {
		log.Printf("[ipfs] failed to marshal pins: %v", err)
		return
	}
	if err := os.WriteFile(n.pinFile, data, 0o644); err != nil {
		log.Printf("[ipfs] failed to save pins: %v", err)
	}
}

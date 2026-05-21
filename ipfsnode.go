package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	cid "github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// ErrPinnerShuttingDown is returned by Pin once Drain or Close has been
// invoked. Callers should treat it as a permanent rejection for the current
// process lifetime, not a retryable error.
var ErrPinnerShuttingDown = errors.New("pinner: shutting down")

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
	maxPinBytes int64

	mu      sync.RWMutex
	pins    map[string]bool // CID -> pinned
	pinFile string
	server  *http.Server
	client  *http.Client

	// Drain coordination. drainMu serializes the gate check + WaitGroup.Add
	// in Pin with the channel close + WaitGroup.Wait in Drain, so a fresh Pin
	// cannot race a concurrent Wait (Go's race detector treats Add with a
	// positive delta as racy against a concurrent Wait when the counter is
	// zero). done is closed exactly once via doneOnce; inFlight tracks Pin
	// calls so Drain can block until they finish. cancels holds a CancelFunc
	// per in-flight Pin so Drain can force-cancel them when its deadline
	// expires; without this, the caller's long-lived ctx keeps io.Copy
	// blocked on the underlying gateway read past the drain budget and the
	// per-Pin tmp-file cleanup defer never runs.
	drainMu   sync.Mutex
	done      chan struct{}
	doneOnce  sync.Once
	inFlight  sync.WaitGroup
	cancels   map[uint64]context.CancelFunc
	nextPinID uint64
}

// NewEmbeddedNode creates an embedded IPFS node with local storage.
// maxPinBytes caps how much can be copied from any single gateway response.
func NewEmbeddedNode(dataDir, gatewayPort string, maxPinBytes int64) (*EmbeddedNode, error) {
	blocksDir := filepath.Join(dataDir, "blocks")
	if err := os.MkdirAll(blocksDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating blocks dir: %w", err)
	}

	node := &EmbeddedNode{
		dataDir:     dataDir,
		gatewayPort: gatewayPort,
		maxPinBytes: maxPinBytes,
		pins:        make(map[string]bool),
		pinFile:     filepath.Join(dataDir, "pins.json"),
		client: &http.Client{
			Timeout: 2 * time.Minute,
		},
		done:    make(chan struct{}),
		cancels: make(map[uint64]context.CancelFunc),
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

func (n *EmbeddedNode) Pin(ctx context.Context, cidStr string) error {
	// Gate check + WaitGroup.Add + per-Pin cancel registration under
	// drainMu so Drain's signal-and-Wait sequence sees a stable counter
	// and a complete cancels map. Once Drain has closed done, every
	// subsequent Pin returns here without Add'ing, leaving Wait free to
	// observe only the strictly-pre-Drain in-flight set. pinCtx wraps the
	// caller ctx so Drain can force-cancel mid-io.Copy on hard-deadline
	// expiry; without this, the caller's long-lived ctx keeps the gateway
	// read blocked past the drain budget and the tmp-file cleanup never
	// runs, allowing a partial file to be promoted at next startup.
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

	// Parse the CID once so each gateway attempt can hash-verify against the
	// requested multihash. A malformed multihash here is a programmer error
	// (ValidateCID's regex passed), so we surface it loudly instead of
	// silently writing unverified bytes.
	c, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("decoding CID %s: %w", cidStr, err)
	}
	expected, err := mh.Decode(c.Hash())
	if err != nil {
		return fmt.Errorf("decoding multihash for %s: %w", cidStr, err)
	}

	// Already pinned: the canonical block path only exists after a prior
	// Pin completed the atomic rename below, so a file here is fully
	// written by construction. Legacy files predating the atomic-write
	// invariant are trusted on a best-effort basis (single-binary
	// HTTP-cache mode does not hash-verify reassembled UnixFS bytes
	// against the CID's multihash digest — trustless verification is the
	// boxo rewrite's contract).
	path := n.blockPath(cidStr)
	if _, err := os.Stat(path); err == nil {
		n.mu.Lock()
		n.pins[cidStr] = true
		n.mu.Unlock()
		n.savePins()
		return nil
	}

	// Atomic block write: stream into <cid>.tmp, fsync, then rename to
	// canonical path. A partial copy (network failure, drain force-cancel,
	// kill -9) only ever lives at the tmp path and is removed on the
	// error branch; the canonical path is reachable only via the rename,
	// which runs only after io.Copy + hash-verify both succeed.
	tmpPath := path + ".tmp"

	// Fetch from public gateways
	var lastErr error
	for _, gw := range publicGateways {
		url := fmt.Sprintf("%s/ipfs/%s", gw, cidStr)
		req, err := http.NewRequestWithContext(pinCtx, http.MethodGet, url, nil)
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

		// Fresh hasher per gateway attempt: a previous gateway's partial
		// bytes must not bleed into this iteration's digest.
		hasher, err := mh.GetHasher(expected.Code)
		if err != nil {
			resp.Body.Close()
			return fmt.Errorf("unsupported multihash code %d for %s: %w", expected.Code, cidStr, err)
		}

		f, err := os.Create(tmpPath)
		if err != nil {
			resp.Body.Close()
			return fmt.Errorf("creating block tmp file: %w", err)
		}

		// Layering: LimitedReader bounds the byte count first (so a hostile
		// gateway streaming gigabytes does not exhaust memory through the
		// hasher); TeeReader fans the bounded stream into the hasher; Copy
		// writes the tmp file from the tee.
		lr := &io.LimitedReader{R: resp.Body, N: n.maxPinBytes + 1}
		tee := io.TeeReader(lr, hasher)
		_, copyErr := io.Copy(f, tee)
		syncErr := f.Sync()
		resp.Body.Close()
		closeErr := f.Close()
		if copyErr != nil {
			_ = os.Remove(tmpPath)
			lastErr = fmt.Errorf("writing block: %w", copyErr)
			continue
		}
		if syncErr != nil {
			_ = os.Remove(tmpPath)
			lastErr = fmt.Errorf("syncing block: %w", syncErr)
			continue
		}
		if closeErr != nil {
			_ = os.Remove(tmpPath)
			lastErr = fmt.Errorf("closing block: %w", closeErr)
			continue
		}
		if lr.N == 0 {
			_ = os.Remove(tmpPath)
			lastErr = fmt.Errorf("gateway %s response exceeded size cap of %d bytes", gw, n.maxPinBytes)
			continue
		}

		actual := hasher.Sum(nil)
		if !bytes.Equal(actual, expected.Digest) {
			_ = os.Remove(tmpPath)
			lastErr = fmt.Errorf("gateway %s content hash mismatch for %s: expected %x, got %x", gw, cidStr, expected.Digest, actual)
			log.Printf("[ipfs] %v", lastErr)
			continue
		}

		if err := os.Rename(tmpPath, path); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("promoting block to canonical path: %w", err)
		}

		n.mu.Lock()
		n.pins[cidStr] = true
		n.mu.Unlock()
		n.savePins()
		log.Printf("[ipfs] pinned %s (fetched from %s)", cidStr, gw)
		return nil
	}

	return fmt.Errorf("failed to fetch CID %s from any gateway: %w", cidStr, lastErr)
}

func (n *EmbeddedNode) Unpin(_ context.Context, cid string) error {
	if err := ValidateCID(cid); err != nil {
		return err
	}
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
	if err := ValidateCID(cid); err != nil {
		return false, err
	}
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

// Drain signals shutdown and blocks until every in-flight Pin returns or ctx
// expires. After Drain has been entered, new Pin calls return
// ErrPinnerShuttingDown immediately. On hard-deadline expiry Drain
// force-cancels every in-flight Pin's child ctx so io.Copy unwinds and the
// per-Pin tmp-file cleanup runs before the process exits; a brief grace
// window after force-cancel lets those deferred cleanups complete. Safe to
// call concurrently and more than once; subsequent calls observe the same
// closed channel.
func (n *EmbeddedNode) Drain(ctx context.Context) error {
	// Close done under drainMu so any concurrent Pin's gate check sees a
	// consistent state; once we drop the lock and start Wait, no fresh Add
	// can sneak through.
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
		// Hard deadline: force-cancel every in-flight Pin so its
		// io.Copy unwinds and the tmp-file cleanup defer runs. Then
		// wait briefly for cleanups to complete; this is bounded so a
		// truly stuck goroutine cannot wedge shutdown indefinitely.
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
			log.Printf("[ipfs] post-cancel cleanup grace expired; some tmp files may remain")
		}
		return ctx.Err()
	}
}

func (n *EmbeddedNode) Close() error {
	// Close still signals done so leaked in-flight pins observe shutdown the
	// next time they reach the inner done-check. Drain remains the
	// synchronous wait; Close on its own only tears down the gateway server.
	n.signalDone()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return n.server.Shutdown(ctx)
}

func (n *EmbeddedNode) signalDone() {
	n.doneOnce.Do(func() {
		close(n.done)
		log.Printf("[ipfs] pin acceptance stopped")
	})
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
	if err := atomicWriteFile(n.pinFile, data, 0o644); err != nil {
		log.Printf("[ipfs] failed to save pins: %v", err)
	}
}

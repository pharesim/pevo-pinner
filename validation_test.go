package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Traversal payloads that a hostile Hive post might place in json_metadata.
// `ValidateCID` is the gate the discovery filter and every backend entry rely on;
// when the gate holds, none of these strings reach `filepath.Join` or any URL.
var traversalPayloads = []string{
	"../../../etc/passwd",
	"..\\..\\windows-style",
	"/absolute/path",
	"Qm" + strings.Repeat("/", 44),
	"",
	"not-a-cid",
}

func TestValidateCIDRejectsTraversalAndJunk(t *testing.T) {
	for _, p := range traversalPayloads {
		if err := ValidateCID(p); err == nil {
			t.Errorf("ValidateCID(%q) = nil, want error", p)
		}
	}
}

func TestEmbeddedNodeRejectsTraversalCID(t *testing.T) {
	tmp := t.TempDir()
	// gatewayPort "0" tells the OS to assign an ephemeral port. Every CID
	// under test is rejected at ValidateCID before any block fetch happens.
	node, err := NewEmbeddedNode(IPFSNodeOptions{
		DataDir:      tmp,
		GatewayPort:  "0",
		Libp2pListen: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	if err != nil {
		t.Fatalf("NewEmbeddedNode: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	ctx := context.Background()
	for _, bad := range traversalPayloads {
		item := DiscoveredItem{CID: bad, CIDType: "supplementary"}

		if err := node.Pin(ctx, item.CID); err == nil {
			t.Errorf("Pin(%q) = nil, want error", bad)
		}
		if err := node.Unpin(ctx, item.CID); err == nil {
			t.Errorf("Unpin(%q) = nil, want error", bad)
		}
		if _, err := node.IsPinned(ctx, item.CID); err == nil {
			t.Errorf("IsPinned(%q) = nil, want error", bad)
		}
	}

	// boxo's flatfs creates a sharded blocks tree under ipfs-repo/blocks.
	// Walk the whole repo dir and assert nothing CID-shaped landed: every
	// failing input was rejected at ValidateCID before any block touched
	// disk.
	repoDir := filepath.Join(tmp, "ipfs-repo")
	var stray []string
	_ = filepath.Walk(repoDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		// Block files use boxo's base32-ish naming; CID-shaped or path-shaped
		// traversal payload bytes would not appear here.
		for _, p := range traversalPayloads {
			if p != "" && strings.Contains(path, p) {
				stray = append(stray, path)
			}
		}
		return nil
	})
	if len(stray) != 0 {
		t.Errorf("traversal payload(s) landed under repo dir: %v", stray)
	}
}

func TestPinataBackendRejectsTraversalCID(t *testing.T) {
	// No network is reachable in tests; ValidateCID is the first line of each
	// method, so rejection happens before any HTTP request is constructed.
	p := NewPinataBackend("k", "s")

	ctx := context.Background()
	for _, bad := range traversalPayloads {
		if err := p.Pin(ctx, bad); err == nil {
			t.Errorf("Pin(%q) = nil, want error", bad)
		}
		if err := p.Unpin(ctx, bad); err == nil {
			t.Errorf("Unpin(%q) = nil, want error", bad)
		}
		if _, err := p.IsPinned(ctx, bad); err == nil {
			t.Errorf("IsPinned(%q) = nil, want error", bad)
		}
	}
}

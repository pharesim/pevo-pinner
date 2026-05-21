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
	// gatewayPort "0" tells the OS to assign an ephemeral port.
	node, err := NewEmbeddedNode(tmp, "0")
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

	// No file should have escaped the blocks dir, and no traversal target
	// should have landed under tmp.
	blocks := filepath.Join(tmp, "blocks")
	entries, err := os.ReadDir(blocks)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", blocks, err)
	}
	if len(entries) != 0 {
		t.Errorf("blocks dir is non-empty after rejected pins: %v", entries)
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

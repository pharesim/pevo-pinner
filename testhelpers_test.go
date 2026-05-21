package main

import (
	"bytes"
	"context"
	"testing"

	blocks "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	carv2 "github.com/ipld/go-car/v2"
	carstorage "github.com/ipld/go-car/v2/storage"
)

// buildCARv1 produces a CARv1 byte stream with the given root and blocks.
// Each entry is written verbatim — key and data are decoupled so callers
// can intentionally produce hash-mismatched sections to exercise the
// trustless verification path on the reader side.
func buildCARv1(t *testing.T, root cid.Cid, entries []carEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := carstorage.NewWritable(&buf, []cid.Cid{root}, carv2.WriteAsCarV1(true))
	if err != nil {
		t.Fatalf("NewWritable: %v", err)
	}
	ctx := context.Background()
	for _, e := range entries {
		if err := w.Put(ctx, e.cid.KeyString(), e.data); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := w.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return buf.Bytes()
}

// carEntry is a single section in a hand-built CAR. cid is the CID written
// in the section header; data is the bytes written as the section payload.
// Hash-verification on read requires cid == hash(data); use a deliberate
// mismatch to test trustless rejection.
type carEntry struct {
	cid  cid.Cid
	data []byte
}

// newRawBlock constructs a raw-codec block from the given bytes. The block's
// CID is derived from the bytes via boxo's default block constructor, so
// hash-verification passes when the same block is fed back through any CAR
// reader.
func newRawBlock(t *testing.T, data []byte) blocks.Block {
	t.Helper()
	return blocks.NewBlock(data)
}

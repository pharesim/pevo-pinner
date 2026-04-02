package main

import (
	"context"
	"fmt"
	"regexp"
)

// cidRegex matches valid IPFS CIDv0 (Qm...) and CIDv1 (b...) strings.
var cidRegex = regexp.MustCompile(`^(Qm[1-9A-HJ-NP-Za-km-z]{44}|b[A-Za-z2-7]{58})$`)

// ValidateCID returns an error if the CID string is not a valid IPFS CID.
func ValidateCID(cid string) error {
	if !cidRegex.MatchString(cid) {
		return fmt.Errorf("invalid CID: %q", cid)
	}
	return nil
}

// IPFSBackend defines the interface for IPFS pin management.
// Implementations: EmbeddedNode (local blockstore + gateway) and PinataBackend (Pinata API).
type IPFSBackend interface {
	// Pin fetches and stores content for the given CID.
	Pin(ctx context.Context, cid string) error

	// Unpin removes a CID from the pinned set.
	Unpin(ctx context.Context, cid string) error

	// IsPinned checks whether a CID is currently pinned.
	IsPinned(ctx context.Context, cid string) (bool, error)

	// PinnedCIDs returns all currently pinned CIDs.
	PinnedCIDs(ctx context.Context) ([]string, error)

	// Close shuts down the backend and releases resources.
	Close() error
}

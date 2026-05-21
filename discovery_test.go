package main

import (
	"strings"
	"testing"
	"time"
)

// Hostile payloads that a Hive author could place in `json_metadata.ipfs_cid`
// or in a `supplementary_files[].cid` entry. Exercises the validator-gated
// path-traversal defense at the discovery layer for the paper and
// supplementary call sites of `extractItemsFromRow`. Mirrors the payload set
// in `validation_test.go`, extended with a NUL byte and a mixed-case
// malformed CID.
var discoveryHostilePayloads = []string{
	"../../../etc/passwd",
	"..\\..\\windows-style",
	"/absolute/path",
	"Qm" + strings.Repeat("/", 44),
	"not-a-cid",
	"\x00",
	"Qm" + strings.Repeat("a", 43) + "/",
	// Right length, right Qm prefix, but contains characters outside the
	// CIDv0 base58btc alphabet (0, O, I, l) so the validator's regex rejects.
	"Qm0OIl" + strings.Repeat("a", 40),
}

// A valid CIDv0, used to confirm the filter accepts well-formed CIDs alongside
// hostile siblings without aborting the per-row loop.
const validCIDv0 = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"

// TestExtractItemsFromRow_PaperCID_DropsHostile exercises the paper CID call
// site of `extractItemsFromRow`. Deleting `ValidateCID(row.IpfsCID)` from the
// paper branch causes every hostile payload to be admitted as a DiscoveredItem
// with the literal traversal string in `CID` — every subtest then fails.
func TestExtractItemsFromRow_PaperCID_DropsHostile(t *testing.T) {
	for _, bad := range discoveryHostilePayloads {
		t.Run(safeName(bad), func(t *testing.T) {
			row := discoveryRow{
				Author:   "alice",
				Permlink: "paper-1",
				Title:    "T",
				IpfsCID:  bad,
				Created:  time.Unix(0, 0),
			}
			seen := make(map[string]bool)
			items, dropped := extractItemsFromRow(row, seen)

			if dropped != 1 {
				t.Errorf("dropped = %d, want 1", dropped)
			}
			if len(items) != 0 {
				t.Errorf("items = %v, want empty (hostile CID must be filtered)", items)
			}
			if seen[bad] {
				t.Errorf("hostile CID %q marked seen", bad)
			}
		})
	}
}

// TestExtractItemsFromRow_PaperCID_EmptyIsNotAnError confirms that a NULL/empty
// paper CID (the SQL representation of a missing `ipfs_cid` column) is filtered
// by the `!= ""` guard upstream of ValidateCID — the drop counter must not
// increment, because empty is "absent" not "hostile".
func TestExtractItemsFromRow_PaperCID_EmptyIsNotAnError(t *testing.T) {
	row := discoveryRow{
		Author:   "alice",
		Permlink: "paper-1",
		IpfsCID:  "",
		Created:  time.Unix(0, 0),
	}
	items, dropped := extractItemsFromRow(row, make(map[string]bool))
	if dropped != 0 {
		t.Errorf("empty IpfsCID: dropped = %d, want 0", dropped)
	}
	if len(items) != 0 {
		t.Errorf("empty IpfsCID: items = %v, want empty", items)
	}
}

// TestExtractItemsFromRow_SupplementaryCID_DropsHostile exercises the
// supplementary CID call site. Each row carries one hostile sibling and one
// valid sibling so the test also pins the "loop continues past a rejected
// entry" invariant. Deleting `ValidateCID(sf.CID)` from the supplementary
// branch causes the hostile sibling to be admitted alongside the valid one;
// `dropped` then drops to 0 and `len(items)` rises to 2.
func TestExtractItemsFromRow_SupplementaryCID_DropsHostile(t *testing.T) {
	for _, bad := range discoveryHostilePayloads {
		t.Run(safeName(bad), func(t *testing.T) {
			suppJSON := `[{"cid":` + jsonString(bad) + `,"filename":"data.csv"},` +
				`{"cid":"` + validCIDv0 + `","filename":"ok.csv"}]`
			row := discoveryRow{
				Author:   "alice",
				Permlink: "paper-2",
				SuppJSON: suppJSON,
				Created:  time.Unix(0, 0),
			}
			seen := make(map[string]bool)
			items, dropped := extractItemsFromRow(row, seen)

			if dropped != 1 {
				t.Errorf("dropped = %d, want 1", dropped)
			}
			if len(items) != 1 {
				t.Fatalf("got %d items, want exactly the one valid sibling", len(items))
			}
			if items[0].CID != validCIDv0 {
				t.Errorf("kept item CID = %q, want %q", items[0].CID, validCIDv0)
			}
			if items[0].CIDType != "supplementary" {
				t.Errorf("kept item CIDType = %q, want supplementary", items[0].CIDType)
			}
		})
	}
}

// TestExtractItemsFromRow_SupplementaryCID_EmptySkipped pins the upstream
// `sf.CID == ""` guard: an empty supplementary CID is skipped silently, not
// counted as a drop.
func TestExtractItemsFromRow_SupplementaryCID_EmptySkipped(t *testing.T) {
	suppJSON := `[{"cid":"","filename":"empty.csv"},` +
		`{"cid":"` + validCIDv0 + `","filename":"ok.csv"}]`
	row := discoveryRow{
		Author:   "alice",
		Permlink: "paper-2",
		SuppJSON: suppJSON,
		Created:  time.Unix(0, 0),
	}
	items, dropped := extractItemsFromRow(row, make(map[string]bool))
	if dropped != 0 {
		t.Errorf("empty supplementary CID: dropped = %d, want 0", dropped)
	}
	if len(items) != 1 || items[0].CID != validCIDv0 {
		t.Errorf("items = %v, want exactly the valid sibling", items)
	}
}

// TestExtractItemsFromRow_InlineImage_BodyRegexFiltersHostile exercises the
// inline-image call site. The body-scanner regex `ipfsCIDRegex` only captures
// well-formed CIDs inside `/ipfs/<cid>/` segments, so direct hostile payloads
// (traversal strings, NUL bytes, malformed CIDs) never reach the captured
// group. The branch's `ValidateCID` call is defense-in-depth against a future
// `ipfsCIDRegex` that is laxer than `cidRegex`. This test asserts (a) hostile
// payloads embedded in a `/ipfs/...` shape are not captured, and (b) when a
// valid CID is present in the body, it lands as an `inline_image` item.
func TestExtractItemsFromRow_InlineImage_BodyRegexFiltersHostile(t *testing.T) {
	for _, bad := range discoveryHostilePayloads {
		t.Run(safeName(bad), func(t *testing.T) {
			// Wrap the hostile payload in the `/ipfs/<x>/` shape the body
			// scanner is looking for. The body regex's anchored character
			// class + length will reject the hostile shape before capture.
			body := "see /ipfs/" + bad + "/ and /ipfs/" + validCIDv0 + "/ here"
			row := discoveryRow{
				Author:   "alice",
				Permlink: "paper-3",
				Body:     body,
				Created:  time.Unix(0, 0),
			}
			seen := make(map[string]bool)
			items, dropped := extractItemsFromRow(row, seen)

			// The valid sibling must still be captured.
			if len(items) != 1 {
				t.Fatalf("got %d items, want exactly the valid embedded CID", len(items))
			}
			if items[0].CID != validCIDv0 {
				t.Errorf("kept item CID = %q, want %q", items[0].CID, validCIDv0)
			}
			if items[0].CIDType != "inline_image" {
				t.Errorf("kept item CIDType = %q, want inline_image", items[0].CIDType)
			}
			// Body regex pre-filters hostile shapes; `ValidateCID` does not
			// fire on this branch with hostile payloads, so `dropped` stays 0.
			if dropped != 0 {
				t.Errorf("dropped = %d, want 0 (body regex pre-filters)", dropped)
			}
		})
	}
}

// TestExtractItemsFromRow_InlineImage_DedupsRepeats pins the dedup invariant
// on the inline-image branch: a body containing the same `/ipfs/<cid>/` shape
// twice yields exactly one DiscoveredItem and marks `seen` once.
func TestExtractItemsFromRow_InlineImage_DedupsRepeats(t *testing.T) {
	body := "/ipfs/" + validCIDv0 + "/ and again /ipfs/" + validCIDv0 + "/"
	row := discoveryRow{
		Author:   "alice",
		Permlink: "paper-4",
		Body:     body,
		Created:  time.Unix(0, 0),
	}
	seen := make(map[string]bool)
	items, dropped := extractItemsFromRow(row, seen)

	if len(items) != 1 {
		t.Errorf("dedupe failed: got %d items, want 1", len(items))
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if !seen[validCIDv0] {
		t.Errorf("valid inline-image CID not marked seen")
	}
}

// TestExtractItemsFromRow_CrossBranchHostile pins the integration assertion:
// one row that places a hostile payload in both the paper-CID and supplementary
// branches reports `dropped == 2` and yields zero items. This is the smoke
// test that fails if either of the two non-defense-in-depth `ValidateCID`
// call sites in `extractItemsFromRow` is removed.
func TestExtractItemsFromRow_CrossBranchHostile(t *testing.T) {
	row := discoveryRow{
		Author:   "alice",
		Permlink: "paper-5",
		IpfsCID:  "../../../etc/passwd",
		SuppJSON: `[{"cid":"..\\..\\windows-only","filename":"x"}]`,
		Created:  time.Unix(0, 0),
	}
	items, dropped := extractItemsFromRow(row, make(map[string]bool))

	if dropped != 2 {
		t.Errorf("dropped = %d, want 2 (paper + supplementary hostile)", dropped)
	}
	if len(items) != 0 {
		t.Errorf("items = %v, want empty (all hostile)", items)
	}
}

// safeName produces a t.Run-safe subtest name from an arbitrary string. Spaces,
// slashes, and control characters are replaced so subtest selectors and test
// output stay readable.
func safeName(s string) string {
	if s == "" {
		return "empty"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ' || r == '/' || r == '\\' || r == ':' || r == '"':
			b.WriteRune('_')
		case r < 0x20 || r == 0x7f:
			b.WriteString("ctrl")
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "empty"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// jsonString minimally escapes a string for embedding as a JSON literal. Used
// to construct supplementary_files JSON payloads that contain backslashes,
// control characters, or quotes without dragging in a full `json.Marshal`.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[(r>>4)&0xf])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

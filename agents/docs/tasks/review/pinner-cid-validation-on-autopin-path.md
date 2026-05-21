# PINNER-CID-VALIDATION-ON-AUTOPIN-PATH — ValidateCID on the discovery → backend.Pin path

**Owner:** pinner
**Created:** 2026-05-21 (surfaced by full-codebase audit 2026-04-21, `.context/audit-2026-04-21/chunk-6-correctness-reviewer.md`)
**Priority:** P0 (partial — HTTP entry already validates; autopin path does not)

## Context

The pinner has two CID entry points:

1. **HTTP API** (`/api/pin/<cid>`, `/api/unpin/<cid>`) — `pinner/server.go` calls `ValidateCID` before forwarding to the backend.
2. **Autopin from Hive discovery** — `pinner/discovery.go` extracts CIDs from `json_metadata->'ipfs_cid'` and `supplementary_files[].cid`, then `pinner/main.go` (or the autopin callback) hands them directly to `backend.Pin(ctx, cid)` without `ValidateCID`.

Inside `EmbeddedNode.Pin`, `filepath.Join(n.dataDir, "blocks", cid)` is called against the unvalidated CID. A malicious Hive post can set `supplementary_files[].cid = "../../../etc/cron.d/evil"` or `"..\\..\\windows-only-but-still"`. The path-traversal write happens under the pinner UID.

The HTTP-entry validation is reachable for hostile callers only if they can reach the pinner's API port (typically firewalled). The autopin path is reachable by **any Hive author** with a single broadcast.

## Goal

Add `ValidateCID` defensively at two layers:

1. **Discovery filter.** In `pinner/discovery.go`, validate each CID extracted from `ipfs_cid` and `supplementary_files[].cid` before yielding a `DiscoveredItem`. Invalid CIDs are logged and dropped.
2. **Backend entry guard.** In every `Backend.Pin` / `Backend.Unpin` / `Backend.IsPinned` implementation (`EmbeddedNode`, `PinataBackend`), call `ValidateCID` as the first line. Belt-and-suspenders against future code paths that bypass discovery.

## Non-goals

- Strict v1-only enforcement. Accept v0 and v1 CIDs equally; `ValidateCID` already handles both.
- Stricter URL-segment guards on the gateway-serving path (`/ipfs/<cid>`) — handled separately by HTTP-mux routing.

## Acceptance

- `pinner/discovery.go` rejects invalid CIDs from `ipfs_cid` and each entry of `supplementary_files[]`, with a counter or log line per drop.
- `EmbeddedNode.Pin`, `EmbeddedNode.Unpin`, `EmbeddedNode.IsPinned`, and the Pinata equivalents validate the CID at entry and return an error on invalid input.
- A test in `pinner/` constructs a `DiscoveredItem` with `cid = "../../../etc/passwd"` and asserts both the discovery filter drops it and the backend would reject it if it slipped through.

## References

- Audit chunk: `.context/audit-2026-04-21/chunk-6-correctness-reviewer.md` (P0: CID path traversal from HAF).

## Architect re-review (2026-05-21) — HELD PENDING FIXES:

- **Discovery filter `ValidateCID` integration is untested.** Acceptance bullet 1 in this task is unmet at the test-suite level: deleting any of the three `ValidateCID(...)` calls in `pinner/discovery.go` `refresh()` (paper CID path, supplementary CIDs path, inline-image CIDs path) leaves all tests green. `validation_test.go` covers the validator function in isolation and the backend entry points; the discovery-level integration is dark to the suite. Add table-driven tests that exercise the three call sites with traversal/junk payloads (`../../../etc/passwd`, empty string, NUL byte, mixed-case malformed CID) and assert the `dropped` counter increments and the item is excluded from the `DiscoveredItem` slice yielded to autopin. Prefer extracting a per-row helper that can be driven in memory over stubbing `sql.Rows`.
- **Pinata backend builds URL path/query from CID without escaping.** `pinner/pinata.go` `Unpin` and `IsPinned` interpolate the CID into the URL via `fmt.Sprintf`. The `ValidateCID` regex this task adds is conservative enough that no dangerous characters can reach the URL builder today, so this is defense-in-depth, not an active bypass. Add `url.PathEscape(cid)` for path segments and `url.QueryEscape(cid)` for query parameters so the regex narrowing stops being load-bearing for URL safety. No new test required — existing Pinata tests should still pass; the change is purely additive at the URL-construction layer.

Pinner re-review signal (2026-05-21, working tree):

- **Discovery filter ValidateCID integration.** Extracted `extractItemsFromRow(discoveryRow, seen)` from `refresh()` in `pinner/discovery.go` so the per-row paper / supplementary / inline-image CID-extraction logic can be exercised in memory without stubbing `sql.Rows`. Added `pinner/discovery_test.go` with table-driven tests covering each call site against the architect's payload set (traversal strings, empty, NUL byte, mixed-case malformed CID): paper branch asserts `dropped == 1` and the hostile CID is excluded from items per payload; supplementary branch asserts the loop continues past a rejected sibling and keeps the valid one; inline-image branch documents that the body regex pre-filters hostile shapes (the `ValidateCID` call there is defense-in-depth and cannot fire as a drop path while `ipfsCIDRegex` and `cidRegex` share character classes). A cross-branch test pins `dropped == 2` for a row hostile in both paper and supplementary, the mutation-witness that fails if either call site is removed.
- **Pinata URL escaping.** Wrapped the CID in `url.PathEscape` for the `Unpin` DELETE path and in `url.QueryEscape` for the `IsPinned` query value in `pinner/pinata.go`. Existing `TestPinataBackendRejectsTraversalCID` continues to pass because rejection happens at the `ValidateCID` gate before URL construction.

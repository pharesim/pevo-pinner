# PINNER-SAVEPINS-DEBOUNCE — Reduce O(N) pins.json marshal cost on autopin batches

**Owner:** pinner
**Created:** 2026-05-21

## Context

`savePins` serializes the entire in-memory pin map and atomic-renames `pins.json` on every successful `markPinned` call. The atomic-write itself is correct (the security model requires it). The cost is the marshal + IO multiplied by autopin batch size.

In production:

- `AUTOPIN_CONCURRENCY=4` × per-batch CIDs ≤ `AUTOPIN_AUTHOR_CAP=20` per author × N authors per discovery batch.
- A single discovery batch of 80 successful pins on a steady-state 1000-CID pinset = 80 marshal calls, each serializing ~1080 entries = ~86k JSON entries serialized during one batch.

Performance review flagged this as P1. Correctness review independently flagged the read-then-write window (savePins snapshots under RLock then writes after release) as a P3 — under concurrent `markPinned` calls the disk-state-after can reflect an older snapshot than what's in memory.

## Goal

Cut the steady-state cost without breaking the crash-recovery contract or the atomic-write invariant.

The brainstorm decision (to be made by the implementer via `/ce-brainstorm`) should consider three approaches:

1. **Time-based debounce.** Dirty-flag + ticker (e.g., 1s). markPinned sets dirty; a background goroutine flushes at most once per interval. Pros: simple, bounded cost. Cons: opens a ~1s crash window where a successful Pin is not on disk yet; need to flush on Drain so the window doesn't extend to shutdown.

2. **Batch-boundary flush.** Move savePins out of markPinned. autopin_runner.Run flushes once at the end of each batch. Pros: zero per-pin cost, no time-window crash gap. Cons: requires plumbing a "flush" call back through the IPFSBackend interface or via a callback; markPinned no longer the natural call site.

3. **Append-only log + periodic compaction.** Write a one-line append to `pins.log` per Pin (cheap), compact to `pins.json` on a slower cadence. Pros: trivially bounded per-pin cost. Cons: log format / compaction adds surface; recovery on startup must merge log + json.

Recommended starting point: **(1) time-based debounce with flush-on-Drain**. Simplest, smallest surface, the crash window is acceptable for a stateless re-pin (the next discovery batch re-discovers the lost CID and Pin is idempotent at the blockstore level). Confirm via `/ce-brainstorm` if a stronger guarantee is desired.

## Non-goals

- Migrating to boxo's pinset. Tracked separately; the in-memory map is the contract today.
- Eliminating savePins entirely. The persistence contract is required.

## Acceptance

- Steady-state autopin of an 80-CID batch on a 1000-pin set produces ≤ 2 `pins.json` writes (debounce flush + final batch-end flush), not 80.
- A crash mid-batch loses at most the last debounce-interval worth of pins; the next startup recovers via re-discovery.
- `Drain` waits for or triggers a flush so a graceful shutdown never strands new pins.
- A stress test demonstrates the bound: spawn N concurrent `markPinned` calls and assert the resulting write count via a hook or atomic counter.

## References

- Boxo review performance finding #10 (savePins serialises full pin map on every successful pin).
- Boxo review correctness finding (savePins read-then-write window can lose concurrent markPinned writes).
- Atomic-write contract: see `atomicWriteFile` and CLAUDE.md security considerations.

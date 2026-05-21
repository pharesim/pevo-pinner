# PINNER-DRAIN-TIMEOUT-PARTIAL-BLOCK-TRUST — Prevent partial block files from being trusted as pinned on next startup

**Owner:** pinner
**Created:** 2026-05-21 (surfaced by architect review of pinner-shutdown-drain task)
**Priority:** P0 (data integrity)

## Context

The drain mechanism added by the prior pinner-shutdown work has a hard deadline. When that deadline expires, in-flight Pin goroutines are abandoned: the process moves on to `backend.Close` and exits, and the OS reaps the goroutines mid-`io.Copy` without running their deferred cleanup paths.

Three interacting defects make this a data-integrity issue rather than a "leaked goroutine" issue:

1. **Pin's `ctx` is the long-lived main ctx, not a drain-cancellable child.** The autopin closure in `main.go` captures the outer `ctx` from the closure scope and passes it to `backend.Pin(ctx, cid)`. When `Drain`'s deadline fires, the main ctx is still alive — `io.Copy` does NOT receive ctx cancellation. It keeps reading until `http.Client.Timeout` (2 minutes) or until the process exits. The existing `os.Remove(path)` cleanup on the `io.Copy`-error path therefore never fires for the leaked goroutine.

2. **Partial block files persist at `blocks/<cid>` across restart.** Whatever bytes the abandoned `io.Copy` had written are still on disk when the next process starts.

3. **The `os.Stat` short-circuit in `Pin` trusts any existing file at `blockPath(cid)` without hash verification.** On next startup, when the autopin callback or an explicit Pin call hits the same CID, `Pin` sees the file at `blockPath`, sets `pins[cidStr] = true`, calls `savePins()`, and returns success. The corrupt partial file is now promoted to "fully pinned" silently. Gateway-served content is wrong. Downstream consumers that hash-verify get a mismatch and may discard the post.

There is also a narrower variant on the persistent-state side: `savePins` is not atomic. A leaked goroutine past the drain deadline that reaches `savePins` mid-process-exit can write a partial `pins.json`, which the next `loadPins` then fails to parse.

## Goal

Close the integrity loop end-to-end so that no leaked goroutine, no abandoned drain, and no partial file can promote corrupt content across a restart.

1. **Drain-cancellable Pin ctx.** When `Drain`'s deadline expires, in-flight `Pin` ctxs must be cancelled so `io.Copy` unwinds within the drain budget. The existing `os.Remove` cleanup path then runs naturally. Either thread a backend-managed child ctx through `Pin` (combining caller ctx with a backend-internal cancellation source), or have the backend store a cancel-func and trigger it from `Drain` when its deadline expires.

2. **Hash-verify the `os.Stat` short-circuit.** On any path where `Pin` short-circuits because a file already exists at `blockPath`, hash-verify the file against the requested CID before trusting it. On mismatch, remove the file and proceed to the gateway fetch. This is defense in depth — even if a partial file leaks for any reason (kill -9, panic, future regression), next-startup does not promote it.

3. **Atomic `savePins`.** Write to `pins.json.tmp` and `os.Rename` over `pins.json`. Standard Go atomic-write pattern. Closes the narrower process-exit-race window on `pins.json`.

4. **Acceptance test for partial-file cleanup.** A test must simulate a slow-write gateway (drips bytes, never finishes), call `Pin` in a goroutine, trigger `Drain` with a short budget so the deadline fires mid-`io.Copy`, and assert that after `Drain` returns `DeadlineExceeded` no partial file remains at `blockPath`. This closes the prior task's acceptance criterion #2 (which was unmet because the existing test never reaches `io.Copy`).

5. **Pin the http.Client.Timeout invariant.** Once the ctx-propagation fix lands, document or assert that the 2-min `http.Client.Timeout` cannot hold `Pin` goroutines past the drain budget — the drain ctx becomes the binding constraint, not the client timeout.

## Non-goals

- Reworking the discovery callback's ctx ownership directly. The pragmatic fix is to thread a drain-cancellable ctx through `Pin`; the autopin closure stays simple.
- Adding HTTP `/healthz` endpoints.
- Migrating to Boxo or a different IPFS backend (separate task).

## Acceptance

- `Drain`'s deadline expiring causes in-flight `Pin` calls' `ctx` to be cancelled; `io.Copy` returns `context.Canceled`; the existing `os.Remove(path)` cleanup runs; no partial file remains.
- A test simulates a slow gateway during shutdown and asserts the partial block file is absent at `blockPath` after `Drain` returns `DeadlineExceeded`.
- `pins.json` is rewritten atomically (write-tempfile + `os.Rename`).
- The `os.Stat` short-circuit in `Pin` hash-verifies any existing file before trusting it; mismatches are removed.
- A test pins a CID, corrupts the on-disk file to a different byte sequence, and asserts that the next `Pin` call for the same CID detects the mismatch, removes the corrupt file, and re-fetches via the gateway path.

## References

- `agents/docs/solutions/conventions/fetch-abort-controller-bounds-headers-only-2026-05-06.md` — documents the "full-call unbounded after headers" pattern. Explicitly scopes to "IPFS gateway wrappers in pinner code." Same root cause as defect #1 above.
- The narrower drain-barrier mechanism that this task interacts with was archived under the prior pinner-shutdown-drain work; see `agents/docs/tasks-archive.md` for context when present, or `git log -- pinner/` for the implementing commit's diff.

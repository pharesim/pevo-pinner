# PINNER-SHUTDOWN-DRAIN-IN-FLIGHT-PINS — Drain in-flight pin operations on shutdown instead of cancelling

**Owner:** pinner
**Created:** 2026-05-21 (surfaced by full-codebase audit 2026-04-21, `.context/audit-2026-04-21/chunk-6-correctness-reviewer.md` + `chunk-6-reliability-reviewer.md`)
**Priority:** P1 (reliability + correctness)

## Context

`pinner/main.go` shutdown sequence currently:

- Cancels the root context, which cancels all in-flight `backend.Pin(ctx, cid)` calls.
- Calls `backend.Close()` on the embedded node.
- Runs `discovery.Stop` after the backend is already closed.

Two defects from the audit:

1. **Autopin runs against a closed backend.** During the shutdown window, `discovery.refresh` can still be mid-iteration and fire its autopin callback against a backend whose underlying file handles and locks are already gone. The error surfaces as a generic write failure, not as a shutdown signal.
2. **In-flight pins are cancelled, not drained.** A pin operation that's mid-stream from a gateway gets its context cancelled. The partial file in `blocks/<cid>` is left on disk unless the cleanup path runs (the audit notes cleanup is racy — partial file may persist). On next startup, `IsPinned` may report the CID as present despite the file being incomplete.

## Goal

Make shutdown deterministic:

1. **Order:** signal discovery to stop first, wait for it to return, then drain in-flight pins, then close the backend.
2. **Drain mechanism.** Track in-flight `Pin` calls via a `sync.WaitGroup`. On shutdown, stop accepting new pins (via a closed `done` channel checked at entry) and `wg.Wait()` with a hard timeout (e.g., 30s).
3. **Partial-file cleanup on cancelled pin.** If a `Pin` returns context-cancelled mid-`io.Copy`, `os.Remove(path)` before returning. Don't leave a partial block on disk.
4. **Health endpoint reflects draining state.** During shutdown, `/healthz` should return 503 (drained, not ready). See related task if/when `pinner-health-readiness-endpoint.md` is filed — for now, add the state and a log line even without an HTTP endpoint.

## Non-goals

- Reworking the discovery loop's polling cadence or concurrency model. Separate concern.
- Adding circuit breakers around backend operations. Different audit P1.

## Acceptance

- Pinner shutdown waits for in-flight pins to complete (or hard-timeout) before closing the backend.
- A test simulates a slow gateway during shutdown and asserts the in-flight pin either completes or has its partial file cleaned up.
- No log lines about "writing block: ..." against a closed backend during a clean shutdown.

## References

- Audit chunks:
  - `.context/audit-2026-04-21/chunk-6-correctness-reviewer.md` (P1: shutdown runs autopin callback against closed backend).
  - `.context/audit-2026-04-21/chunk-6-reliability-reviewer.md` (P1: in-flight pin operations cancelled not drained on shutdown).

## Architect re-review (2026-05-21) — HELD PENDING FIXES

The drain barrier itself is correct: gate-check + WaitGroup.Add under `drainMu` serializes against signal + Wait; sync.Once on `signalDone`; shutdown order `httpServer.Shutdown → discovery.Stop → backend.Drain → backend.Close` is the right shape. Two items block archive.

A separate task in `tasks/pending/` captures the deeper data-integrity concerns (partial-file-trust on next startup, ctx propagation through Drain, atomic `savePins`, the unmet acceptance criterion #2 test). Those are out of scope here — this task is the drain-barrier mechanism; the new task is the integrity loop around it. Land the two items below to archive this task; the new task carries the rest.

1. **The comment above `discovery.Stop()` in `pinner/main.go` overstates the ordering guarantee.** It claims stopping discovery first prevents fresh autopin callbacks from queueing pins behind the drain barrier. In fact, `discovery.Stop()` only cancels the ticker context; an in-flight `refresh(ctx)` call that has already produced items continues into the `onRefresh` callback (the autopin closure), which iterates synchronously calling `backend.Pin(ctx, cid)` with the long-lived outer ctx. New Pin calls CAN land after `Stop()` returns. The drain gate catches them — that's the actual barrier. Rewrite the comment to be honest: the drain gate is the barrier; `discovery.Stop()` only halts new refresh cycles. Don't promise an ordering invariant the code does not enforce.

2. **The 30s drain budget produces a worst-case shutdown wall-clock of ~45s (10s HTTP + 30s drain + 5s gateway close), which exceeds Docker's default 10s SIGTERM grace.** Community operators running the pinner via `docker run` or their own `docker-compose.yml` without `stop_grace_period: 60s` will have the drain killed mid-flight by SIGKILL — defeating the whole drain mechanism in the most common deployment path. Lower `drainCtx` to ~5s so the worst-case wall-clock (~20s) fits inside reasonable orchestrator defaults. Also document the recommended `stop_grace_period` (and equivalents for systemd / k8s) in the pinner's README so operators who want a longer drain budget know what to configure. The README is in the pinner zone.

Re-review path: when both items land, `git mv` this file back to `tasks/review/` and the next architect review pass picks it up.

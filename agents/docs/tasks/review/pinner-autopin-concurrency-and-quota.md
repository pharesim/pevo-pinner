# PINNER-AUTOPIN-CONCURRENCY-AND-QUOTA — Bound autopin queue and per-author cost

**Owner:** pinner
**Created:** 2026-05-21

## Context

`pinner/main.go:55-76` `SetOnRefresh` callback calls `backend.Pin(ctx, cid)` synchronously per new CID in a serial loop. Under the current `EmbeddedNode`, the HTTP client timeout is 2 minutes and the gateway loop tries up to 4 public gateways, giving an upper bound of ~8 minutes per unreachable CID. A single hostile accredited Hive author broadcasting 50 posts × 50 supplementary CIDs = 2500 unreachable CIDs ≈ 14 days of stalled autopin. The hash-verify + size-cap commits worsened this in the current HEAD because each gateway now drains a full body before deciding to reject.

This is orthogonal to the `pinner-embedded-ipfs-node-via-boxo` rewrite — boxo's bitswap has its own per-fetch timeouts but does not by itself bound the discovery-callback queue or apply per-author quotas. The wedge attack survives any IPFS-subsystem rewrite that keeps the callback structurally serial.

Cross-reviewer corroboration in the 2026-05-21 architect review: adversarial reviewer (P1, 75) + performance reviewer (P2, 75) flagged the same code path.

## Goal

Add two protections to the autopin callback path:

1. **Bounded concurrency.** Replace the serial loop with a bounded goroutine pool (e.g. `errgroup.WithContext` + `SetLimit(N)`, or a semaphore channel). `N` should be small enough that disk I/O and HTTP-fetch CPU don't thrash, large enough that a stuck gateway on one CID does not stall progress on others. Tune empirically; start at `N=4`.
2. **Per-author quota or queue-depth bound.** Either (a) cap the number of CIDs from any single Hive author in one discovery batch (e.g., 20 CIDs/author/batch, drop the rest with a log), or (b) cap total queue depth across the autopin worker pool and shed oldest-or-newest on overflow. Either approach prevents a single hostile broadcast from monopolizing autopin capacity.

The implementer should `/ce-brainstorm` on which quota shape fits PEvO's threat model and operator visibility needs.

## Non-goals

- Cross-batch persistence of the per-author counter. Single-batch quota is sufficient; persistence adds Redis/disk-state complexity that the brainstorm should defer unless a concrete failure mode is identified.
- Retry semantics for dropped/quota-shed CIDs. Once a CID is dropped from one batch, it appears in the next discovery cycle naturally — discovery is idempotent on the Hive post set. No explicit retry queue needed.
- Coupling the bound to the `IPFSBackend` choice. The concurrency and quota apply equally to `EmbeddedNode` (current or post-boxo) and `PinataBackend`.

## Acceptance

- The autopin callback in `pinner/main.go` processes CIDs through a bounded pool, not a serial loop. The bound `N` is configurable via env var + CLI flag (matches the pinner's existing config-knob convention — bare `AUTOPIN_CONCURRENCY` and `--autopin-concurrency`, no `PINNER_` prefix).
- A per-author or per-batch quota mechanism prevents a single hostile author from wedging the queue. Reject-policy is logged so operators can observe drops.
- A test (with a deliberately blocked / unresponsive backend `Pin` stub) demonstrates the bound: a batch of 100 CIDs from one author with `Pin` artificially blocked completes the per-batch quota's worth of attempts and shed the rest, not all 100.
- Operator-visible startup log line shows the configured concurrency + quota values.

## References

- 2026-05-21 architect review of `pinner-content-hash-verify-on-pin`: adversarial reviewer `adv-3` (hostile-author serial-loop wedge) + performance reviewer `PERF-01` (serial autopin loop stall with 2-min gateway timeout).
- Related but separate: `pinner-embedded-ipfs-node-via-boxo.md` (the IPFS-subsystem rewrite). This task targets the autopin queue, not the fetch subsystem.

## Brainstorm output (2026-05-21)

### Summary

Bounded worker pool (default N=4) replaces the serial loop. Per-author CID cap per discovery batch (default 20) bounds a single hostile author's impact while leaving legitimate authors (1-5 CIDs/paper) untouched. Single-batch quota only. Each batch emits one shed-summary log line per author that tripped the cap. Pool drains before the callback returns so discovery batches don't overlap each other's in-flight pins, which composes cleanly with the existing shutdown drain.

### Key decisions

- **Quota shape: per-author cap per batch.** Chosen over total queue-depth cap, belt-and-suspenders combination, and weighted round-robin. Rationale: per-author cap targets the actual attack vector (a single hostile accredited author broadcasting many CIDs) rather than memory pressure; legitimate authors fall far below the cap; operator log identifies the responsible author. Queue-depth alone is threat-blind and would still let a hostile author saturate the queue; combination and round-robin are over-engineered for PEvO's single-instance scale.
- **Default concurrency N=4.** Matches the task author's starting point. Small enough that disk I/O + HTTP fetch CPU don't thrash; large enough that one stuck gateway doesn't stall others.
- **Default per-author cap = 20.** Matches the task's example. A normal paper carries 1 PDF + a handful of supplementary files + a few inline images, well under 20. Multi-paper batches from one author at one moment are unusual outside of mass broadcast.
- **Log shape: one summary line per capped author per batch.** Form `[autopin] shed N CIDs from <author> (per-author cap=<cap>)`. NOT per-shed-CID. Preserves operator visibility without inflating log volume.
- **Quota and pool live in their own file under `pinner/`.** The semaphore/pool, per-author counter map, and shed accounting are larger than fits inline in `main.go`'s `SetOnRefresh` closure.
- **`AutoPinManager.MatchingCIDs` signature widens to return items, not CID strings.** Author is required downstream by the quota. The single existing call site (`main.go` autopin callback) is the only consumer.
- **Pool drains before the callback returns.** Successive discovery batches do not overlap each other's in-flight pins. Composes with `pinner-shutdown-drain-in-flight-pins` (already in `tasks/review/`).

### Configuration knobs

| Env | Flag | Default | Description |
|---|---|---|---|
| `AUTOPIN_CONCURRENCY` | `--autopin-concurrency` | `4` | Max in-flight pin operations per batch. |
| `AUTOPIN_AUTHOR_CAP` | `--autopin-author-cap` | `20` | Max CIDs pinned from any single Hive author per discovery batch. Excess CIDs are shed. |

Naming matches the bare pinner convention (`PORT`, `GATEWAY_PORT`, `APP_TAG`, `MAX_PIN_BYTES`); no `PINNER_` prefix.

### Acceptance refinement (supplements the Acceptance section above)

- Startup log includes both `AUTOPIN_CONCURRENCY` and `AUTOPIN_AUTHOR_CAP` alongside existing knobs.
- The 100-CID-one-author test (per the original Acceptance) asserts: exactly `AUTOPIN_AUTHOR_CAP` worth of `Pin` attempts were made; the rest were shed; a single shed-summary log line was emitted naming the author and the cap.
- A second test asserts the per-author cap does NOT shed across multiple authors: 100 CIDs split across 10 authors (10 CIDs each) all attempt `Pin`, none shed.

### Scope boundaries (clarifications)

The task's existing Non-goals stand. The brainstorm adds:

- Global queue-depth cap as a second backstop. Rejected in favor of single-axis per-author cap; revisit only if a concrete failure mode surfaces.
- Weighted round-robin / fair-share queueing. Rejected as over-engineered for PEvO scale.
- Per-CID drop logging. Rejected to keep log volume bounded.

### Handoff

The pinner agent picks this up via `/ce-work`. The brainstorm-output section above resolves the open decision the task deferred; the original Goal / Non-goals / Acceptance / References remain authoritative for everything else.

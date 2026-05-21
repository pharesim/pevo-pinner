# PINNER-EMBEDDED-IPFS-NODE-VIA-BOXO — Restore the embedded-IPFS-node design intent

**Owner:** pinner
**Created:** 2026-05-21

## Context

The pinner was designed as a single Go executable that ships with an embedded IPFS node so third-party libraries and institutions can mirror PEvO content from one binary, no separate Kubo container or pinning-service account required. The current `pinner/ipfsnode.go` `EmbeddedNode` does not honor that design: it is an HTTP cache that fetches reassembled file bytes from public gateways (`ipfs.io`, `dweb.link`, `cloudflare-ipfs.com`, `gateway.pinata.cloud`) and writes them to `<dataDir>/blocks/<cid>` as a single file per CID. It does not participate in libp2p or bitswap, does not maintain a real blockstore, does not walk dag-pb DAGs, and has no trustless verification primitive available to it.

Three P0 hardening commits landed against the wrong layer:

- `a9091bc1 pinner(cid-validation-on-autopin-path)` — `ValidateCID` at autopin discovery + every backend entry. Independently useful; archived on its own merits.
- `7e2e0458 pinner(response-size-cap-on-gateway-fetch)` — `io.LimitedReader` cap on gateway reads. Defense-in-depth for the current HTTP-cache code; becomes moot once a real IPFS node is in place (boxo enforces its own per-block limits and trusts bitswap's block-level verification). Archived on its own merits.
- `28167cb6 pinner(content-hash-verify-on-pin)` — multihash-verify gateway body bytes against the CID. Verified empirically (2026-05-21) to reject 100% of real `ipfs add`-produced content: public gateways return reassembled UnixFS file bytes, whose sha2-256 does not equal the CID's multihash digest (which digests the dag-pb root block, not the file content). The test fixture `cidForContent` constructs synthetic CIDs as `cid.NewCidV0(mh.Sum(content, sha2-256))` — a shape `ipfs add` never produces — which made the test pass. Reverted as part of this task because non-functional verification is worse than no verification (false confidence). Task superseded by this one.

## Goal

Replace `EmbeddedNode` with a boxo-based in-process IPFS node. The pinner stays a single Go binary; the IPFS subsystem comes from `github.com/ipfs/boxo` (Kubo's components decomposed for embedding). Trustless verification, bitswap participation, dag-pb walking, and gateway serving are all handled by boxo libraries.

Concrete deliverables (high-level, to be refined via `/ce-brainstorm` when picked up):

1. A new `EmbeddedNode` backed by boxo's blockstore, libp2p host, bitswap, DHT, and HTTP gateway handler.
2. `Pin(cid)` fetches via bitswap (with optional trustless-gateway HTTP fallback using `Accept: application/vnd.ipld.car` so each block is hash-verified during import), stores blocks in the boxo blockstore, and updates `pins.json`.
3. `IsPinned(cid)` checks the boxo pin set.
4. `Unpin(cid)` removes the pin and lets GC reclaim blocks.
5. `/ipfs/<cid>` gateway serving handled by boxo's gateway handler so reassembled-file reads work correctly for any UnixFS DAG (multi-block files supported).
6. Existing `ValidateCID` guards stay at entry points (defense in depth; cheap).
7. Migration path for existing `<dataDir>/blocks/<cid>` files: either re-import on first start (preferred — boxo re-verifies during import) or document as a clean-slate transition (acceptable since beta has minimal user content).

## Non-goals

- Changing the Pinata mode. Unaffected.
- Changing the backend's Kubo container or upload path (`backend/src/routes/ipfs.ts`). PEvO's own upload-side is correct.
- Changing HAF discovery (`pinner/discovery.go`). Discovery feeds CIDs into the backend interface unchanged.
- Removing the `pinata` mode. Some third-party operators prefer it; it stays.
- A full design for libp2p config (NAT traversal, default ports, bootstrap nodes, peer-discovery policy). That's design work for the implementing pinner agent; brainstorm before coding.
- Performance optimization beyond what boxo provides out of the box. Single-instance PEvO traffic does not stress this layer.

## Acceptance

- `pinner/ipfsnode.go` `EmbeddedNode` participates in libp2p+bitswap (verified by a startup log line and an integration test that fetches a known CID from the public DHT).
- For a real `ipfs add`-produced CID, `Pin` fetches the content, every block is hash-verified, the resulting file is served correctly via `GET /ipfs/<cid>` on the pinner's gateway port.
- A unit/integration test exercises the multi-block file path (file >256 KiB) end-to-end and asserts the served bytes equal the original file. The prior single-block-only test fixture (`cidForContent` building `cid.NewCidV0(mh.Sum(content, sha2-256))`) is deleted or rewritten because it constructs synthetic CIDs that don't exist in the real ecosystem.
- Commit `28167cb6` is reverted as part of the rewrite (or its non-functional code removed during the rewrite). The replacement implementation provides trustless verification by construction (bitswap and CAR-import both verify block hashes during transfer).
- `ValidateCID` guards remain at `Pin/Unpin/IsPinned` entry points on both `EmbeddedNode` and `PinataBackend`.
- Single-binary deploy still holds: no separate Kubo container, no required external IPFS service. `docker run pinner` (or equivalent) is sufficient.
- The `IPFSBackend` interface docblock states its verification guarantee per implementation: `EmbeddedNode` (post-rewrite) verifies content via bitswap and CAR-import (block-level hash check by boxo); `PinataBackend` does not see bytes and trusts the Pinata service. `agents/pinner/CLAUDE.md` gains a short "Trust model" section formalizing the same divergence so operators picking `IPFS_MODE=pinata` understand the weaker guarantee. This closes the interface-contract ambiguity surfaced in the 2026-05-21 review (adversarial reviewer's `adv-7` Pinata vs EmbeddedNode hash-verify divergence).

## Implementation notes

Pinner agent: please run `/ce-brainstorm` before starting code work to refine the boxo dependency surface, libp2p config, blockstore choice (badger vs flatfs), gateway-fallback policy (whether to keep the HTTP gateway list as a CAR-fetch fallback for content missing from the DHT), and the migration story for existing `<dataDir>/blocks/<cid>` files. The architect-side synthesis that produced this task is captured in this file's Context + Goal sections; the prior failed approach in commit `28167cb6` is the cautionary anchor.

The reviews that surfaced the structural issue (audit `2026-04-21` + the architect's review of `28167cb6` on `2026-05-21`) treated three different symptoms — false-confidence verification, partial-file cleanup races, layering-order bugs — but the underlying disease is "EmbeddedNode pretends to be an IPFS node and is not." This task addresses the disease.

## Brainstorm decisions (2026-05-21)

Pinner brainstorm refined the five sub-decisions the Implementation notes flagged. Below is the scope the implementer should plan against; an implementation plan (file shape, dependency surface, test layout) is the next step via `/ce-plan` or direct `/ce-work` once the implementer is ready.

**Sequencing.** Single-shot replacement. No interim hash-verify revert; no parallel coexistence as a third `IPFS_MODE`. The current HTTP-cache `EmbeddedNode` is removed wholesale when boxo lands. Accepted cost: embedded mode stays broken against real `ipfs add`-produced CIDs until boxo ships; operators who need a working pinner in the interim use `IPFS_MODE=pinata`.

**Fetch path.** Bitswap is primary. On per-CID timeout (~60s default, tunable), fall back to a CAR-fetch chain (`Accept: application/vnd.ipld.car`) against a configurable list of trustless gateways. CAR import hash-verifies every block during transfer, so the fallback preserves the trustless trust model — no centralized-gateway trust is added.

**PEvO main as the network anchor.** PEvO main acts as **both** a trusted bootstrap libp2p peer (dialed directly on startup so bitswap always has at least one guaranteed provider for any PEvO CID) AND the first entry in the CAR-fetch fallback chain. Defaults ship pointing at PEvO main's known production endpoints; operators can override and add additional community-pinner peers and gateways for chained discovery.

**Community pinner mesh (auto-discovery).** Pinners advertise their presence on a libp2p pubsub topic namespaced per `APP_TAG` (so `pevo` and `pevotest` meshes stay separate). Heartbeats carry the pinner's libp2p peer ID and, optionally, its public gateway URL. Each pinner maintains a known-peers cache built from received heartbeats, dials those peers as additional bitswap providers, and chains their gateways into the CAR-fetch fallback list after PEvO main but ahead of generic public gateways. A community pinner that loses connectivity to PEvO main can still operate by routing through fellow pinners it has discovered. Heartbeat cadence, cache TTL, and an operator opt-out for the advertise side are tunable.

**Blockstore.** flatfs (Kubo default — one file per block in sharded directories). Simple, inspectable with `ls`, easy backups, no compaction surprises. Sufficient for projected PEvO scale (papers in the low thousands, each ~10–100 blocks). Badger is rejected as default.

**libp2p / NAT.** DHT **client** mode by default. The pinner queries the DHT and advertises its pinned content as a provider, but does NOT serve routing queries for the wider network — that's "good network citizen" work separable from the pinner's core job. NAT traversal is enabled aggressively (AutoNAT, UPnP / NAT-PMP, circuit relays) because **reachability**, not DHT mode, is the actual lever for offloading PEvO main when downloaders fetch content. Operators with public IPs can opt into server mode.

**Migration.** None. Pre-launch, no installed base. The boxo IPFS repo lives at a new path under `<dataDir>/`; any legacy `<dataDir>/blocks/` from tester machines is left alone (operator deletes by hand if they care).

**Gateway scope.** The pinner's public IPFS HTTP endpoint at `GATEWAY_PORT` serves only **pinned** content — no pull-through bitswap fetch for unpinned CIDs requested by external HTTP callers. Resource-control default; operators can opt in later if they want their pinner to act as a full public gateway.

**Pin state.** Boxo's pinset replaces (or wraps) the current `pins.json` — one less hand-rolled state file. The atomic-write helper introduced for `pins.json` / `autopin.json` is retained for autopin rule storage.

**Test fixtures.** The existing `cidForContent` synthetic-CID fixture is deleted or rewritten to derive real `ipfs add`-shape CIDs via boxo's UnixFS primitives. The fixture's reason for existing — making the structurally non-functional hash-verify pass in tests — disappears with the rewrite. Tests gated by build tag or env flag may exercise real DHT / bitswap; default `go test ./...` must remain offline-only.

**Configuration knobs.** New env vars surface for PEvO main's libp2p multiaddr, PEvO main's gateway URL, libp2p listen port, and an extra fallback-gateway list. Naming follows the bare-prefix convention used elsewhere in the pinner config set.

**Out of scope.** Two-step rollout (revert hash-verify first, boxo later); `IPFS_MODE=boxo` coexisting with HTTP-cache mode; badger blockstore; DHT server-mode default; pull-through public gateway; configurable chunker / block-size / CID-version; any change to Pinata mode; running a separate Kubo container or external IPFS service alongside the pinner binary.

## References

- Empirical verification of the multihash mismatch: architect review session 2026-05-21 (`git diff a9091bc1^..28167cb6` review; CIDs `QmPZ9g...`, `QmTudJ...`, `bafkrei...` curl + multihash compared).
- Original audit chunk that motivated the hash-verify task: `.context/audit-2026-04-21/chunk-6-correctness-reviewer.md`.
- Boxo: `github.com/ipfs/boxo` (Kubo libraries decomposed for embedding).
- Boxo gateway client (trustless CAR fetch): see boxo's `boxo/gateway/client` or its successor.

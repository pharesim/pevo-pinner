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

## References

- Empirical verification of the multihash mismatch: architect review session 2026-05-21 (`git diff a9091bc1^..28167cb6` review; CIDs `QmPZ9g...`, `QmTudJ...`, `bafkrei...` curl + multihash compared).
- Original audit chunk that motivated the hash-verify task: `.context/audit-2026-04-21/chunk-6-correctness-reviewer.md`.
- Boxo: `github.com/ipfs/boxo` (Kubo libraries decomposed for embedding).
- Boxo gateway client (trustless CAR fetch): see boxo's `boxo/gateway/client` or its successor.

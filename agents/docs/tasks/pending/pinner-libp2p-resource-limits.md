# PINNER-LIBP2P-RESOURCE-LIMITS — Bound libp2p connection + stream concurrency

**Owner:** pinner
**Created:** 2026-05-21

## Context

`newLibp2pHost` constructs the libp2p host with `NATPortMap`, `EnableNATService`, `EnableHolePunching`, and `EnableRelay` but configures neither a `ConnectionManager` nor a `ResourceManager`. The defaults are intentionally permissive — go-libp2p ships an auto-scaled resource manager that allows large numbers of inbound connections and streams. With Relay enabled the pinner is also reachable through public relays even when behind NAT, so the inbound surface is wider than a typical "behind NAT" deployment.

The boxo-review adversarial pass (P1) flagged this as a connection-flood / stream-flood DoS vector: an attacker opening 10k inbound connections or streams can exhaust the pinner's file descriptors and stall bitswap. Reliability review independently flagged it as residual risk.

## Goal

Configure both managers explicitly with values tuned for a small, public, single-instance pinner.

1. **`libp2p.ConnectionManager`** — low-watermark / high-watermark connection pruning. Likely target: `low=200`, `high=400`, grace `30s`. Pruning trims idle inbound connections so a peer flood does not crowd out useful peers.

2. **`libp2p.ResourceManager`** — explicit `rcmgr.NewResourceManager` with conservative per-protocol / per-peer / per-conn limits. Likely target: scope-defaults sized for a Raspberry-Pi-class operator (the documented minimum target for the pinner), with explicit ceilings on inbound streams and total memory.

3. **Operator override** — env knobs so an operator on a beefier host can raise the ceilings without rebuilding. Suggested shapes:
   - `LIBP2P_CONNMGR_LOW` / `LIBP2P_CONNMGR_HIGH`
   - `LIBP2P_RCMGR_MAX_INBOUND_STREAMS_PER_PEER`
   - `LIBP2P_RCMGR_MEMORY_LIMIT_MB`

4. **Startup log** — print the effective values so operators can see them without enabling debug logs.

## Non-goals

- A private libp2p swarm key. Out of scope; the threat model accepts public-mesh participation.
- Disabling Relay / HolePunching. Both improve reachability for behind-NAT pinners and the trust model is unaffected.
- Per-CID admission control. That belongs in `pinner-autopin-concurrency-and-quota.md` (already in `review/`).

## Acceptance

- `libp2p.New` is called with `libp2p.ConnectionManager(...)` and `libp2p.ResourceManager(...)`, both populated from config defaults that can be overridden by env / CLI flag.
- A startup log line reports the effective low/high watermarks and the chosen rcmgr scope.
- An integration test (or a documented manual repro) demonstrates that the rcmgr actually pushes back: open >max inbound streams from a peer and assert subsequent streams are rejected at the rcmgr boundary.

## References

- Boxo review adversarial finding #6 (no libp2p ConnectionManager / ResourceManager — connection-flood + stream-flood DoS).
- Boxo review reliability residual: "libp2p resource manager (rcmgr) is not configured. go-libp2p v0.48 defaults to an auto-scaled resource manager that allows an unbounded number of connections under high peer discovery load".
- go-libp2p docs: rcmgr scope tree (system / transient / service / protocol / peer / connection / stream).

# PINNER-STATUS-API-RUNTIME-STATE — Expose new boxo runtime state via HTTP `/api/status`

**Owner:** pinner
**Created:** 2026-05-21

## Context

The boxo rewrite added substantial new runtime state inside `EmbeddedNode`:

- libp2p peer identity (peer ID + listen multiaddrs).
- Live connected-peer count (`host.Network().Peers()`).
- Mesh known-peers snapshot (per-peer ID, gateway URL, last-seen).
- Drain state (accepting pins or not).
- In-flight Pin count.
- Effective bitswap timeout, MAX_PIN_BYTES, CAR-fallback chain.
- Mesh advertise mode (publish vs subscribe-only).

`StatusResponse` (the response body of `GET /api/status`) does not expose any of this. The boxo-review ce-agent-native-reviewer flagged 4 must-have + 3 should-have gaps: an agent or operator polling `/api/status` after the rewrite sees exactly the same fields as before, despite the new state being meaningful for operating a community pinner fleet.

The `StatusResponse` struct comment is explicit that the response shape is the canonical agent-facing surface (`so an operator (or agent) who did not start the process can read back the enforced limits without parsing startup logs`), so adding the new fields is on-charter — it just wasn't part of the boxo task scope.

## Goal

Add the new runtime state to `/api/status` in a backwards-compatible way (new fields, never renames). Must-have first; should-have can land in a follow-up:

**Must-have:**

1. `libp2p_peer_id` (string) — `host.ID().String()`.
2. `libp2p_listen_addrs` ([]string) — `host.Addrs()` rendered as `<multiaddr>/p2p/<peerid>`.
3. `libp2p_peer_count` (int) — `len(host.Network().Peers())`.
4. `mesh` (object) — `{peer_count, advertise_mode, peers: [{peer_id, gateway_url, last_seen_seconds_ago}]}` — sourced from `mesh.KnownPeers()`.
5. `draining` (bool) — `true` once Drain or Close has been called.

**Should-have:**

6. `bitswap_timeout_seconds` (number).
7. `max_pin_bytes` (number).
8. `car_fallback_chain` ([]string) — the operator-supplied head only (default-tail is implicit; advertising it pollutes the response).
9. `in_flight_pins` (int) — count of registered `cancels` under `drainMu`.

Embedded-mode fields should be omitted (or `null`) in Pinata mode rather than returning zero values. The `backendMode` helper + the `IPFSMode` field already discriminate.

A `NodeStats() NodeStatsResponse` method on `IPFSBackend` (with a no-op on `PinataBackend`) keeps `Server.handleStatus` free of type assertions — recommended but not required for the must-have set.

## Non-goals

- A `/healthz` endpoint. Tracked separately in CLAUDE.md security considerations.
- Real-time push (SSE / WebSocket). Polling `/api/status` is the contract.
- Authentication on `/api/status`. The management API is already `127.0.0.1`-bound; auth is a separate concern.

## Acceptance

- `StatusResponse` carries the must-have fields when `IPFSMode=embedded`.
- Pinata mode response is unchanged in shape from before the boxo task (no new fields, no unexplained zeros).
- A test calls `/api/status` against a `newTestEmbeddedNode`, asserts the new fields are present, populated, and well-typed.
- The README's API section documents the new fields with one-line semantics each.
- The static UI surfaces at minimum the libp2p peer count and the mesh peer count somewhere visible to operators (per the agent-native parity rule: any state visible to agents should also be visible to operators).

## References

- Boxo review ce-agent-native-reviewer report (10 findings: 4 must-have, 3 should-have, 3 observations).
- `server.go` `StatusResponse` definition + `handleStatus`.
- `ipfsnode.go` exported state surface: `host`, `mesh`, `done`, `cancels`, `bitswapTimeout`, `maxPinBytes`, `fallbackGateways`.
- `ipfsnode_pubsub.go` `meshManager.KnownPeers()`.

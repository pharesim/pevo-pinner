---
title: "Boxo gateway pinned-only requires HTTP-route guard, not just the offline blockservice"
date: 2026-05-21
category: conventions
module: pinner
problem_type: convention
component: gateway
severity: high
applies_when:
  - "Wiring boxo's `gateway.NewBlocksBackend` against any blockservice"
  - "Designing an IPFS-shaped HTTP surface that must only serve content the operator explicitly pinned (not every block in the blockstore)"
  - "Reviewing any boxo configuration where the word `offline` could be misread as `pinned-only`"
  - "Adopting boxo as a dependency in a system that has its own pin-set definition (pins.json, recursive pinset, application-tracked allowlist)"
tags:
  - boxo
  - ipfs-gateway
  - offline-blockservice
  - pinned-only
  - session-bleed
  - access-control
  - http-route-guard
---

# Boxo gateway pinned-only requires HTTP-route guard, not just the offline blockservice

## Context

The pinner's boxo HTTP gateway is wired against the offline blockservice so that HTTP traffic never triggers a bitswap fetch:

```go
backend, err := gateway.NewBlocksBackend(n.offlineSvc)
// offlineSvc := blockservice.New(bs, offlinexchange.Exchange(bs))
cfg := gateway.Config{NoDNSLink: true, DeserializedResponses: true}
boxoHandler := gateway.NewHandler(cfg, backend)
mux.Handle("/ipfs/", n.gatewayGuard(boxoHandler))
```

The original `gatewayGuard` validated CID shape only. The brainstorm intent was "pinner serves only pinned content" — and the implementer assumed that wiring the gateway to the offline blockservice was the enforcement mechanism.

The coverage gap that the boxo-review adversarial pass surfaced: **`offline` means "do not initiate a bitswap fetch," not "only serve content from the pin set."** The offline blockservice happily serves any block already in the blockstore. The blockstore typically contains more than the pin set:

- **Bitswap session-bleed.** When `merkledag.FetchGraph` walks a pinned DAG, bitswap pulls every referenced block into the blockstore. The pin set records the root CID, but every descendant block ends up in the blockstore individually addressable. A request to `GET /ipfs/<child-block-cid>` succeeds, even though the operator never pinned that child as a top-level CID.
- **Partial-CAR-import orphans.** When `fetchCARFromGateway` is cancelled mid-stream, the blocks imported so far stay in the blockstore. The corresponding pin record was never written. Those orphan blocks remain reachable via the gateway forever (no GC implemented yet).
- **Future GC orphans.** Any garbage-collection layer that lags behind unpin operations leaves orphan blocks reachable until the next sweep.

For the pinner's threat model this is meaningful: hostile-author content can land in the blockstore as a side-effect of a sibling pin (when the hostile author's CIDs are descendants of an otherwise-pinned DAG, or when a partial import races with cancellation). The operator's explicit pin choice is the access-control surface; the blockstore's contents are not. The fix landed in commit `6067035`.

## Guidance

**Rule.** When boxo's gateway must enforce a "pinned-only" contract, add the access-control check at the HTTP route layer, not at the blockservice layer. The blockservice answers "do we have this block?" The access-control gate answers "is the operator willing to serve this CID?"

The cheapest correct shape: in the HTTP route guard, look up the requested top-level CID against the application's pin set and 404 on miss.

```go
func (n *EmbeddedNode) gatewayGuard(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        path := r.URL.Path
        var raw string
        switch {
        case strings.HasPrefix(path, "/ipfs/"):
            raw = path[len("/ipfs/"):]
        case strings.HasPrefix(path, "/ipns/"):
            next.ServeHTTP(w, r); return
        default:
            next.ServeHTTP(w, r); return
        }
        if idx := strings.Index(raw, "/"); idx != -1 {
            raw = raw[:idx] // strip sub-path; pin lookup is on the leading CID only.
        }
        if raw == "" {
            http.Error(w, "missing CID", http.StatusBadRequest); return
        }
        if err := ValidateCID(raw); err != nil {
            http.Error(w, err.Error(), http.StatusBadRequest); return
        }
        n.mu.RLock()
        pinned := n.pins[raw]
        n.mu.RUnlock()
        if !pinned {
            http.Error(w, "not pinned", http.StatusNotFound); return
        }
        next.ServeHTTP(w, r)
    })
}
```

Granularity choice: **check the leading CID component only**. Sub-paths of a pinned root (`/ipfs/<pinned-root>/file.pdf`) work as expected because boxo resolves sub-paths internally from the root via the blockservice — those internal block lookups are not exposed as separate `/ipfs/<child-hash>` requests. A request for `/ipfs/<some-descendant-block-cid>` directly is rejected because the descendant is not a pinned root. This matches the pinner's pin semantics (recursive pin = serve the whole DAG; descendant CIDs are not separately addressable as root content).

**Do not rely on `gateway.Config` flags to enforce access control.** The boxo gateway config has options like `NoDNSLink`, `DeserializedResponses`, and per-handler subdomain routing, but none of them implement "serve only what is in <application's external pin set>." Boxo's pinning system (when used) is internal to boxo — applications that maintain their own pin record (`pins.json`, recursive pinset, app-managed allowlist) must enforce the gate themselves.

**Be deliberate about CID-form normalization.** The pin lookup uses the exact string form recorded at pin time. A pinned CIDv0 (`QmFoo...`) does not match a request for the equivalent CIDv1 (`bafy...`) and vice versa unless the application normalizes at pin time and lookup time. For the pinner, exact-form matching is acceptable — operators can pin both forms if both should be served. Document the choice; do not silently normalize.

**Code-review rule.** When reviewing a new boxo gateway wiring, ask:

- Is the underlying blockservice online or offline? (Online: bitswap fetches on demand. Offline: serves anything in the local blockstore.)
- What is the application's access-control surface? (Pin set, allowlist, public-by-design?)
- Where is access control enforced — boxo config, application middleware, or assumed-from-blockservice-choice?
- For "pinned-only" enforcement: is the pin set lookup at the HTTP route layer, or somewhere that doesn't actually gate the request?

## Why This Matters

The `offline` qualifier in boxo's blockservice naming carries an intuition that "offline means safe" — no network traffic, so no surprises. In practice it means something narrower: the blockservice will refuse to *fetch* missing blocks over bitswap, but it will happily *serve* any block already present in the blockstore. The semantic distance between "doesn't fetch" and "doesn't serve" is exactly where the access-control gap lives.

This is structurally analogous to filesystem permission gotchas where read-only mounting prevents writes but allows reads of files an attacker placed there earlier. The mount mode constrains one direction; access control is a separate concern.

The blockstore-vs-pin-set distinction matters more for community-operated pinners than for personal IPFS nodes. A personal node's blockstore contents reflect a single user's deliberate choices. A community pinner's blockstore reflects the union of (a) every CID an operator chose to pin, plus (b) every block bitswap pulled to satisfy a recursive pin, plus (c) every partial import that ever stalled. (b) and (c) are operator-invisible side-effects that the offline-blockservice assumption silently surfaces as publicly-fetchable content.

The boxo documentation does not lead with this. The `NewBlocksBackend` constructor signature accepts any `BlockService`, and the pattern of passing an offline service "for safety" is common in IPFS gateway tutorials. The intuition that "offline = safe" is reinforced by every example that does not have a separate pin set to enforce. Applications that *do* have a separate pin set must add the gate themselves.

## When to Apply

- **Building any HTTP surface on top of boxo where the operator's intent is "only serve what I explicitly chose to pin."**
- **Designing an application-managed pin set that lives outside boxo's own pinning system** (pins.json, recursive pinset, app allowlist).
- **Reviewing any new boxo gateway configuration.** Apply the four review questions above.
- **Auditing existing boxo deployments** for the same pattern — if the access-control story is "we use the offline blockservice," there is a gap.

Does **not** apply when:
- The boxo gateway is intentionally a public open gateway (no pin set to enforce; serving any block is the contract).
- The application uses boxo's internal pinning system as the single source of truth and only pinned content ever enters the blockstore (rare in practice).

## Examples

### Vulnerable: rely on offline blockservice for access control

```go
backend, _ := gateway.NewBlocksBackend(n.offlineSvc)
cfg := gateway.Config{NoDNSLink: true, DeserializedResponses: true}
boxoHandler := gateway.NewHandler(cfg, backend)
mux.Handle("/ipfs/", n.gatewayGuard(boxoHandler))

// gatewayGuard only validates CID shape:
func (n *EmbeddedNode) gatewayGuard(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        raw := strings.TrimPrefix(r.URL.Path, "/ipfs/")
        if err := ValidateCID(raw); err != nil {
            http.Error(w, err.Error(), http.StatusBadRequest); return
        }
        next.ServeHTTP(w, r)  // <- serves anything in the blockstore
    })
}
```

A `GET /ipfs/<descendant-of-some-pinned-root>` succeeds. A `GET /ipfs/<partial-import-orphan>` succeeds. Both are reachable to anyone who can hit the gateway.

### Fixed: HTTP-route guard checks the application's pin set

```go
func (n *EmbeddedNode) gatewayGuard(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // ... CID-shape validation as before ...
        n.mu.RLock()
        pinned := n.pins[raw]
        n.mu.RUnlock()
        if !pinned {
            http.Error(w, "not pinned", http.StatusNotFound); return
        }
        next.ServeHTTP(w, r)
    })
}
```

A `GET /ipfs/<pinned-root>` succeeds. A `GET /ipfs/<pinned-root>/file.pdf` succeeds (boxo resolves the sub-path internally; the route guard only checks the leading CID). A `GET /ipfs/<unpinned-cid>` returns 404 regardless of whether the block happens to be in the blockstore.

### Negative-test pattern: prove the access-control gate fires

```go
func TestGatewayDeniesUnpinnedCID(t *testing.T) {
    node := newTestEmbeddedNode(t)
    srv := httptest.NewServer(node.gatewayGuard(passthroughHandler()))
    t.Cleanup(srv.Close)
    resp, _ := http.Get(srv.URL + "/ipfs/QmYwAPJzv5CZsnAzt8auVZRn5RnAvTPnG3vMr1pUgQy9k7")
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusNotFound {
        t.Errorf("status = %d, want 404", resp.StatusCode)
    }
}
```

Pair with positive coverage (pinned root reaches the handler) and sub-path coverage (`/ipfs/<pinned-root>/sub` reaches the handler) so the granularity choice is explicit.

## Related

- `agents/docs/tasks/pending/pinner-gateway-origin-scoping.md` — the next layer on the boxo gateway: even with pinned-only enforcement, hostile-author HTML/JS in a pinned DAG executes under the shared `/ipfs/` origin. Decide between CSP block-scripts, subdomain gateway, or localhost-only-as-policy.
- boxo `gateway.NewBlocksBackend` API: takes a `BlockService` of any shape; the access-control contract is intentionally the caller's responsibility.
- `agents/docs/solutions/conventions/libp2p-pubsub-authenticate-via-msg-getfrom-2026-05-21.md` — the other half of the boxo-review hardening pass: when a layer offers a security signal (authenticated sender, pinned-set membership), the application must consume it; the signal is not automatic.

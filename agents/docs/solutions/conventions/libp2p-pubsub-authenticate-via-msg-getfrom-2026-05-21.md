---
title: "libp2p-pubsub: authenticate publishers via msg.GetFrom(), not in-payload peer-ID"
date: 2026-05-21
category: conventions
module: pinner
problem_type: convention
component: mesh
severity: critical
applies_when:
  - "Building any application-layer message handler on top of libp2p-pubsub (GossipSub, FloodSub, RandomSub)"
  - "Implementing peer-discovery via pubsub heartbeats that carry metadata (multiaddrs, gateway URLs, capabilities)"
  - "Storing peer metadata keyed by peer-ID in a cache or routing table"
  - "Reviewing any JSON or protobuf payload schema that includes a `peer_id` or `sender` field"
  - "Designing message handlers in any pubsub system that authenticates publishers at the transport layer (NATS, Kafka with SASL, etc.) but is consumed by code that trusts payload-claimed identity"
tags:
  - libp2p
  - pubsub
  - gossipsub
  - authentication
  - peer-id
  - ssrf
  - address-book-pollution
  - mesh-discovery
---

# libp2p-pubsub: authenticate publishers via msg.GetFrom(), not in-payload peer-ID

## Context

The pinner's community-pinner mesh on `/pevo-pinners/<APP_TAG>/heartbeat/1.0.0` ingests JSON heartbeat messages from other pinners. The original `applyHeartbeat` (`ipfsnode_pubsub.go`) decoded the in-payload `hb.PeerID` field, looked up the matching `peer.ID`, and used it as the cache key in `m.peers` and as the target of a `host.Connect` dial:

```go
func (m *meshManager) applyHeartbeat(ctx context.Context, hb heartbeat) {
    pid, err := peer.Decode(hb.PeerID)   // <- trusted payload field
    if err != nil { return }
    // ...
    m.peers[pid] = &meshPeer{addrInfo: peer.AddrInfo{ID: pid, Addrs: addrs}, ...}
    go m.host.Connect(dialCtx, mp.addrInfo)
}
```

The handler looked complete — the JSON shape was symmetric with what the publisher constructed in `publishHeartbeat`, and gossipsub at the transport layer ensures only well-formed signed messages reach the subscription. The coverage gap that the boxo-review adversarial pass surfaced: **gossipsub authenticates the publisher (`msg.GetFrom()`), but the application-layer handler ignored that signal and trusted a separate identity field inside the payload.**

Any peer on the topic could publish a heartbeat with `PeerID=<victim>`, attacker-controlled multiaddrs, and an attacker-controlled gateway URL. The handler overwrote `m.peers[<victim>]` with that data. Two downstream consequences:

1. **Address-book pollution + forced dial.** `host.Connect(dialCtx, mp.addrInfo)` dialed the attacker's multiaddrs under the victim's peer-ID — wasting connections, polluting the connection manager's view of the victim, and (via the connect-on-discovery side-effect) using the pinner as a probe for attacker-chosen targets.
2. **CAR-fetch chain amplification.** `assembleFallbackChain` later spliced `meshPeer.gatewayURL` into the per-Pin HTTP fallback list. Combined with the lack of SSRF-guarding on `hb.GatewayURL`, every pinner could be pivoted into the operator's intranet — `http://169.254.169.254/`, `http://localhost:5432/`, RFC1918 hosts, even `file:///etc/passwd`.

Cross-reviewer corroboration in the 2026-05-21 code review (security P1, adversarial P0) raised this to P0 in synthesis. The fix landed in commit `6067035`.

## Guidance

**Rule.** When a pubsub system authenticates publishers at the transport layer, the application-layer message handler must use that authenticated identity (in libp2p: `msg.GetFrom()`) as the authoritative peer-ID. Any peer-ID claim inside the payload is informational at best — treat it like a self-reported header in an HTTP request body that does not come from the verified header set.

Two acceptable shapes:

1. **Pass the authenticated sender into the handler explicitly.** Decouples authentication from the payload schema, makes the trust boundary visible in the function signature.

   ```go
   func (m *meshManager) recvLoop(ctx context.Context) {
       for {
           msg, err := m.sub.Next(ctx)
           if err != nil { return }
           sender := msg.GetFrom()           // <- authoritative
           if sender == m.host.ID() { continue }
           var hb heartbeat
           if err := json.Unmarshal(msg.Data, &hb); err != nil { continue }
           m.applyHeartbeat(ctx, sender, hb)  // <- sender, not hb.PeerID
       }
   }

   func (m *meshManager) applyHeartbeat(ctx context.Context, sender peer.ID, hb heartbeat) {
       // Reject impersonation outright: if the publisher claimed a peer-ID at
       // all, it must agree with the authenticated sender.
       if hb.PeerID != "" {
           claimed, err := peer.Decode(hb.PeerID)
           if err != nil || claimed != sender { return }
       }
       // Cache + dial keyed off `sender`, never `hb.PeerID`.
   }
   ```

2. **Drop the payload peer-ID field entirely.** If the payload never carries a peer-ID, there is no impersonation surface. This is the cleanest option for new protocols. The pinner kept the field for backward-compat with deployed publishers; the authentication check above reduces it to "informational sanity check, rejected on mismatch."

**Validate every attacker-controlled URL or address before it crosses a trust boundary.** Even with `msg.GetFrom()` authentication, the payload can still carry attacker-controlled URLs that the application splices into HTTP calls. Apply the standard SSRF guard at the ingestion point:

```go
func validateMeshGatewayURL(raw string, allowPrivate bool) (string, error) {
    u, err := url.Parse(strings.TrimSpace(raw))
    if err != nil { return "", err }
    scheme := strings.ToLower(u.Scheme)
    if scheme != "http" && scheme != "https" {
        return "", fmt.Errorf("scheme %q not allowed", u.Scheme)
    }
    if u.User != nil { return "", errors.New("userinfo not allowed") }
    host := u.Hostname()
    if host == "" { return "", errors.New("empty host") }
    if !allowPrivate && isUnsafeHost(host) {
        return "", fmt.Errorf("host %q not allowed", host)
    }
    // Reconstruct normalized form so downstream dedupe is consistent.
    u.Host = strings.ToLower(u.Host)
    u.Path = strings.TrimRight(u.Path, "/")
    u.Fragment = ""
    u.RawQuery = ""
    return u.String(), nil
}
```

`isUnsafeHost` rejects loopback, RFC1918, link-local, multicast, unspecified, and the literal `localhost` (some resolvers point it elsewhere). DNS resolution failure counts as unsafe so a hostname that doesn't resolve cannot bypass the guard. An operator opt-in (`MeshAllowPrivate` for trusted-network meshes) is acceptable; a default-allow is not.

**Bound per-peer state.** A malicious publisher can flood a topic with heartbeats. Cap (a) the per-message size at the pubsub layer (`pubsub.WithMaxMessageSize(...)` — gossipsub default is ~1 MiB, far larger than legitimate heartbeats need), (b) the per-peer multiaddr list before storing in the cache, and (c) the total cache size so a peer-flood cannot exhaust memory.

**Code-review rule.** When reviewing a new pubsub message handler, ask:

- Does the handler use `msg.GetFrom()` (or the equivalent transport-layer authenticated identity) as the authoritative source for any peer/sender identity?
- Is any URL, multiaddr, or routable address from the payload validated against an SSRF guard before it enters an HTTP / network call site?
- Are per-peer state structures bounded so a single hostile publisher cannot exhaust memory?

## Why This Matters

libp2p-pubsub (and most production pubsub systems: NATS with NKey auth, Kafka with SASL, etc.) authenticates publishers at the transport layer. That authentication is **per-message**: each delivered message carries a verified sender identity exposed via an API like `msg.GetFrom()`. The application layer can choose to use that signal or not. Most do not by default — the canonical pattern in tutorials and example code is to unmarshal the payload, extract identity fields from it, and key state off those fields. The transport-layer authentication is "ambient" in the same way that a TLS-terminating reverse proxy verifies a client certificate: the verified identity is available, but only consumed if the application asks for it.

The gap is invisible at the handler's surface. Reading the original `applyHeartbeat`, the code is symmetric — the publisher writes `hb.PeerID = m.host.ID().String()`, the receiver reads it back. The handler does the same thing the publisher does, which feels correct. The bug only surfaces under adversarial review when someone asks: "what if the publisher and the claimed PeerID don't match?"

The amplification matters because peer-ID-keyed state usually feeds downstream trust decisions: connection establishment, routing, content discovery, address-book population, CAR-fetch chains. A single payload-trust bug at the message-handler layer cascades into every downstream system that consumes the peer-ID-keyed state.

The same shape recurs in any pubsub-driven mesh where the application reuses payload identity instead of the transport-authenticated identity. NATS subjects with publisher metadata. Kafka headers vs. authenticated client identity. WebSocket session subscriptions where the JWT identifies the session but the message body claims a different actor. All structurally the same gap.

## When to Apply

- **Any new pubsub-based protocol design.** Decide on the trust source for peer-identity at design time — payload vs. transport-authenticated. Document it.
- **Any code review of a pubsub message handler.** Check the three review questions above.
- **Any peer-discovery system that caches metadata keyed by peer-ID.** Verify the cache key comes from authenticated identity, not payload identity.
- **Any handler that splices payload-supplied URLs / multiaddrs into network call sites.** SSRF guard before splice.

Does **not** apply when:
- The pubsub system has no transport-layer publisher authentication (rare in production; in that case the entire pubsub channel must be treated as untrusted and a separate authentication layer is needed).
- The message is broadcast intentionally without identity (e.g., gossip of public state where the sender is irrelevant).

## Examples

### Vulnerable: trust payload identity (the original `applyHeartbeat`)

```go
func (m *meshManager) recvLoop(ctx context.Context) {
    for {
        msg, err := m.sub.Next(ctx)
        if err != nil { return }
        if msg.GetFrom() == m.host.ID() { continue }
        var hb heartbeat
        if err := json.Unmarshal(msg.Data, &hb); err != nil { continue }
        m.applyHeartbeat(ctx, hb)        // <- sender not threaded through
    }
}

func (m *meshManager) applyHeartbeat(ctx context.Context, hb heartbeat) {
    pid, err := peer.Decode(hb.PeerID)   // <- trusts the payload
    if err != nil { return }
    m.peers[pid] = &meshPeer{addrInfo: peer.AddrInfo{ID: pid, ...}, ...}
}
```

### Fixed: authoritative sender from `msg.GetFrom()`

```go
func (m *meshManager) recvLoop(ctx context.Context) {
    for {
        msg, err := m.sub.Next(ctx)
        if err != nil { return }
        sender := msg.GetFrom()
        if sender == m.host.ID() { continue }
        var hb heartbeat
        if err := json.Unmarshal(msg.Data, &hb); err != nil { continue }
        m.applyHeartbeat(ctx, sender, hb)
    }
}

func (m *meshManager) applyHeartbeat(ctx context.Context, sender peer.ID, hb heartbeat) {
    if hb.PeerID != "" {
        claimed, err := peer.Decode(hb.PeerID)
        if err != nil || claimed != sender { return }
    }
    // ...sender is the cache key for m.peers and the AddrInfo.ID for any dial.
}
```

### Negative-test pattern: prove impersonation is rejected

```go
func TestApplyHeartbeatRejectsImpersonation(t *testing.T) {
    m := newApplyHeartbeatMesh(false)
    attacker := genPeerID(t)
    victim := genPeerID(t)
    hb := heartbeat{PeerID: victim.String(), Multiaddrs: []string{"/ip4/127.0.0.1/tcp/4001"}}
    m.applyHeartbeat(context.Background(), attacker, hb)
    if _, ok := m.peers[victim]; ok {
        t.Error("victim peer-ID was inserted from a spoofed heartbeat")
    }
}
```

Generate real ed25519-backed peer IDs in tests rather than synthetic strings — `peer.Decode("12D3KooWFake")` fails the base58 parser and would silently skip the very paths under test.

## Related

- `agents/docs/solutions/conventions/fetch-abort-controller-bounds-headers-only-2026-05-06.md` — the structurally-analogous gap on the HTTP side: a wrapper's contract appears to bound the full call but only bounds the headers phase. Same shape, different layer: a guard that looks present but only covers part of the surface.
- libp2p-pubsub docs, `Message.GetFrom()`: "The peer ID of the message's signer (set by libp2p-pubsub once signing is enabled, which is the default for GossipSub)."
- go-libp2p-pubsub source: `pubsub.Message` wraps `pb.Message` and `ReceivedFrom`; `GetFrom()` returns the verified signer identity, distinct from the `From` field on the protobuf payload.

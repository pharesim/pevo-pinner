---
title: "Wrapping Node fetch() with AbortController bounds the headers phase, not body-read"
date: 2026-05-06
category: conventions
module: pinner
problem_type: convention
component: http-client
severity: medium
imported_from: pharesim/pevo-science (agents/docs/solutions/conventions/fetch-abort-controller-bounds-headers-only-2026-05-06.md)
imported_on: 2026-05-21
applies_when:
  - "Wrapping an HTTP client call with a timeout/cancel mechanism for a wall-clock bound"
  - "Reviewing any helper that abstracts an HTTP fetch behind a timeout-discipline contract"
  - "Adding new third-party HTTP integrations (IPFS gateways, future API integrations)"
  - "Adapting an existing fetch wrapper to a new caller that needs full-call timeout protection"
tags:
  - fetch
  - abortcontroller
  - timeout
  - wrapper-coverage
  - body-read
  - third-party-http
  - ipfs-gateway
---

# Wrapping Node fetch() with AbortController bounds the headers phase, not body-read

## Import context

This convention was originally written for PEvO's TypeScript backend (against Node's WHATWG `fetch`). It is carried into pevo-pinner because the same conceptual gap applies to any HTTP client that splits response-head and response-body handling — including the pinner's IPFS gateway wrappers (current HTTP-cache `EmbeddedNode.Pin` uses Go's `net/http` with `client.Timeout` and `context.WithTimeout`, which have different but analogous gotchas around streaming body reads).

For Go-specific applicability, see the "When to Apply (Go-flavored)" section at the bottom. The original TypeScript examples are kept as the canonical illustration of the structural insight.

## Context (original, sanitized)

A fetch wrapper in TypeScript used `AbortController` + `setTimeout` to bound an upstream HTTP call:

```ts
const controller = new AbortController();
const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);
try {
  return await fetch(url, { ...init, signal: controller.signal });
} finally {
  clearTimeout(timer);
}
```

The wrapper looked complete — timer + controller + finally-clear is the canonical Node fetch-timeout idiom. An adversarial review surfaced a coverage gap that the wrapper's surface obscures: **Node's `fetch()` resolves as soon as response headers arrive, not when the response body is fully read.** Body-read calls (`Response.json()`, `Response.text()`, etc.) execute AFTER the wrapper has resolved and the `finally` block has cleared the abort timer. The `AbortController` is no longer scheduled to fire. A provider that returns `200 OK` headers within the timeout window but then dribbles body bytes for an arbitrary duration hangs the route handler indefinitely.

## Guidance

**Rule.** When wrapping an HTTP call with a cancel/timeout mechanism, the contract is usually "abort if **headers** don't arrive within N ms," not "abort the whole call within N ms." Body-read calls (`Response.json()`, `.text()`, `.arrayBuffer()`, `.formData()`, `.blob()`, streaming via `Response.body`) run AFTER `fetch()` resolves and are unbounded by any timer cleared in the wrapper's `finally`.

If you need full-call timeout protection (headers AND body-read), pick one:

1. **Keep the timer armed across the body-read.** Don't clear the timeout in `finally` until after the body is consumed; pass the same `AbortController` (or its `AbortSignal`) into the body-read site so the abort propagates through stream cancellation. Caveat: if the wrapper returns the `Response` to a caller, the caller is responsible for body-read cancellation; the wrapper alone cannot enforce it.

2. **Wrap body-read separately with its own timeout.** An `awaitWithTimeout(promise, ms)` helper applied to `await tokenRes.json()` or equivalent. Each phase gets its own bound; the overall call has a max wall-clock cost of `headersTimeoutMs + bodyTimeoutMs`.

3. **Use `AbortSignal.timeout(ms)` and pass it through both legs.** `AbortSignal.timeout(ms)` (Node 17.3+) returns a signal that fires after `ms`; passed as `init.signal` to `fetch()`, it bounds the headers phase. Capturing the same signal at the body-read site (via a `Promise.race` against `signalToPromise(signal)`) extends the bound to the full call.

If the wrapper's caller does NOT need body-read protection, document the gap explicitly. Inline the rationale: "this wrapper bounds the headers phase only; ${UPSTREAM} returns small JSON bodies fast; if a future caller needs full-call bounds, see this doc."

**Code-review rule.** When reviewing a new fetch wrapper, ask three questions before approving:

- What does the wrapper's contract claim — full-call timeout, or headers-phase timeout?
- If full-call, where is the body-read bounded?
- If headers-phase, is the gap documented?

## Why This Matters

The WHATWG fetch specification resolves the `fetch()` promise when the response head (status + headers) is received, deferring body-stream consumption to the `Response` interface's separate methods. This split lets callers stream-read large bodies without blocking on header processing. The cost of the split: a wrapper that times out only the `fetch()` call timeouts only on the head phase, leaving the body-read unbounded.

The gap is invisible at the wrapper's surface. Reading a typical wrapper, a reviewer sees the canonical timeout shape and assumes the contract bounds the whole call. The MDN documentation for `AbortController` reinforces the assumption: examples show "cancellable fetch" without distinguishing headers from body.

The cost is highest when the wrapper is consumed by code paths that read large bodies (paginated APIs, file downloads, streaming endpoints), where the body-read can legitimately take seconds and a slow-body adversary can chain many seconds together.

## When to Apply (Go-flavored)

In Go (`net/http`), the analogous gaps and their fixes:

- **`http.Client.Timeout`** covers the entire request from dial through body-close, BUT only if the caller actually closes the body. If the caller drops the `*http.Response` without `resp.Body.Close()`, the timeout has no effect on a goroutine still holding the connection. Always `defer resp.Body.Close()` immediately after the error check.
- **`context.WithTimeout` passed to `http.NewRequestWithContext`** bounds the round-trip, BUT body-read streaming via `io.Copy(dst, resp.Body)` is NOT cancelled by ctx expiry directly. `io.Copy` blocks until the connection's read deadline fires (which `http.Client.Timeout` sets, but ctx by itself does not). Use both: pass the ctx AND set the client's `Timeout`, OR wrap `resp.Body` in a context-aware reader.
- **`io.LimitedReader`** caps bytes but does not cap time. A gateway that drips one byte every 10 seconds eventually hits the limit but takes hours to do so. Combine with a deadline-bounded reader if slow-stream attacks are in scope.

For the pinner specifically:
- `pinner/ipfsnode.go` `EmbeddedNode.Pin` uses `http.Client` with `Timeout` set; the ctx flowing in from `Drain` does NOT cancel `io.Copy` mid-stream. This is the partial-block-file integrity gap captured in `tasks/pending/pinner-drain-timeout-partial-block-trust.md`.
- The boxo rewrite changes the calculus: bitswap has its own per-block timeouts and trustless verification, so the slow-stream attack surface largely disappears for the bitswap path. The CAR-import fallback retains the same headers-vs-body concern.

## Examples

### Headers-only wrapper (the original gap, TypeScript)

```ts
async function fetchWithTimeout(url: string, init: RequestInit = {}): Promise<Response> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);
  try {
    return await fetch(url, { ...init, signal: controller.signal });
  } finally {
    clearTimeout(timer);
  }
}

// Caller:
const tokenRes = await fetchWithTimeout(url, { method: "POST", body: form });
const tokenJson = await tokenRes.json();   // UNBOUNDED. Provider can dribble bytes here.
```

### Full-call coverage via shared signal (TypeScript)

```ts
const signal = AbortSignal.timeout(ms);
const res = await fetch(url, { ...init, signal });
// Body-read site uses the same signal:
const bodyPromise = res.json();
const abortPromise = new Promise<never>((_, rej) => {
  if (signal.aborted) rej(new Error("body-read timeout"));
  else signal.addEventListener("abort", () => rej(new Error("body-read timeout")), { once: true });
});
const tokenJson = await Promise.race([bodyPromise, abortPromise]);
```

### Go equivalent (full-call bound via client.Timeout + ctx propagation)

```go
func fetchWithDeadline(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
    client := &http.Client{Timeout: 30 * time.Second}
    req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
    if err != nil { return nil, err }
    resp, err := client.Do(req)
    if err != nil { return nil, err }
    defer resp.Body.Close()
    // Wrapping in LimitedReader caps bytes; client.Timeout caps wall-clock.
    return io.ReadAll(&io.LimitedReader{R: resp.Body, N: maxBytes + 1})
}
```

Note both `Timeout` (wall-clock) and `LimitedReader` (bytes) — neither alone is sufficient against a hostile gateway.

## Related

- `tasks/pending/pinner-drain-timeout-partial-block-trust.md` — the active task that captures how this gap surfaces in the pinner's Drain + io.Copy interaction.
- `tasks/pending/pinner-embedded-ipfs-node-via-boxo.md` — the architectural fix that retires most of the HTTP-fetch surface (bitswap + DHT replace the gateway loop).
- WHATWG fetch standard, [Body section](https://fetch.spec.whatwg.org/#body-mixin): the spec text that defines the headers/body split.

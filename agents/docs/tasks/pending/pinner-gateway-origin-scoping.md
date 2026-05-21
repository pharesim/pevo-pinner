# PINNER-GATEWAY-ORIGIN-SCOPING — Decide CSP vs subdomain vs localhost-only for the boxo gateway

**Owner:** pinner
**Created:** 2026-05-21

## Context

The boxo HTTP gateway is wired with `gateway.Config{DeserializedResponses: true, NoDNSLink: true}` and a path-style mux. Every pinned CID is served under the same origin: `http://<pinner-host>/ipfs/<cid>`. With the boxo-review hardening commit the gateway now defaults to `127.0.0.1` bind, which limits the blast radius to the operator's own browser — but operators that opt in to public exposure via `GATEWAY_BIND=0.0.0.0` are exposed to the path-style XSS problem: a hostile UnixFS payload containing HTML/JS executes under the pinner's gateway origin and can read other `/ipfs/*` responses, localStorage, cookies, etc.

Security review flagged this as P2. Adversarial review noted the same surface combined with the (now-fixed) pinned-only contract — even with pinned-only, the operator-curated pinset can include hostile-author HTML/JS that the operator did not screen.

## Goal

Pick a design that lets operators safely expose the gateway publicly.

Options to evaluate (likely via `/ce-brainstorm`):

1. **CSP block-scripts on `/ipfs/*`** — wrap the boxo handler in middleware that sets `Content-Security-Policy: script-src 'none'; sandbox; default-src 'none';` and `X-Content-Type-Options: nosniff`. Pros: tiny code surface; preserves path-style URLs; works for PDFs, images, plain data. Cons: breaks any pinned content that legitimately needs JS (HTML papers, interactive notebooks) — PEvO content is PDF-shaped today but the option closes that door.

2. **Boxo subdomain gateway** — wrap with `gateway.WithHostname(...)` and configure subdomain routing so each CID gets its own origin (`<cid>.ipfs.<pinner-host>/...`). Pros: per-CID origin is the standard IPFS gateway hardening; JS in one CID cannot read another. Cons: requires wildcard DNS, larger config surface, breaks the `127.0.0.1:port` simplicity.

3. **Keep localhost-only as the documented stance** — declare that public exposure is unsupported, refuse to bind non-loopback addresses unless `--gateway-bind-public` is explicitly passed and the operator accepts the documented risk. Pros: smallest code change; matches the today-default behavior of the hardening commit. Cons: punts the design rather than solving it.

The recommended starting brainstorm anchor: **(1) CSP block-scripts**, gated on `GATEWAY_BIND` being non-loopback so localhost operators preserve the friendliest experience and public operators get the safer default. Subdomain gateway is the textbook-correct option if the operator surface ever needs to host interactive content.

## Non-goals

- Reworking the gateway handler to do per-CID auth. Out of scope.
- Adding rate limiting at the gateway layer. Separate concern, separate task if needed.

## Acceptance

- The chosen design lands behind a config knob (or as an unconditional default for public binds) and is documented in `.env.example` + README.
- An offline test asserts the chosen guarantee: if CSP, that script-execution headers are present; if subdomain, that the configured routing emits the expected `Host:` header expectations; if localhost-only, that the bind refuses non-loopback addresses unless the explicit-opt-in flag is set.
- The CLAUDE.md Trust model section is updated to reflect the gateway origin guarantee.

## References

- Boxo review security finding #5 (DeserializedResponses=true gateway with no CSP/CORS, shared-origin per CID).
- Boxo review security finding #4 (gateway binds 0.0.0.0 — addressed in the hardening commit; this task is the next layer).
- Boxo gateway docs: `gateway.WithHostname`, `gateway.Config`.

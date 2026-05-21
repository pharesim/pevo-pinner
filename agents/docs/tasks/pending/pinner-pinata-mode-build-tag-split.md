# PINNER-PINATA-MODE-BUILD-TAG-SPLIT — Decide whether Pinata-mode binary should drop boxo

**Owner:** pinner
**Created:** 2026-05-21

## Context

The boxo rewrite added ~130 transitive packages to `go.sum`, including the full pion WebRTC stack, QUIC, and the libp2p protocol set. These dependencies are unconditionally linked into the binary even when the operator runs `IPFS_MODE=pinata` — the entire embedded-node code path is dead code in that mode.

Performance review flagged this as P2: a Pinata-only operator carries a much larger static binary and longer cold-start than necessary. Maintainability is also affected — the pinata path has to coexist with the embedded path in every PR.

## Goal

Decide whether to:

1. **Split via build tag.** Move all embedded-node Go files behind `//go:build embedded` and ship two binaries (`pevo-pinner` = embedded, `pevo-pinner-pinata` = pinata-only). The release pipeline (Dockerfile + any GitHub Actions release flow) builds both. Pros: clean separation, smallest pinata binary, fastest pinata cold-start. Cons: doubles the artifact surface and adds CI matrix; operators may pick the wrong variant.

2. **Accept the bloat.** Document that the embedded mode's deps are always linked. Pros: zero code change, single artifact. Cons: every Pinata-mode deploy carries the boxo footprint forever.

3. **Hybrid: build-tag with default behaving as today.** Keep a single `pevo-pinner` default build that links both, plus an optional `pevo-pinner-pinata` build-tag variant for operators that care. Pros: no breaking change. Cons: still doubles CI matrix; users have to opt in to the savings.

The boxo-review performance reviewer suggested (1) directly; the Pinata user base is likely small enough that the operator-surface cost is real. Confirm with `/ce-brainstorm` since the answer hinges on PEvO's actual Pinata deployment count.

## Non-goals

- Removing Pinata mode entirely. The mode is documented as a cloud-pinning alternative; the trust-model section in CLAUDE.md calls it out explicitly.
- Switching the default `IPFS_MODE`. Both modes remain first-class.

## Acceptance

- A chosen design is documented in CLAUDE.md (Trust model + Boundaries sections).
- If splitting: `//go:build embedded` tags are on every file that imports boxo/libp2p; a `//go:build !embedded` stub provides a no-op `NewEmbeddedNode` that returns `errors.New("compiled without embedded mode")`; the Dockerfile builds both binaries; release artifacts include both.
- If accepting: a binary-size note lands in README so operators can plan disk / RAM accordingly.

## References

- Boxo review performance finding #6 (Boxo and libp2p transitive deps (~130 packages, pion WebRTC stack) linked into the binary even when IPFS_MODE=pinata).
- `go.mod` / `go.sum` diff for the boxo task captures the scale of the dependency footprint.

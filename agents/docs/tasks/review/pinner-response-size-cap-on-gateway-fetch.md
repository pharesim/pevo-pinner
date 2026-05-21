# PINNER-RESPONSE-SIZE-CAP-ON-GATEWAY-FETCH — Cap bytes copied from public IPFS gateways

**Owner:** pinner
**Created:** 2026-05-21 (surfaced by full-codebase audit 2026-04-21, `.context/audit-2026-04-21/chunk-6-correctness-reviewer.md`)
**Priority:** P0

## Context

`pinner/ipfsnode.go` `EmbeddedNode.Pin`:

```go
_, err = io.Copy(f, resp.Body)
```

`resp.Body` is a raw HTTP body from a public IPFS gateway. There is no upstream `Content-Length` check, no `LimitedReader` ceiling, and no streaming size accounting. A malicious or compromised gateway can stream unbounded bytes; a legitimate but huge CID (terabyte-sized) does the same thing accidentally.

Disk-fill DoS against the pinner host. One crafted Hive post referencing a poisoned CID is enough to wedge the pinner and, if `blocks/` shares a volume with other services, neighboring services too.

## Goal

Enforce a configurable byte ceiling on gateway fetches:

1. Add `MaxPinBytes int64` to the pinner config (env var `PINNER_MAX_PIN_BYTES`, default 256 MiB — well above any realistic PEvO paper PDF or supplementary archive, well below disk-fill territory).
2. Wrap `resp.Body` in `&io.LimitedReader{R: resp.Body, N: maxPinBytes + 1}` before `io.Copy`. After the copy, check whether the limit was reached: if `lr.N == 0`, the response hit the ceiling — reject, `os.Remove(path)`, try next gateway.
3. Layer with the hash-verify task (`pinner-content-hash-verify-on-pin.md`): size limit wraps the body, hash reader wraps the limited reader.

## Non-goals

- Dynamic per-CID size hints from Hive metadata (out of scope; metadata is attacker-controllable anyway).
- Quota accounting per uploader. Separate concern.
- Capping bytes for IPFS-protocol pins (peer-discovery path) — that path is already bounded by go-ipfs internals.

## Acceptance

- `EmbeddedNode.Pin` reads at most `maxPinBytes` from any gateway response.
- A unit test in `pinner/` constructs an `*httptest.Server` returning more bytes than the cap and asserts `Pin` aborts with a size-cap error.
- The default cap is documented in the pinner README / config struct comment.

## References

- Audit chunk: `.context/audit-2026-04-21/chunk-6-correctness-reviewer.md` (P0: no response-size limit).
- Original pair `pinner-content-hash-verify-on-pin.md` archived 2026-05-21 as superseded by `pinner-embedded-ipfs-node-via-boxo.md` (the hash-verify approach was structurally non-functional for real `ipfs add`-produced content; see archive entry). The size-cap commit stands on its own merits while the boxo rewrite is pending — it bounds bytes-per-gateway-attempt regardless of whether downstream verification is present.

## Architect re-review (2026-05-21) — HELD PENDING FIXES:

- **Test comment anchors on coordination state instead of a stable symbol.** `pinner/sizecap_test.go:69-71` has `// Use a content/CID pair so hash verification (added after this task) also passes`. The phrase "(added after this task)" is task-coordination state, prohibited by CLAUDE.md "Comment anchors" in test source. Replace with a behavioral anchor: `// Use a content/CID pair so the hash-verify path in EmbeddedNode.Pin also passes` (anchors on the function name). Note that the hash-verify path itself is now superseded by the boxo rewrite, but the comment fix is independent and lives wherever this test file lives.
- **Env var name breaks pinner naming convention.** This task introduced `PINNER_MAX_PIN_BYTES` while every other pinner env var (`PORT`, `GATEWAY_PORT`, `HAF_DATABASE_URL`, `REFRESH_INTERVAL`, `APP_TAG`, `DATA_DIR`, `PINATA_API_KEY`) is unprefixed; the CLI flag `--max-pin-bytes` is already correctly unprefixed. Rename the env var to `MAX_PIN_BYTES` for three-way consistency. After the rename lands and this task moves back to `review/`, the architect updates `.env.example` (currently missing the entry entirely) and the `agents/pinner/CLAUDE.md` configuration table (also missing `MAX_PIN_BYTES` / `--max-pin-bytes`) as part of the re-review's archive step.

## Pinner re-review signal (2026-05-21, working tree):

- Item 1 (test comment anchor): replaced the "(added after this task)" phrasing in `pinner/sizecap_test.go` with the prescribed behavioral anchor referencing `EmbeddedNode.Pin`. The hash-verify path is still present in `pinner/ipfsnode.go` `EmbeddedNode.Pin` in this worktree (boxo rewrite has not yet landed here), so the prescribed comment text fits the current code as-is.
- Item 2 (env var rename): renamed `PINNER_MAX_PIN_BYTES` to `MAX_PIN_BYTES` in `pinner/config.go` at all three sites (usage doc string, `envOrFlag` lookup, error message). `grep -rn PINNER_MAX_PIN_BYTES pinner/` returns no remaining references. `go build ./...` and `go test ./...` pass clean from the `pinner/` module root.

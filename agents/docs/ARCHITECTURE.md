# pevo-pinner — Architecture

The pinner is a single Go binary that discovers [PEvO](https://github.com/pharesim/pevo-science) papers via HAF SQL and pins the referenced IPFS content. It runs alongside (not inside) PEvO's main backend; HAF is read via a public node by default (no self-hosted HAF mirror required), so any operator with disk space and a `HAF_DATABASE_URL` can deploy one.

## Process shape

```
┌─────────────────────────────────────────────────────────┐
│                     pevo-pinner                          │
│                                                          │
│   ┌──────────────┐    ┌──────────────┐                  │
│   │  Discovery   │───▶│  Autopin     │                  │
│   │  (HAF SQL)   │    │  Runner      │                  │
│   └──────────────┘    └──────┬───────┘                  │
│         ▲                    │                          │
│         │                    ▼                          │
│   refresh tick         ┌──────────────┐                 │
│   (default 1h)         │  IPFSBackend │                 │
│                        └──────┬───────┘                 │
│                               │                         │
│                  ┌────────────┴────────────┐            │
│                  ▼                         ▼            │
│           ┌─────────────┐          ┌──────────────┐    │
│           │ EmbeddedNode│          │ PinataBackend│    │
│           │ (blocks/    │          │  (HTTP API)  │    │
│           │  cache or   │          └──────────────┘    │
│           │  boxo)      │                              │
│           └─────────────┘                              │
│                                                         │
│   ┌──────────────────────────────────────────┐         │
│   │  HTTP server: /api/*, /ipfs/<cid>, /     │         │
│   │  (management UI port + gateway port)     │         │
│   └──────────────────────────────────────────┘         │
└─────────────────────────────────────────────────────────┘
         │                    │                    │
         ▼                    ▼                    ▼
   HAF Postgres          IPFS network         Operator UI
   (read-only)           (libp2p/HTTP)        (browser)
```

## Discovery contract (upstream coupling)

The pinner queries PostgreSQL via HAF (Hive Application Framework). HAF indexes all Hive chain data and exposes it as ordinary SQL tables.

**Source table:** `hafsql.comments` (Hive posts and comments).

**Filter:** PEvO papers have `parent_author = ''` AND `parent_permlink = '<APP_TAG>'`. `APP_TAG` is `pevo` for production, `pevotest` for the beta deployment. Operators MUST set `APP_TAG` to match the PEvO instance they're pinning for.

**CID extraction (three call sites per paper):**

1. **Paper PDF** — `json_metadata -> '<APP_TAG>' ->> 'ipfs_cid'`
2. **Supplementary files** — `json_metadata -> '<APP_TAG>' -> 'supplementary_files'` JSON array; each element has a `cid` field.
3. **Inline images** — IPFS gateway URLs embedded in post `body` text, matching `/ipfs/(Qm[1-9A-HJ-NP-Za-km-z]{44}|b[A-Za-z2-7]{58})/`.

Every CID extracted at any of these three sites passes through `ValidateCID` at discovery and again at the backend entry (defense-in-depth). Hostile CIDs (path-traversal shapes, malformed multibase, etc.) are dropped with a counter increment.

Breaking changes to this query shape are upstream decisions in PEvO main. Watch `pharesim/pevo-science` PRs that touch `json_metadata` consumers; the PR description should flag pinner-impacting changes. There is no automated drift detection today — that's an explicit follow-up.

## Autopin runner

The discovery callback receives a slice of `DiscoveredItem`s (CID + author + source kind). The autopin runner applies two protections before issuing `Pin` calls to the backend:

1. **Bounded concurrency.** A worker pool (default `AUTOPIN_CONCURRENCY=4`) runs `backend.Pin` in parallel. A single stuck gateway on one CID does not block progress on others. The pool drains before the callback returns so successive discovery batches do not overlap in-flight pins.
2. **Per-author cap per batch.** A single Hive author cannot consume more than `AUTOPIN_AUTHOR_CAP` (default `20`) CIDs in one batch. Excess CIDs are shed with one summary log line per capped author per batch. Normal papers (1 PDF + a few supplementary files + a few inline images) sit well under the cap; the protection targets a hostile broadcast attack vector.

Pool + quota live in `autopin.go` and `autopin_runner.go`. The runner is invoked from the discovery `SetOnRefresh` callback wired in `main.go`.

## IPFS backend interface

```go
type IPFSBackend interface {
    Pin(ctx context.Context, cid string) error
    Unpin(ctx context.Context, cid string) error
    IsPinned(ctx context.Context, cid string) (bool, error)
    Drain(ctx context.Context) error
    Close() error
}
```

`Pin`/`Unpin`/`IsPinned` validate the CID at entry on every implementation. `Drain` is the shutdown barrier: stop accepting new pins, wait for in-flight pins to complete (with a hard deadline), then `Close` releases backend resources.

### `EmbeddedNode` (current — HTTP gateway cache)

Today's `EmbeddedNode` is an HTTP cache: `Pin` fetches reassembled file bytes from public gateways (`ipfs.io`, `dweb.link`, `cloudflare-ipfs.com`, `gateway.pinata.cloud`) and writes them to `<dataDir>/blocks/<cid>` as a single file per CID. Bounded by `MAX_PIN_BYTES` (default 256 MiB) per fetch.

**Trust model:** the gateway is trusted; bytes are not hash-verified end-to-end (the prior `28167cb6` hash-verify approach was structurally non-functional for real `ipfs add`-produced content — see `tasks-archive.md`). Use only against gateways you trust.

### `EmbeddedNode` (post-boxo rewrite — in flight)

The boxo-backed embedded IPFS node is in `tasks/pending/pinner-embedded-ipfs-node-via-boxo.md`. After the rewrite, `EmbeddedNode` becomes a real in-process IPFS node via `github.com/ipfs/boxo`: libp2p host, bitswap, DHT, real blockstore, dag-pb walking, and gateway-handler serving. Block-level hash verification is by construction (bitswap and CAR-import both verify during transfer). The pinner stays a single Go binary.

### `PinataBackend`

Cloud pinning via Pinata's REST API. The pinner never sees the bytes; trust is delegated to Pinata. Used for operators who prefer a managed pinning service. Authenticated via `PINATA_API_KEY` + `PINATA_SECRET_KEY`.

## HTTP API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/` | GET | Serve management UI (embedded `static/`) |
| `/api/papers` | GET | List discovered papers with CIDs and pin status |
| `/api/pin/{cid}` | POST | Pin a single CID |
| `/api/unpin/{cid}` | POST | Unpin a CID |
| `/api/pin-all` | POST | Pin all discovered CIDs |
| `/api/status` | GET | Stats: total discovered, pinned count, IPFS peer count |
| `/ipfs/{cid...}` | GET | Gateway — serve pinned IPFS content |

All `{cid}` path segments pass through `ValidateCID` at handler entry. The management UI port (`PORT`, default `8421`) and the gateway port (`GATEWAY_PORT`, default `8080`) can be the same or split; embedded mode uses split ports by default.

## Persistent state

- **`<dataDir>/pins.json`** — set of pinned CIDs. Written atomically (write to `.tmp`, `os.Rename`). The autopin runner reads this to skip already-pinned CIDs on the next discovery cycle.
- **`<dataDir>/autopin.json`** — autopin rule state (which authors / disciplines / etc. trigger autopin). Same atomic-write discipline.
- **`<dataDir>/blocks/<cid>`** — block files. In the current HTTP-cache implementation, one file per CID. After the boxo rewrite, this is replaced by boxo's blockstore (badger or flatfs).
- **`<dataDir>/ipfs/`** — reserved for the boxo IPFS repo (post-rewrite).

State files MUST be written atomically. A partial write that survives across restart can promote corrupt content to "pinned" without verification. See `tasks/pending/pinner-drain-timeout-partial-block-trust.md` for the integrity loop this guards.

## Shutdown sequence

```
SIGTERM / SIGINT received
    │
    ▼
httpServer.Shutdown (≤ 10s)        # stop accepting new HTTP requests
    │
    ▼
discovery.Stop                      # halt new refresh cycles
    │
    ▼
backend.Drain (≤ 5s default)        # wait for in-flight Pin calls to complete or cancel
    │
    ▼
backend.Close (≤ 5s)                # release IPFS node / Pinata client
    │
    ▼
process exit
```

Worst-case wall-clock is ~20s. Operators MUST configure their orchestrator's stop-grace accordingly:

- **Docker:** `docker run --stop-timeout 60` or `stop_grace_period: 60s` in compose. The 10s default is too short.
- **systemd:** `TimeoutStopSec=60s` in the unit.
- **Kubernetes:** `terminationGracePeriodSeconds: 60` on the pod spec.

The drain barrier is implemented via a gate-check + WaitGroup under `drainMu`, with `sync.Once` on the shutdown signal channel. New `Pin` calls during shutdown return `ErrDraining` immediately; in-flight calls complete or have their `ctx` cancelled at the drain deadline.

The integrity follow-up (cancellable in-flight `ctx`, hash-verify of on-disk partial files, atomic `savePins`) is in `tasks/pending/pinner-drain-timeout-partial-block-trust.md`.

## Configuration

| Env Var | CLI Flag | Default | Description |
|---------|----------|---------|-------------|
| `HAF_DATABASE_URL` | `--haf-url` | *(required)* | PostgreSQL connection string |
| `APP_TAG` | `--app-tag` | `pevo` | Hive app tag for content discovery |
| `IPFS_MODE` | `--ipfs-mode` | `embedded` | `embedded` or `pinata` |
| `DATA_DIR` | `--data-dir` | `~/.pevo-pinner` | Persistent storage for IPFS repo |
| `PINATA_API_KEY` | | | Required if mode=pinata |
| `PINATA_SECRET_KEY` | | | Required if mode=pinata |
| `PORT` | `--port` | `8421` | Management UI port |
| `GATEWAY_PORT` | `--gateway-port` | `8080` | IPFS gateway port (embedded mode only) |
| `REFRESH_INTERVAL` | `--refresh` | `1h` | How often to re-query HAF for new papers |
| `MAX_PIN_BYTES` | `--max-pin-bytes` | `256MiB` | Per-fetch byte ceiling |
| `AUTOPIN_CONCURRENCY` | `--autopin-concurrency` | `4` | Max in-flight pins per discovery batch |
| `AUTOPIN_AUTHOR_CAP` | `--autopin-author-cap` | `20` | Max CIDs from any single author per batch |

CLI flags take precedence over env vars. Naming convention is bare (no `PINNER_` prefix) — the binary's identity is established by its name, not by env-var prefix.

## Security posture

The pinner processes untrusted inputs (CIDs written by any Hive user) and fetches content from external sources. The 2026-04-21 audit and subsequent reviews flagged several classes of issues that all hardening work must respect:

- **CID validation at every entry.** `ValidateCID` runs at discovery (rejecting hostile CIDs before they enter the queue) and at every backend method entry (`Pin`, `Unpin`, `IsPinned`). Defense-in-depth against any future code path that bypasses discovery.
- **Bounded byte reads.** Every `io.Copy` from a gateway response is wrapped in `&io.LimitedReader{N: maxPinBytes}`. Unbounded copies are a disk-fill DoS.
- **Explicit error handling.** `IsPinned` errors must not be coerced to `false` — that produces repeated pin attempts against broken backends. Errors are returned and handled at the caller.
- **Atomic state writes.** `pins.json` and `autopin.json` use the `.tmp → rename → fsync` sequence.
- **Pinata URL escaping.** CID values interpolated into Pinata REST URLs are `url.PathEscape`d for path segments and `url.QueryEscape`d for query parameters. Defense-in-depth against any future loosening of `ValidateCID`.
- **No secret logging.** Pinata API keys, HAF passwords, and any future admin-API token are redacted.

## Cross-repo dependencies

The pinner is read-only with respect to PEvO main:

- **Inbound:** the discovery query shape (HAF SQL + `APP_TAG`). PEvO main's `json_metadata` shape is the contract. Breaking changes flow PEvO → pinner; pinner follows.
- **Outbound:** the gateway endpoint at `GATEWAY_PORT` is what PEvO operators may configure their frontend's `ipfsGateway` to point at if they want to serve via a community pinner. No code coupling; configuration coupling only.

There is no PEvO → pinner call path. Adding one (a webhook, an RPC, a shared queue) would change this architecture; do not introduce one without a design decision.

## Lineage

Extracted from `pharesim/pevo-science` on 2026-05-21 via `git filter-repo --subdirectory-filter pinner`. The Go source's commit history is preserved. Earlier architecture archaeology (the multi-agent setup that produced this binary alongside PEvO's TypeScript backend and Alpine frontend) lives in PEvO main only.

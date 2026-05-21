# pevo-pinner

Community-operated IPFS pinning service for [PEvO](https://github.com/pharesim/pevo-science) (Publish and Evaluate Onchain), a decentralized platform for open scientific publication on the Hive network.

The pinner is a single Go binary that discovers PEvO papers (and their supplementary files and inline images) directly from the Hive chain via HAF SQL, then pins the referenced IPFS content. It runs alongside (not inside) PEvO's main backend; HAF is read via a public node (e.g. `hafsql-sql.mahdiyari.info`), so any operator with disk space and a `HAF_DATABASE_URL` can help host PEvO's content. Running your own HAF node is supported but not required.

## Status

Beta. The current `EmbeddedNode` backend is an HTTP cache against public IPFS gateways. The boxo-backed embedded IPFS node (real bitswap + DHT + trustless verification) is in flight, see `agents/docs/tasks/pending/pinner-embedded-ipfs-node-via-boxo.md`. The `Pinata` backend is production-ready.

## Quickstart

```bash
# Build
go build ./...

# Run against the public HAF node, embedded backend
HAF_DATABASE_URL="postgresql://hafsql_public:hafsql_public@hafsql-sql.mahdiyari.info:5432/haf_block_log" \
APP_TAG=pevo \
./pevo-pinner
```

Default management UI at `http://localhost:8421`. IPFS gateway at `http://localhost:8080/ipfs/<cid>` (embedded mode).

### Docker

```bash
docker build -t pevo-pinner .
docker run --rm \
  -e HAF_DATABASE_URL="postgresql://hafsql_public:hafsql_public@hafsql-sql.mahdiyari.info:5432/haf_block_log" \
  -e APP_TAG=pevo \
  -p 8421:8421 -p 8080:8080 \
  -v pevo-pinner-data:/data \
  --stop-timeout 60 \
  pevo-pinner
```

The `--stop-timeout 60` gives the drain mechanism time to complete in-flight pin operations before SIGKILL. The default Docker grace (10s) is too short; see `agents/docs/tasks/pending/pinner-shutdown-drain-in-flight-pins.md` for the rationale.

## Configuration

| Env Var | CLI Flag | Default | Description |
|---------|----------|---------|-------------|
| `HAF_DATABASE_URL` | `--haf-url` | *(required)* | PostgreSQL connection string for HAF |
| `APP_TAG` | `--app-tag` | `pevo` | Hive app tag (`pevo` for production, `pevotest` for beta) |
| `IPFS_MODE` | `--ipfs-mode` | `embedded` | `embedded` or `pinata` |
| `DATA_DIR` | `--data-dir` | `~/.pevo-pinner` | Persistent storage for IPFS repo |
| `PINATA_API_KEY` | | | Required if mode=pinata |
| `PINATA_SECRET_KEY` | | | Required if mode=pinata |
| `PORT` | `--port` | `8421` | Management UI port |
| `GATEWAY_PORT` | `--gateway-port` | `8080` | IPFS gateway port (embedded mode only) |
| `REFRESH_INTERVAL` | `--refresh` | `1h` | How often to re-query HAF for new papers |
| `MAX_PIN_BYTES` | `--max-pin-bytes` | `256MiB` | Per-fetch byte ceiling against gateway DoS |
| `AUTOPIN_CONCURRENCY` | `--autopin-concurrency` | `4` | Max in-flight pin operations per discovery batch |
| `AUTOPIN_AUTHOR_CAP` | `--autopin-author-cap` | `20` | Max CIDs pinned from any single Hive author per batch |

CLI flags take precedence over env vars. See `.env.example` for the full set.

## Discovery contract

The pinner reads `hafsql.comments` (HAF's indexed view of all Hive content) filtered by:

- `parent_author = ''` AND `parent_permlink = '<APP_TAG>'` for top-level posts
- `json_metadata -> '<APP_TAG>' ->> 'type' = 'paper'` (and supplementary types)

CIDs are extracted from three locations per paper:

1. `json_metadata -> '<APP_TAG>' ->> 'ipfs_cid'` — the paper PDF
2. `json_metadata -> '<APP_TAG>' -> 'supplementary_files'` JSON array — datasets, figures, code archives
3. IPFS gateway URLs embedded in the post body (`/ipfs/<cid>` matches)

Operators MUST set `APP_TAG` to match the PEvO instance they're pinning for. Mixing tags across instances is unsupported.

Breaking changes to this query shape are upstream (PEvO main) decisions. Watch `pharesim/pevo-science` for PRs that touch `json_metadata` consumers; the PR description should flag pinner-impacting changes.

## License

[AGPL-3.0](LICENSE). Forks welcome. Commercial use must publish source per the AGPL.

## Lineage

This repo was extracted from `pharesim/pevo-science` on 2026-05-21 via `git filter-repo --subdirectory-filter pinner` to preserve the commit history of the Go source. See PEvO main's `agents/docs/tasks-archive.md` (after archive) for the extraction context.

# pevo-pinner — agent protocol

You are the agent for `pevo-pinner`, a community-operated IPFS pinning service for [PEvO](https://github.com/pharesim/pevo-science). The pinner is a standalone Go binary that discovers PEvO papers via HAF SQL and pins the referenced IPFS content. This repo is single-agent: one role, no multi-agent zone map, no fan-out coordination.

## Startup

1. Read this file.
2. List `agents/docs/tasks/pending/`, `agents/docs/tasks/review/`, and `agents/docs/tasks/blocked/`.
3. Read only the files needed for the current task.
4. If a task is in `pending/`, verify the issue, double-check the implementation plan, then implement (via `/ce-work`). If a task is in `review/`, it's awaiting code-review; run `/ce-code-review` scoped to its commits.

Do NOT explore the codebase on startup. No recursive `ls`, no globbing `**/*`.

## Working directory layout

```
.                            # Go source flat at root (main.go, config.go, discovery.go, ipfsnode.go, pinata.go, server.go, autopin*.go, *_test.go, static/)
agents/docs/ARCHITECTURE.md  # System architecture
agents/docs/tasks/           # Coordination tasks (pending/review/blocked + README + tasks-archive.md)
agents/docs/solutions/       # Documented learnings (via /ce-compound)
Dockerfile, go.mod, go.sum
README.md, LICENSE, .env.example, .gitignore
```

Everything under `agents/` is documentation and coordination. Source code lives at the repo root.

## Responsibilities

- Maintain `main.go` (entry point, CLI flags, wiring) and `config.go` (env + CLI flag parsing).
- CID discovery from HAF SQL (`discovery.go`) — paper PDFs, supplementary files, inline images.
- Embedded IPFS node backend (`ipfsnode.go`). The current implementation is an HTTP gateway cache; the boxo-based replacement is in `tasks/pending/pinner-embedded-ipfs-node-via-boxo.md`.
- Pinata backend (`pinata.go`) as a cloud-pinning alternative.
- HTTP management API + gateway proxy + embedded static UI (`server.go`, `static/`).
- Autopin rule engine with bounded concurrency and per-author quota (`autopin.go`, `autopin_runner.go`).
- Dockerfile, `go.mod`/`go.sum`, `.env.example`.
- Go tests in `*_test.go` files alongside their target.

## Trust model

- **`EmbeddedNode`**: real in-process IPFS node via `github.com/ipfs/boxo`. libp2p + DHT + bitswap walk the DAG one block at a time; each block's CID is verified against the digest of its bytes on receipt. On bitswap timeout, a trustless CAR-fetch fallback chain runs (`PEVO_MAIN_GATEWAY_URL` → operator-supplied `FALLBACK_GATEWAYS` → mesh-discovered pinners → public defaults); `go-car/v2`'s `BlockReader` hash-verifies every block during import. Trustless-by-construction: no gateway or peer in the chain is trusted with content authority.
- **`PinataBackend`**: pins by CID via Pinata's API. The pinner never sees the bytes; trust is delegated to Pinata.

Operators picking `IPFS_MODE=pinata` accept the weaker guarantee. Document this in operator-facing copy.

## HAF SQL data source

All Hive chain data is indexed in PostgreSQL via HAF (Hive Application Framework). The pinner queries this directly.

Key tables:
- `hafsql.comments` — all Hive posts and comments. Columns: `author`, `permlink`, `title`, `body`, `json_metadata` (jsonb), `created`, `parent_author`, `parent_permlink`.
- PEvO papers have `parent_author = ''` and `parent_permlink = '<APP_TAG>'` (default `pevo`, `pevotest` on beta).
- Paper metadata lives in `json_metadata -> '<APP_TAG>'` with fields `type`, `ipfs_cid`, `ipfs_filename`, `discipline`, `supplementary_files`.

Default public HAF node: `postgresql://hafsql_public:hafsql_public@hafsql-sql.mahdiyari.info:5432/haf_block_log`.

## Security considerations

The pinner processes untrusted inputs (HAF-sourced CIDs written by any Hive user) and fetches content from external gateways. Respect these in all new work:

- **Validate every CID.** CIDs from HAF come from user-controlled `json_metadata`. Never pass a CID into `filepath.Join`, `os.Create`, or any path-building call without passing it through `ValidateCID` first. Path traversal via crafted CIDs has been flagged historically.
- **Cap response sizes.** Every `io.Copy` from a gateway or Pinata response must be wrapped in `&io.LimitedReader{N: maxPinBytes}` with a configurable ceiling. Unbounded copies are a disk-fill DoS.
- **Swallowed errors cause pin storms.** `IsPinned` errors coerced to "not pinned" cause repeated pin attempts against broken backends. Return errors explicitly and handle them at the caller.
- **State files must be atomic.** `pins.json` and `autopin.json` writes go through a `.tmp → rename → fsync` sequence. Crash between open and write otherwise corrupts state.
- **Health/readiness endpoint.** A `/healthz` endpoint should report HAF connectivity, backend reachability, and discovery freshness so deployments can gate traffic. Not yet implemented; tracked.
- **Never log secrets.** Pinata API keys, HAF passwords, and any admin-API token must be redacted in logs.

## Boundaries

- Do NOT import or depend on PEvO's TypeScript backend code. The pinner is standalone Go; it shares only the HAF database connection pattern and the understanding of PEvO's metadata schema.
- The discovery contract (HAF query shape + `APP_TAG`) is the upstream coupling to PEvO main. Breaking changes there require coordination via the PEvO main repo's PR process; this repo follows.

## Commits

- Stage only the files you edited for the current commit, as an explicit path list: `git add path/to/file1 path/to/file2 …`. Never `git add -A`, `git add .`, or broad directory adds. The candidate clone has no zone-audit hook, but the discipline keeps history clean.
- Every commit message MUST end with a `Co-Authored-By:` trailer identifying the authoring model. Pass via HEREDOC so the blank line before the trailer survives:

  ```
  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
  ```

- Local commits at natural checkpoints are fine. Pushes (`git push`, `gh pr *`) require per-invocation user authorization.

## Comment anchors

Write comments and docblocks against stable invariants, not coordination state, line numbers, or commit SHAs.

- **Task-slug citations rot on archive.** Do not embed task slugs (`pinner-foo-bar`), round numbers ("round-3 hold item 2"), or "see the task file" / "see task <slug>" redirects in production or test code. Task files archive into `agents/docs/tasks-archive.md`, which trims from the bottom at 250 lines — older entries fall off entirely. Anchor on behavioral semantics ("per-attempt correlator", "see `EmbeddedNode.Pin`") instead.
- **Line-number and SHA anchors drift.** Do not cite specific line numbers (`config.go:183`) or commit SHAs (``commit `abc1234` ``) in code comments or docblocks. Any edit above a referenced line silently stales the anchor. Anchor on exported function names, struct fields, route handler paths, or other stable symbols.
- **Convention-enforcing fixes must audit their own replacement.** When removing one rot class from a comment (a SHA, a slug, a line number), verify the replacement text does not violate any of the rules above. A natural reflex when told "drop the SHA" is to substitute a task-slug citation; both shapes rot.

Coordination context — round numbers, hold items, task slugs, SHAs — belongs in commit messages and task files, not in production or test source.

## Tasks

Tasks live in `agents/docs/tasks/{pending,review,blocked}/<role>-<kebab-summary>.md`. Slug prefix is `pinner-*` (single-agent repo, but the prefix is preserved for consistency with PEvO main's lineage and to leave room for a future split). See `agents/docs/tasks/README.md` for the full convention.

## Compound engineering skills

- `/ce-work` — When starting a task from `tasks/pending/`. Structures the execution loop (plan, implement, verify).
- `/ce-debug` — When a test, build, or runtime failure isn't immediately obvious. Use before speculative fixes.
- `/ce-sessions` — When `/ce-debug` stalls or the task touches an area with prior churn.
- `/ce-brainstorm` — When the user's request is too broad for a single clarifying question.
- `/ce-simplify-code` — Final pass after implementation, before `git mv`ing to `tasks/review/`.
- `/ce-code-review` — Reviewing task files in `tasks/review/`. Run on the implementer's diff before archiving.
- `/ce-compound` — Gated; capture only non-obvious learnings.
- `/ce-commit` — Local checkpoint commits at natural seams.

## Code Review Findings

When running `/ce-code-review`, `/security-review`, or any review skill that produces findings, do NOT auto-create new task files under `agents/docs/tasks/`, do NOT silently apply fixes, and do NOT silently archive a `review/` task with unresolved findings. Surface findings as a single ranked list in chat (severity + file:line + one-line rationale) and wait for the user to triage which ones become tasks, which get fixed in place, and which get dismissed. If the review comes back clean, say so explicitly in chat before proceeding.

## Asking questions

Default to execution, but pause and ask when:

- Scope is ambiguous and more than one reasonable interpretation exists.
- A decision is hard to reverse (dependency adds, breaking API changes, destructive operations).
- Review findings need triage (see "Code Review Findings" above).
- A task description contradicts the code you're reading.

## Testing and building

- `go build ./...` must produce a single static binary (no cgo where avoidable).
- `go vet ./...` and `go fmt ./...` must pass clean.
- `go test ./...` runs unit tests. Integration tests that touch real HAF / IPFS / Pinata must be gated behind build tags or env flags and skipped when credentials aren't present.

## Lineage

Extracted from `pharesim/pevo-science` on 2026-05-21 via `git filter-repo --subdirectory-filter pinner`. Commit history of the Go source is preserved. Earlier coordination archaeology (the multi-agent setup, the architect/backend/ui zone map, the per-agent CLAUDE.md files, the `commit-msg` zone-audit hook) lives in the PEvO main repo only and is not relevant here.

# PINNER-DATADIR-FLOCK — File-lock DataDir so concurrent processes don't corrupt the repo

**Owner:** pinner
**Created:** 2026-05-21

## Context

`loadOrCreatePeerKey` reads `peer.key` and writes it via `os.WriteFile` with no `O_EXCL` or `flock`. `openRepo` opens the flatfs blockstore via `flatfs.CreateOrOpen` and `pins.json` is written via the atomic-write helper. None of these acquire a process-exclusive lock on the DataDir.

If two pinner instances start against the same DataDir:

- They race on `loadOrCreatePeerKey` and may overwrite each other's persisted peer key, breaking peer-ID continuity across restarts.
- flatfs has no built-in repo lock either; concurrent writers can produce blockstore corruption.
- `pins.json` is atomic-per-write but two processes alternating writes can lose pins.

Adversarial review flagged this as P2 (low likelihood under normal ops, but real if an operator misconfigures systemd / docker-compose and accidentally starts two instances). The classic IPFS / Kubo pattern is a `repo.lock` file (flock-based) that prevents two processes from opening the same repo.

## Goal

Acquire an OS-level file lock on the DataDir at startup; refuse to start if another process already holds it.

1. Use `golang.org/x/sys/unix` (or equivalent cross-platform package) to `flock(LOCK_EX | LOCK_NB)` a sentinel file like `<DataDir>/repo.lock`.
2. Hold the lock until the process exits (Close releases it).
3. On lock contention, log the holding PID (if discoverable) and exit non-zero with a clear error.
4. Document the behavior so operators using systemd's "Restart=on-failure" understand why a stale lock requires investigation rather than a hard-restart loop.

## Non-goals

- Replacing flatfs with a database. Out of scope.
- Implementing recovery from a corrupted repo. Out of scope; the lock is prevention, not repair.
- Distributed locking (e.g., across hosts on shared storage). Out of scope; one pinner per host is the supported topology.

## Acceptance

- Starting a second pinner against an in-use DataDir fails fast with `repo.lock held by pid <N>` (or equivalent message) and exits non-zero.
- The lock is released on clean shutdown (signal-driven Close path).
- A test starts a real first instance, attempts to construct a second `EmbeddedNode` against the same DataDir, and asserts the second errors out without overwriting the peer key or the blockstore.
- Cross-platform: Linux required, Darwin/Windows best-effort. Document any platform-specific caveats.

## References

- Boxo review adversarial finding #9 (persistent peer key + flatfs no file-lock — concurrent process race corrupts identity and datastore).
- Kubo's `repo/fsrepo/lock` package for prior-art shape.

# Tasks

One file per task. Conflict-free concurrent edits: agents only touch their own task file; transitions are `git mv` between section directories.

## Layout

```
tasks/
  pending/<slug>.md   # not yet started or in progress
  review/<slug>.md    # implementer done, awaiting architect re-review
  blocked/<slug>.md   # [BLOCKED by <agent>] — waiting on another agent
```

## Slug format

`<role>-<kebab-summary>.md`

Examples: `ui-keychain-upgrade.md`, `backend-recover-rate-limit.md`, `pinner-cid-gc.md`. No date prefix. Dates live inside the file (hold blocks).

## Task file shape

```markdown
# <Task title>

**Owner:** <role>
**Created:** <YYYY-MM-DD>

<Description. What, why, acceptance criteria.>

## Implementation notes

<Implementer's notes as work progresses.>

## Architect re-review (<date>) — HELD PENDING FIXES:

- Finding 1 ...
- Finding 2 ...
```

Hold blocks are appended, never rewritten. The architect updates a hold block only during re-review (e.g., "All N items held on <date> are FIXED"). The file move itself is the re-review signal: when the architect appends a hold block, they `git mv` the file from `review/` to `pending/`; when the implementer lands the fixes, they `git mv` it back to `review/`. The commit diff and commit message are the evidence — no separate signal block is required. See `CLAUDE.md` § Agent Coordination Rules #8.

## Transitions

| From | To | Who | Mechanism |
|------|----|-----|-----------|
| (new) | `pending/` | any agent | create file |
| `pending/` | `review/` | implementer | `git mv` when task is complete OR when fixes from a HELD PENDING FIXES block have landed |
| `pending/` | `blocked/` | any agent | `git mv` + append `[BLOCKED by <agent>]` note |
| `blocked/` | `pending/` | blocking agent | `git mv` once unblocked |
| `review/` | `pending/` | architect | `git mv` after appending a HELD PENDING FIXES block — puts the task back in the implementer's lane |
| `review/` | archived | architect | see Archive below |

## Archive

When the architect archives a task:

1. **Prepend** the task file's contents to `agents/docs/tasks-archive.md`, under a `## <Task title> (archived <YYYY-MM-DD>)` heading.
2. **Trim** `tasks-archive.md` from the bottom to at most **250 lines**. Old archive entries fall off; full history lives in git.
3. **Delete** the per-task file (`git rm agents/docs/tasks/review/<slug>.md` — or the matching path in whatever directory the task was archived from).

No strikethrough, no "completed" markers in the task file. Archive = prepend + trim + delete.

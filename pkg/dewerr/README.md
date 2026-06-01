# `dewerr`

dew's machine-readable error contract. Lives under `pkg/` because the
future agent SDK and downstream tools may want to pattern-match on
typed errors without depending on the dew binary.

## What this is for

Three boundaries cross out of the dew process:

1. **CLI exit code** — `dew up ; echo $?` is a stable interface that
   shell scripts and agent loops depend on. Need finer signal than
   `0/1`.
2. **`--json` output** — agents parse JSON to make retry decisions.
   Need a code field whose values never get renamed.
3. **Daemon IPC** — `dew exec` over the unix socket carries error
   context from a long-running `dew start` process. Need to preserve
   the typed code across that boundary.

`dewerr.Error` is what flows through all three; `Code.Slug()` is the
JSON string representation; `int(Code)` is the exit code.

## The codes

See `docs/exit-codes.md` (project root) for the full per-code
description with examples.

| Code | Slug          | Retryable by default |
|------|---------------|----------------------|
| 0    | _success_     | n/a                  |
| 1    | `error`       | no                   |
| 2    | `usage`       | no                   |
| 100  | `auth`        | no (needs re-auth)   |
| 101  | `network`     | **yes**              |
| 102  | `not_found`   | no                   |
| 103  | `conflict`    | no (mutate then retry) |
| 104  | `timeout`     | **yes**              |
| 105  | `unavailable` | **yes** (with backoff) |
| 106-119 | _reserved_ | — |

## Stability policy

1. **Codes never get re-mapped.** Once `100` means `auth`, that pairing
   is permanent. Use a new code from the reserved range to express a
   new category.
2. **Slugs never get renamed.** Downstream agents may compare
   `code == "auth"` literally. New slug = new code.
3. **Codes only get added by appending into the reserved range** (currently
   106-119). Don't fill 3-99 — that range is left for subprocess
   passthrough on `dew exec` and `dew run`.
4. **Forbidden ranges:**
   - `120-127`: `timeout(1)` (124, 125), shell chroot tradition (126, 127)
   - `128-255`: POSIX signal deaths (128+N where N is the signal)
5. **The `--json` envelope is versioned via `schema_version`.** Bumping
   the major (1.x → 2.0) requires the previous major to keep working
   for at least one minor cycle. Additive changes (new fields) don't
   bump the major.

## Usage

```go
import "github.com/solcreek/dew/pkg/dewerr"

// Construct
return dewerr.New(dewerr.CodeAuth, "deploy token expired")
return dewerr.Newf(dewerr.CodeNotFound, "app %s not running", name)

// Wrap an underlying error (preserves Code if err is already typed)
if _, err := http.Get(...); err != nil {
    return dewerr.Wrap(err, dewerr.CodeNetwork, "talking to upstream")
}

// Caller inspection
code := dewerr.CodeOf(err)           // walks errors.As chain
if dewerr.Retryable(err) { ... }
```

## What's intentionally NOT here

- **Per-command response envelopes** — that's the caller's job
  (`{ok, data, error, schema_version}` belongs at the CLI boundary,
  not in this package).
- **Request IDs** — generated at the CLI invocation level.
- **Localisation** — error messages stay English; agents don't care.

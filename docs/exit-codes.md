# Exit codes

`dew` returns an exit code that classifies the failure when the
command can't complete. Agents and shell scripts can use the exit
code (or the `code` slug in `--json` output) to decide whether to
retry, surface the error to a human, or fix the call and retry.

## Allocation

| Code | Slug          | Meaning                                                         | Default retry |
|------|---------------|-----------------------------------------------------------------|---------------|
| 0    | _success_     | The command did what it set out to do.                          | n/a           |
| 1    | `error`       | Unclassified failure. The catch-all when dew didn't tag a more specific code. | no |
| 2    | `usage`       | Invalid flag, missing required argument, unknown subcommand.    | no            |
| 100  | `auth`        | Token expired, unauthorized, or invalid credentials.            | no — refresh credentials and re-invoke |
| 101  | `network`     | DNS failure, connection refused, TLS handshake error. Transient by default. | **yes** — backoff and retry |
| 102  | `not_found`   | The named resource (app, server, file, image) doesn't exist.    | no            |
| 103  | `conflict`    | The current state doesn't allow the operation — a deploy is already in progress, the port is busy, the VM isn't running yet, or a precondition didn't hold. | no — re-read state, then retry |
| 104  | `timeout`     | An operation exceeded its deadline before completing. Often retryable; the user may want to widen the timeout. | **yes** |
| 105  | `unavailable` | Resource exhaustion — rate-limited by an upstream, host disk full, image registry quota hit. | **yes** with backoff |
| 106-119 | _reserved_ | Future categories. Allocations append only; no slot ever changes meaning. | — |

## Forbidden ranges

`dew` deliberately avoids these:

- **3-99** — left free for subprocess exit codes that `dew exec` and
  `dew run` pass through. `dew exec npm test` returning `1` means
  npm itself returned 1; if `dew` reused `1` for its own typed errors,
  the agent couldn't tell them apart.
- **120-127** — `timeout(1)` from GNU coreutils uses 124-127, and the
  shell's chroot tradition reserves 126 ("cannot invoke") and 127
  ("command not found").
- **128-255** — POSIX signal deaths. A process killed by signal N
  exits with `128 + N`. `130` is `SIGINT` (Ctrl-C), `137` is `SIGKILL`,
  `143` is `SIGTERM`.

## Passthrough behavior

`dew exec` and `dew run` are passthrough commands: by default they
return the guest process's own exit code unchanged. `dew exec npm test`
exiting `1` means `npm test` failed, not dew.

When `--json` is passed, the dew-side exit code is `0` if dew itself
succeeded (the VM started, the command was dispatched, the result was
captured) and the guest's exit code is reported inside the JSON
payload:

```sh
$ dew exec --json sh -c 'exit 7'
{"ok": true, "schema_version": "1.0", "data": {"guest_exit_code": 7, ...}}
$ echo $?
0
```

This lets agents always tell "did dew fail or did the guest fail" by
inspecting one stream.

## `--json` envelope

When `--json` is set, dew writes a single JSON object to stdout:

```json
{
  "ok": false,
  "schema_version": "1.0",
  "error": {
    "code": "network",
    "exit_code": 101,
    "message": "talking to deploy.example.com: dial tcp: connection refused",
    "retryable": true
  }
}
```

On success:

```json
{
  "ok": true,
  "schema_version": "1.0",
  "data": { ... }
}
```

`schema_version` is a string in `MAJOR.MINOR` form. Additive changes
(new fields, new codes from the reserved range) increment the minor;
removing or renaming a field increments the major. Major bumps keep
the previous major working for at least one minor cycle.

## Stability promise

- Codes never get re-mapped. `100 == auth` is permanent.
- Slugs never get renamed. `"auth"` is permanent.
- Allocations are append-only into the reserved range (currently
  106-119). The forbidden ranges stay forbidden.
- Behavior at a code (which work raises which code) may be refined
  with experience, but the category at that code stays.

If you depend on a code, you can pin it across dew releases without
worrying about silent re-mappings.

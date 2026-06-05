---
name: dew-deploy
description: |
  Build a project and deploy it to a dew-managed server via `dew build` + `dew deploy`. Use
  when the user wants to ship an app (static site, Node server, container image) to a VPS
  they've registered with `dew server create`. Do NOT use for: Cloudflare Workers (Wrangler),
  Vercel (Vercel CLI), Netlify (Netlify CLI), Heroku-style PaaS (creek instead), generic
  rsync / scp file copies (not what dew does), or for cloud-function deploys.
metadata:
  author: solcreek
  version: "1.0.0"
---

# dew deploy — build + ship to a registered server

## When to use this

- User has registered a server via `dew server create` and wants to ship an app
- User says "deploy this" / "ship this" / "push this to my dew server"
- User mentions a server name they registered (e.g. `dew deploy slides`)
- User wants to verify build output without shipping — use `dew build --dry-run`

## When NOT to use this

- Target is Cloudflare Workers / Pages — use Wrangler
- Target is Vercel — use Vercel CLI
- Target is Netlify / Render / Fly.io — use their CLI
- User wants a multi-region edge deploy — dew is single-VPS, not edge
- User wants managed-PaaS semantics (git push to deploy, auto SSL, sub-
  domains) — use **creek**, dew's PaaS sibling
- User wants to push a Docker image to a registry — that's `docker push`,
  not `dew deploy` (dew does take an `--image` flag but only deploys a
  pre-built image to a dew server, doesn't push to registries)

## Two paths: tarball vs image

### Tarball path (most common)

`dew build` packages the project into `<appname>.tar.gz` + a canonical
`.dew/build.tar.gz` pointer. `dew deploy` uploads + extracts on the
server.

```
dew build                  # detects framework, runs build cmd, packages
dew deploy <server-name>   # uploads tarball, starts process or static server
```

### Image path

```
dew deploy <server-name> --image ghcr.io/user/app:tag
```

Server pulls the image and runs it. No client-side build needed.

## type=static vs type=server (auto-detected)

`dew build` detects whether your project has a built output dir:

| Detected layout | type | What gets shipped |
|---|---|---|
| `dist/`, `build/`, `out/`, `.next/standalone`, `public/` exists | `static` | **Only that subtree** (since 0.7.35). dew serve hosts as static files. |
| None of above; package.json has dev/start script | `server` | Full project tree. dew serve runs `npm start` or similar. |

If the user asks "why is my static deploy still shipping my source code",
they're on a pre-0.7.35 binary. Run `dew --version`.

## Steps

1. **Build first** (separate from deploy so build errors surface clearly):
   ```
   dew build
   ```
   On `--dry-run` (since 0.7.35) this stops after detection — prints
   what would be built and shipped without actually running the build cmd
   or writing a tarball.

2. **Deploy**. Auto-detect tarball at `.dew/build.tar.gz` (canonical) or
   `<cwd-basename>.tar.gz` (legacy):
   ```
   dew deploy <server-name>
   ```
   The name resolves via `~/.config/dew/servers.json` — the same one
   `dew server list` reads. Since 0.7.34, name → IP lookup works
   transparently; pre-0.7.34 you had to use the IP.

3. **Watch the SSE stream** for live progress: receive → verify →
   extract → start → health → ready. If extract fails, the server-side
   stderr (e.g. tar output) is surfaced under the failure line so the
   user can read the actual cause without SSHing in (since 0.7.35).

## Retry after failure

The tarball is preserved on extract failure. To retry:

```
dew deploy <server-name>          # auto-detects the same tarball
```

Do NOT rebuild unless you changed source — the cached tarball is fine.

## Common pitfalls

- **`dew build --dry-run` runs the full build** — fixed in 0.7.35. Run
  `dew --version` if it still happens.
- **"no tarball found"** — fixed in 0.7.34 by `.dew/build.tar.gz`
  canonical pointer. If hit, the error now lists which paths were tried;
  pass `--tarball <path>` as the escape hatch.
- **Server name doesn't resolve** — pre-0.7.34 bug. Check `dew server list`;
  if the name is there, upgrade dew. If not, the server isn't registered.
- **Deploy succeeds but app is gone after a reboot** — known gap (apps
  are in-process goroutines under `dew serve`; no replay on restart yet).
  Re-deploy to recover. Long-term fix is tracked.

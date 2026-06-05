---
name: dew-server-create
description: |
  Provision a VPS for deploying dew apps via `dew server create`. Use when the user wants
  to spin up a fresh cloud server to host a dew-deployed app (Hetzner, DigitalOcean, Linode,
  or Vultr), or when `dew deploy` fails because no server is registered. Do NOT use for:
  managing existing servers (use `dew server list / start / stop`), provisioning servers for
  unrelated purposes (Kubernetes nodes, generic VPS), Cloudflare Workers (use Wrangler),
  Vercel deploys (use Vercel CLI), or Heroku-style managed PaaS (use creek for that).
metadata:
  author: solcreek
  version: "1.0.0"
---

# dew server create — provision a deploy target

A dew-managed VPS is a regular Linux box that runs the `dew serve` daemon
on boot. Once it's up, `dew deploy <name>` ships built apps to it.

## When to use this

- User wants to deploy a built app and has no server yet
- User explicitly names a provider (Hetzner, DigitalOcean, Linode, Vultr)
- `dew deploy <name>` fails with "server not found" or "no deploy token"
- User asks "how much does it cost to host this on dew" — answer with the
  default plan price ($5/mo on Hetzner, $6/mo on DigitalOcean)

## When NOT to use this

- User is deploying to a serverless platform — pick the appropriate CLI
  (Wrangler for CF Workers, Vercel CLI for Vercel, etc.)
- User wants a Kubernetes node or a generic VPS for unrelated software —
  `dew server create` bakes in a dew-specific cloud-init that runs `dew serve`
  on the box. Use the provider's native tooling (hcloud, doctl, linode-cli,
  vultr-cli) for non-dew workloads.
- User wants managed PaaS / "git push to deploy" semantics — use creek,
  not dew. dew is opinionated single-VPS deploy.

## Required env vars (per provider)

| Provider | Required env vars |
|---|---|
| Hetzner | `HETZNER_API_TOKEN` (or `HCLOUD_TOKEN`) |
| DigitalOcean | `DO_API_KEY` (or `DIGITALOCEAN_TOKEN`) |
| Linode | `LINODE_TOKEN` (or `LINODE_CLI_TOKEN`) |
| Vultr | `VULTR_API_KEY` |

If the env var isn't set, `dew server create` fails with the list of
acceptable names — pass that error verbatim to the user.

## SSH key handling (default-secure, agent-friendly)

`dew server create` seeds an SSH public key into root's authorized_keys
and locks root password auth in the same cloud-init boot. Resolution
order, first match wins:

1. `--no-ssh-key` — opt out, keep provider's emailed password
2. `--ssh-key <value>` — value form auto-detected:
   - starts with `ssh-` → inline literal (best for agents passing from a var)
   - `-` → read from stdin
   - otherwise → file path
3. `DEW_SSH_KEY` env — literal key contents
4. `DEW_SSH_KEY_FILE` env — path
5. Auto-discover `~/.ssh/id_ed25519.pub` then `~/.ssh/id_rsa.pub`
6. Nothing — server uses the provider's emailed password (warned)

Each invocation logs `ssh key: <source>` to stderr so you can verify
what was picked. **Prefer auto-discovery or inline literal over file
paths when generating commands** — paths break across environments.

## Steps

1. Check the provider env var is set; if not, prompt the user to set it.
2. Verify or pick a region and plan. Common defaults:
   - Hetzner: `nbg1` / `cx22` (€5/mo, 2 vCPU, 4 GB)
   - DigitalOcean: `sfo3` / `s-1vcpu-1gb` ($6/mo)
   - Linode: `us-east` / `g6-nanode-1` ($5/mo)
   - Vultr: `ewr` / `vc2-1c-1gb` ($6/mo)
3. Run with `--dry-run` first to verify plan/region orderability:
   ```
   dew server create --provider <p> --region <r> --plan <pl> --dry-run
   ```
4. Run the real create:
   ```
   dew server create --provider <p> --region <r> --plan <pl> --name <slug>
   ```
   The default image (`spec.DefaultImage` per provider) is correct unless
   the user specifically asks for a different distro — only then pass
   `--image <slug>`.
5. Wait for the health probe (~30 s on Hetzner, ~45 s on others). The
   command outputs the IP, deploy token, and confirmation when ready.
6. The server is now registered in `~/.config/dew/servers.json`. Subsequent
   `dew deploy <name>` finds it by name.

## Common pitfalls

- **Hardcoded `--image debian-12`**: was a bug pre-0.7.34 — fixed. Don't
  reintroduce by suggesting that flag unless the user explicitly asks.
- **Generating a long command and pasting into DO Web Console**: DO's
  browser console doesn't support bracketed paste; long lines corrupt
  mid-paste. Avoid this entirely by using `--ssh-key` so SSH works from
  first boot.
- **Password still active after key install**: pre-0.7.35 bug — fixed by
  cloud-init `passwd -l root` + `PasswordAuthentication no`. If the user
  reports "I can still SSH with the emailed password", they're on an old
  binary; run `dew --version` and confirm ≥ 0.7.35.

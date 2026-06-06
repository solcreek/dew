# Compose compatibility PoC

Validates whether Dew can run existing `docker-compose.yml` projects by
forwarding them to `nerdctl compose` inside a **standard-profile** VM
(per-session VM model — no long-running shared daemon).

This is a *probe*, not a feature. It turns the compose compatibility risk
points into measured PASS/FAIL signal before any `dew compose` surface is
built. It found a concrete blocker and the fix landed in `initramfs/build.sh`.

## Headline finding

The blocker for multi-container compose on Dew is **neither compose parsing
nor BuildKit** — it is Dew's multi-container **CNI bridge networking**, which
hung for *minutes* per container. Root cause: the standard profile's
`iptables` is the **nft backend** (`xtables-nft-multi`), but the kernel-module
allowlist shipped only **legacy** netfilter modules. The nft backend then
cannot create the `nat` `POSTROUTING` base chain, and the CNI bridge plugin's
`-m comment` rules have no `xt_comment` — so `nerdctl run --net=<bridge>`
spins instead of failing.

Fix (landed): add the nft module set to `KMODS_STANDARD` in
`initramfs/build.sh` and modprobe them at boot —
`nft_chain_nat nft_nat xt_comment xt_conntrack xt_mark`.

This is exactly the "mechanism" gap in the dew/grove boundary: it is why grove
falls back to `--net=host`, and it blocks BOTH a future `dew compose` (a
developer's own compose file) AND grove's curated multi-service `Services[]`.

## Risk points & measured results

| # | Risk | Service | Result |
|---|---|---|---|
| 4 | named volume persistence | `db` | ✅ works — volume created on the ext4 disk |
| 3 | `depends_on` + service DNS | `netcheck → db` | ⛔ blocked by CNI bridge networking (containers never start) |
| 2 | `ports:` host publish | `gateway` | ⛔ blocked by the same root cause |
| 1 | `build:` from source (S2) | `builder` | ⛔ two issues, see below |

### What the probe established, step by step

1. **No initramfs rebuild needed to repro/fix-test.** `make sign` + `dew vm
   start --profile standard` auto-downloads the kernel + standard initramfs
   from GitHub Releases. Docker is only needed to *rebuild* the initramfs.
2. **The 11-minute hang** is `nerdctl run --net=compose_default` spinning in
   CNI ADD. Proven by `/proc/<pid>` + `iptables` diagnostics in-guest.
3. **`xt_comment` alone** turns the hang into a fast, debuggable failure
   (necessary, not sufficient).
4. **`nft_chain_nat` + `nft_nat`** are what let iptables-nft create the `nat`
   `POSTROUTING` base chain. With the full set loaded, the exact CNI iptables
   command that was failing succeeds. Confirmed live by injecting the
   decompressed `.ko` files via a virtiofs share and `insmod` (the `XT_MODS`
   path in `run.sh`), without rebuilding the initramfs.
5. **`iptables-legacy` is not an alternative** — the standard profile ships no
   legacy binary, only `xtables-nft-multi`. So the fix must be the nft module
   set, not a backend switch.

### S2 build: (BuildKit-in-a-container)

Two findings, independent of the networking blocker:

- `nerdctl build`/`compose build` shells out to the **`buildctl` client**
  binary locally and talks to a `buildkitd` over `BUILDKIT_HOST`. The standard
  profile ships neither. S2 keeps the heavy daemon out of the base by running
  `buildkitd` as a throwaway container and copying the small `buildctl` client
  **out of that same image** into the guest PATH — no extra download, no base
  bloat.
- End-to-end S2 build could not be confirmed here because the `buildctl`-copy
  helper container itself runs on the (broken) default bridge network. It
  should pass once the CNI fix is built into the initramfs (or by pinning the
  helper to `--net=host`).

## Run

```bash
make sign                 # build host binary → ./dew
bash test/apps/compose/run.sh         # kernel + standard initramfs auto-download

# To prove the CNI fix WITHOUT rebuilding the initramfs, inject the modules:
#   extract xt_comment/nft_chain_nat/nft_nat/... .ko from the Alpine linux-virt
#   apk, gunzip them into a dir, then:
XT_MODS=/path/to/decompressed-kos bash test/apps/compose/run.sh
```

`run.sh` boots a standard VM, shares this directory into the guest at
`/compose`, forwards host ports 8080/8081, brings up the image-only services
first (clean signal), then exercises the S2 build tier, and tears down.

## Proper end-to-end validation (needs the Linux/Docker build env)

The CNI fix is in `initramfs/build.sh` but cannot be built on macOS
(`apk-tools-static` is Linux-only). To get an all-green run: rebuild the
standard initramfs in CI / a Linux box, publish it, then re-run `run.sh`
(without `XT_MODS`).

## Not yet probed (follow-ups)

- bind mounts whose source is on the virtiofs share (`./src:/app`)
- `env_file:` / `.env` interpolation
- compose `profiles:` and multiple compose files
- resource limits (`deploy.resources`) mapping to VM cpu/memory

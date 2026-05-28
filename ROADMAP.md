# Roadmap

## v0.1 (current)

macOS. Apple Virtualization.framework. Alpine Linux.

- `dew up` — zero-config project detection (Vite, Next.js, Astro, Nuxt, SvelteKit)
- Three profiles: minimal, node, standard (containerd)
- vsock exec with `--json`, `--events`, `--stream`
- Port forwarding, persistent disk, network isolation
- Auto-download assets on first use

## v0.2

- **Windows support** — WSL2 backend, `dew.exe` wrapper, `.exe` installer
- **Python profile** — detect `pyproject.toml` / `requirements.txt`, pip install
- **`dew up --with postgres,redis`** — start services alongside your app
- **`dew down`** — explicit stop (not just Ctrl+C)
- **Creek sandbox integration** — `creek sandbox` powered by Dew

## Future

- Go, Rust, Deno detection
- `dew.toml` project config (override auto-detect defaults)
- Pre-baked Node.js in initramfs (faster first boot)
- Turbo kernel for all profiles
- `dew ps` — list running VMs
- Official website (dewvm.dev)

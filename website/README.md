# dewvm.dev

Marketing site for [dew](../README.md). Astro 5, static output, deployed to
Cloudflare Workers Static Assets.

Lives in this monorepo (not a separate `solcreek/dewvm.dev`) because LLM
agents working on dew benefit from reading blog posts, docs, and code in
one checkout. See `project_dew_monorepo_decision` in memory for the
reasoning.

## Develop

```bash
pnpm install
pnpm dev        # http://localhost:4321
```

## Build

```bash
pnpm build      # outputs to dist/
```

The build does a one-time fetch of the latest `solcreek/dew` GitHub release
tag and star count, baking both into the static HTML. Re-run the build to
refresh.

## Deploy (Cloudflare Workers)

First time:

```bash
pnpm wrangler login
pnpm wrangler deploy   # uses wrangler.toml; deploys dist/ as static assets
```

After first deploy, attach the `dewvm.dev` custom domain in the Cloudflare
dashboard → Workers → your worker → Custom Domains.

Subsequent deploys:

```bash
pnpm deploy           # = astro build && wrangler deploy
```

## Structure

```
website/
├── public/
│   ├── favicon.svg          dew dewdrop mark
│   └── scripts/app.js       vanilla JS: theme toggle, terminal animation,
│                             scroll reveals, copy buttons, install tabs
├── src/
│   ├── styles/
│   │   ├── tokens.css       Loopwise design system (Geist + ink/lime)
│   │   └── site.css         page-specific layout
│   └── pages/
│       └── index.astro      single landing page
├── astro.config.mjs         static output, lightningcss minify
├── wrangler.toml            Workers Static Assets binding
├── package.json
└── tsconfig.json
```

## Updates

- **Hero version** + GitHub star count: re-build (build-time fetch from GH API)
- **Copy / sections**: edit `src/pages/index.astro` — preserves the original
  Dew Landing prototype structure 1:1
- **Tokens / colors**: edit `src/styles/tokens.css`
- **Layout / components**: edit `src/styles/site.css`
- **Terminal animation script** / install tabs: edit `public/scripts/app.js`

## Conventions

- English only (per `feedback_english_only` memory)
- No mentions of Marina / Grove (per `feedback_dew_repo_independence`) —
  this is dew's site, downstream products have their own
- Competitor names are fine here (marketing context allowed) — but NOT in
  commit messages or in the dew CLI codebase
- Update the page's `Run any app, anywhere.` tagline only with consensus
  (matches CLI tagline)

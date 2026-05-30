// @ts-check
import { defineConfig } from 'astro/config'

// Pure static landing — no SSR needed. Build outputs to dist/, deployed via
// Cloudflare Workers Static Assets (see wrangler.toml). If we later add SSR
// (e.g., dynamic GitHub release fetch), swap to @astrojs/cloudflare adapter
// and `output: 'server'`.

export default defineConfig({
  site: 'https://dewvm.dev',
  output: 'static',
  build: {
    inlineStylesheets: 'auto',
  },
  vite: {
    build: {
      cssMinify: 'lightningcss',
    },
  },
})

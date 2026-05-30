#!/usr/bin/env node
// Copies /install.sh into website/public/install.sh so Astro serves it at
// https://dewvm.dev/install.sh. Runs as `prebuild` so every `pnpm run
// build` (and therefore every deploy) picks up the current contents.
//
// website/public/install.sh is gitignored; the source of truth is the
// repo-root install.sh. This guarantees a deploy can never serve a stale
// copy — if you can build, you've already synced.

import { copyFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const src  = join(here, "..", "..", "install.sh");
const destDir = join(here, "..", "public");
const dest = join(destDir, "install.sh");

mkdirSync(destDir, { recursive: true });
copyFileSync(src, dest);
console.log(`sync-install-sh: ${src} → ${dest}`);

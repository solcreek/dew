#!/usr/bin/env node
// Copies the repo-root install scripts into website/public/ so Astro
// serves them at https://dewvm.dev/install.{sh,ps1}. Runs as
// `prebuild` so every `pnpm run build` (and therefore every deploy)
// picks up the current contents — a deploy can never serve a stale
// copy. website/public/install.{sh,ps1} are gitignored; sources of
// truth are the repo-root scripts.
//
// macOS / Linux fetch install.sh; Windows PowerShell fetches
// install.ps1 via `irm`. Keeping both behind the same short brand
// URL is what lets the website's install tabs use one domain for
// every platform instead of branching to a long GH-Releases path
// for Windows only.

import { copyFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..");
const destDir = join(here, "..", "public");

mkdirSync(destDir, { recursive: true });

for (const name of ["install.sh", "install.ps1"]) {
  const src = join(repoRoot, name);
  const dest = join(destDir, name);
  copyFileSync(src, dest);
  console.log(`sync-install-scripts: ${src} → ${dest}`);
}

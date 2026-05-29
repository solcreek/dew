#!/usr/bin/env node
// sync-version.mjs — set the main package.json to <version> and lock every
// optionalDependency at the exact same version. Used by CI on tag push.
//
// Usage:
//   node npm/scripts/sync-version.mjs <version> [<package.json-path>]
//
// When <package.json-path> is omitted, defaults to npm/package.json relative
// to this script (so the script Just Works in CI). Explicit path is for tests.

import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join, resolve } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const defaultPkgPath = join(here, "..", "package.json");

const [, , version, pkgPathArg] = process.argv;
if (!version) {
  console.error("Usage: sync-version.mjs <version> [<package.json-path>]");
  process.exit(1);
}

const pkgPath = pkgPathArg ? resolve(pkgPathArg) : defaultPkgPath;

const pkg = JSON.parse(readFileSync(pkgPath, "utf8"));
pkg.version = version;

for (const dep of Object.keys(pkg.optionalDependencies ?? {})) {
  pkg.optionalDependencies[dep] = version;
}

writeFileSync(pkgPath, JSON.stringify(pkg, null, 2) + "\n");
console.log(`sync-version: ${pkg.name}@${version} (${pkgPath})`);
for (const [dep, v] of Object.entries(pkg.optionalDependencies ?? {})) {
  console.log(`  ${dep}@${v}`);
}

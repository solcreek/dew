#!/usr/bin/env node
// generate-packages.mjs — scaffold @solcreek/dew-<triple> publish directories
// from the built (signed + notarized) binaries.
//
// Usage:
//   node npm/scripts/generate-packages.mjs <version> <artifacts-dir> <out-dir>
//
// Reads from <artifacts-dir>:
//   dew-darwin-arm64
//   dew-darwin-amd64
//   dew-linux-amd64
//   dew-linux-arm64
//   dew-windows-x86_64.exe
//
// Writes to <out-dir>/dew-<triple>/:
//   package.json   (name, version, os, cpu, files)
//   bin/dew        (the native binary, chmod 755; .exe on win32)
//
// The binary is the same file as the GitHub Release asset, so its Apple
// Developer ID + notarization staple is preserved verbatim — npm tarball
// extraction does not touch the bytes inside the Mach-O.

import { mkdirSync, copyFileSync, writeFileSync, chmodSync, existsSync } from "node:fs";
import { join } from "node:path";

const [, , version, artifactsDir, outDir] = process.argv;

if (!version || !artifactsDir || !outDir) {
  console.error("Usage: generate-packages.mjs <version> <artifacts-dir> <out-dir>");
  process.exit(1);
}

const platforms = [
  { triple: "darwin-arm64", asset: "dew-darwin-arm64",       os: "darwin", cpu: "arm64", binName: "dew"     },
  { triple: "darwin-x64",   asset: "dew-darwin-amd64",       os: "darwin", cpu: "x64",   binName: "dew"     },
  { triple: "linux-x64",    asset: "dew-linux-amd64",        os: "linux",  cpu: "x64",   binName: "dew"     },
  { triple: "linux-arm64",  asset: "dew-linux-arm64",        os: "linux",  cpu: "arm64", binName: "dew"     },
  { triple: "win32-x64",    asset: "dew-windows-x86_64.exe", os: "win32",  cpu: "x64",   binName: "dew.exe" },
];

let scaffolded = 0;
let skipped = 0;

for (const p of platforms) {
  const srcBinary = join(artifactsDir, p.asset);
  if (!existsSync(srcBinary)) {
    console.warn(`generate-packages: ${p.asset} not found in ${artifactsDir}, skipping ${p.triple}`);
    skipped++;
    continue;
  }

  const pkgDir = join(outDir, `dew-${p.triple}`);
  const binDir = join(pkgDir, "bin");
  mkdirSync(binDir, { recursive: true });

  const dstBinary = join(binDir, p.binName);
  copyFileSync(srcBinary, dstBinary);
  chmodSync(dstBinary, 0o755);

  const pkgJson = {
    name: `@solcreek/dew-${p.triple}`,
    version,
    description: `dew binary for ${p.os} ${p.cpu}`,
    license: "MIT",
    repository: { type: "git", url: "https://github.com/solcreek/dew" },
    os: [p.os],
    cpu: [p.cpu],
    files: [`bin/${p.binName}`],
  };

  writeFileSync(
    join(pkgDir, "package.json"),
    JSON.stringify(pkgJson, null, 2) + "\n",
  );

  console.log(`generate-packages: scaffolded @solcreek/dew-${p.triple} -> ${pkgDir}`);
  scaffolded++;
}

console.log(`generate-packages: ${scaffolded} scaffolded, ${skipped} skipped`);
if (scaffolded === 0) {
  console.error("generate-packages: no platform packages produced");
  process.exit(1);
}

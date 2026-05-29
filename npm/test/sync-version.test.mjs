// sync-version.test.mjs — operate on a tmp package.json copy so we never
// touch the real npm/package.json during tests.

import { test } from "node:test";
import { strict as assert } from "node:assert";
import { spawnSync } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  readFileSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const script = join(here, "..", "scripts", "sync-version.mjs");

function makePkg(extra = {}) {
  const dir = mkdtempSync(join(tmpdir(), "dew-pkg-"));
  const pkgPath = join(dir, "package.json");
  writeFileSync(
    pkgPath,
    JSON.stringify(
      {
        name: "@solcreek/dew",
        version: "0.0.0",
        bin: { dew: "bin/dew" },
        optionalDependencies: {
          "@solcreek/dew-darwin-arm64": "0.0.0",
          "@solcreek/dew-darwin-x64": "0.0.0",
          "@solcreek/dew-linux-x64": "0.0.0",
          "@solcreek/dew-linux-arm64": "0.0.0",
          "@solcreek/dew-win32-x64": "0.0.0",
        },
        ...extra,
      },
      null,
      2,
    ) + "\n",
  );
  return { dir, pkgPath };
}

function run(version, pkgPath) {
  return spawnSync("node", [script, version, pkgPath], { encoding: "utf8" });
}

test("syncs main version and every optional dep to the same version", () => {
  const { dir, pkgPath } = makePkg();
  try {
    const r = run("0.6.0", pkgPath);
    assert.equal(r.status, 0, `stderr: ${r.stderr}`);

    const pkg = JSON.parse(readFileSync(pkgPath, "utf8"));
    assert.equal(pkg.version, "0.6.0");
    for (const v of Object.values(pkg.optionalDependencies)) {
      assert.equal(v, "0.6.0");
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("idempotent: running twice produces same output", () => {
  const { dir, pkgPath } = makePkg();
  try {
    run("0.7.1", pkgPath);
    const first = readFileSync(pkgPath, "utf8");
    run("0.7.1", pkgPath);
    const second = readFileSync(pkgPath, "utf8");
    assert.equal(first, second);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("works on a package.json with no optionalDependencies", () => {
  const { dir, pkgPath } = makePkg({ optionalDependencies: undefined });
  // Remove the field entirely (above merges undefined into the object).
  const p = JSON.parse(readFileSync(pkgPath, "utf8"));
  delete p.optionalDependencies;
  writeFileSync(pkgPath, JSON.stringify(p, null, 2) + "\n");

  try {
    const r = run("0.8.0", pkgPath);
    assert.equal(r.status, 0);
    const pkg = JSON.parse(readFileSync(pkgPath, "utf8"));
    assert.equal(pkg.version, "0.8.0");
    assert.equal(pkg.optionalDependencies, undefined);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("missing version arg → exit 1 + usage", () => {
  const r = spawnSync("node", [script], { encoding: "utf8" });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /Usage:/);
});

test("preserves other fields in package.json", () => {
  const { dir, pkgPath } = makePkg({
    description: "kept",
    license: "MIT",
    customField: { nested: true },
  });
  try {
    run("0.6.1", pkgPath);
    const pkg = JSON.parse(readFileSync(pkgPath, "utf8"));
    assert.equal(pkg.description, "kept");
    assert.equal(pkg.license, "MIT");
    assert.deepEqual(pkg.customField, { nested: true });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

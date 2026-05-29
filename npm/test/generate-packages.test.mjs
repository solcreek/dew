// generate-packages.test.mjs — feed synthetic artifacts to the scaffolder
// and verify the produced package directories.

import { test } from "node:test";
import { strict as assert } from "node:assert";
import { spawnSync } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  readFileSync,
  rmSync,
  statSync,
  existsSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const script = join(here, "..", "scripts", "generate-packages.mjs");

function setupArtifacts(assets) {
  const dir = mkdtempSync(join(tmpdir(), "dew-art-"));
  for (const name of assets) {
    writeFileSync(join(dir, name), `fake-binary-content-${name}`);
  }
  return dir;
}

function run(version, artifactsDir, outDir) {
  return spawnSync("node", [script, version, artifactsDir, outDir], {
    encoding: "utf8",
  });
}

const ALL_ASSETS = [
  "dew-darwin-arm64",
  "dew-darwin-amd64",
  "dew-linux-amd64",
  "dew-linux-arm64",
  "dew-windows-x86_64.exe",
];

const TRIPLES = ["darwin-arm64", "darwin-x64", "linux-x64", "linux-arm64", "win32-x64"];

test("all 5 binaries present → 5 packages scaffolded", () => {
  const arts = setupArtifacts(ALL_ASSETS);
  const out = mkdtempSync(join(tmpdir(), "dew-out-"));
  try {
    const r = run("1.2.3", arts, out);
    assert.equal(r.status, 0, `stderr: ${r.stderr}`);

    for (const triple of TRIPLES) {
      const pkgDir = join(out, `dew-${triple}`);
      assert.ok(existsSync(pkgDir), `missing ${pkgDir}`);
      const pkg = JSON.parse(readFileSync(join(pkgDir, "package.json"), "utf8"));
      assert.equal(pkg.name, `@solcreek/dew-${triple}`);
      assert.equal(pkg.version, "1.2.3");
      assert.deepEqual(pkg.os, [triple.split("-")[0]]);
      assert.deepEqual(pkg.cpu, [triple.split("-")[1]]);

      const binName = triple.startsWith("win32") ? "dew.exe" : "dew";
      const binPath = join(pkgDir, "bin", binName);
      assert.ok(existsSync(binPath), `missing ${binPath}`);
      const mode = statSync(binPath).mode & 0o777;
      assert.equal(mode, 0o755, `binary at ${binPath} should be 755, got 0${mode.toString(8)}`);
    }
  } finally {
    rmSync(arts, { recursive: true, force: true });
    rmSync(out, { recursive: true, force: true });
  }
});

test("partial assets → only matching triples scaffolded, exits 0", () => {
  const arts = setupArtifacts(["dew-darwin-arm64", "dew-linux-arm64"]);
  const out = mkdtempSync(join(tmpdir(), "dew-out-"));
  try {
    const r = run("0.9.0", arts, out);
    assert.equal(r.status, 0);
    assert.ok(existsSync(join(out, "dew-darwin-arm64")));
    assert.ok(existsSync(join(out, "dew-linux-arm64")));
    assert.ok(!existsSync(join(out, "dew-darwin-x64")));
    assert.ok(!existsSync(join(out, "dew-linux-x64")));
    assert.ok(!existsSync(join(out, "dew-win32-x64")));
  } finally {
    rmSync(arts, { recursive: true, force: true });
    rmSync(out, { recursive: true, force: true });
  }
});

test("zero assets → exit 1 with diagnostic", () => {
  const arts = setupArtifacts([]);
  const out = mkdtempSync(join(tmpdir(), "dew-out-"));
  try {
    const r = run("0.0.0", arts, out);
    assert.equal(r.status, 1);
    assert.match(r.stderr, /no platform packages produced/);
  } finally {
    rmSync(arts, { recursive: true, force: true });
    rmSync(out, { recursive: true, force: true });
  }
});

test("missing arguments → exit 1 with usage", () => {
  const r = spawnSync("node", [script], { encoding: "utf8" });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /Usage:/);
});

test("binary content preserved byte-for-byte (notarization staple safety)", () => {
  const arts = mkdtempSync(join(tmpdir(), "dew-art-"));
  const out = mkdtempSync(join(tmpdir(), "dew-out-"));
  try {
    // Bytes that include the kind of binary payload the notarization staple
    // would live in — we don't care that it's a real Mach-O, only that the
    // copy is exact.
    const original = Buffer.from([0xcf, 0xfa, 0xed, 0xfe, 0x00, 0x01, 0x02, 0x03, 0x42]);
    writeFileSync(join(arts, "dew-darwin-arm64"), original);

    const r = run("1.0.0", arts, out);
    assert.equal(r.status, 0);

    const copied = readFileSync(join(out, "dew-darwin-arm64", "bin", "dew"));
    assert.equal(
      Buffer.compare(original, copied), 0,
      "binary bytes must match exactly — npm pack/install preserves them too",
    );
  } finally {
    rmSync(arts, { recursive: true, force: true });
    rmSync(out, { recursive: true, force: true });
  }
});

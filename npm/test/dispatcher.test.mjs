// Dispatcher tests — spawn npm/bin/dew with controlled env and assert
// behavior. We only exercise paths that don't require network access;
// the download/checksum/cosign code is covered by the release pipeline
// itself (a successful release proves the artifact layout matches what
// the dispatcher expects to fetch).

import { test } from "node:test";
import { strict as assert } from "node:assert";
import { spawnSync } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  chmodSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const dispatcher = join(here, "..", "bin", "dew");

function makeFakeBinary(dir, exitCode = 0) {
  const path = join(dir, "fake-dew");
  writeFileSync(
    path,
    `#!/usr/bin/env node
process.stdout.write(JSON.stringify({
  args: process.argv.slice(2),
  env_tag: process.env.FAKE_TAG,
}));
process.exit(${exitCode});
`,
  );
  chmodSync(path, 0o755);
  return path;
}

function runDispatcher({ env = {}, args = [] } = {}) {
  return spawnSync("node", [dispatcher, ...args], {
    env: { ...process.env, ...env },
    encoding: "utf8",
  });
}

test("DEW_BINARY override: spawns the named binary and forwards argv", () => {
  const dir = mkdtempSync(join(tmpdir(), "dew-disp-"));
  try {
    const fake = makeFakeBinary(dir, 0);
    const result = runDispatcher({
      env: { DEW_BINARY: fake, FAKE_TAG: "hello" },
      args: ["one", "two", "three"],
    });
    assert.equal(result.status, 0);
    const parsed = JSON.parse(result.stdout);
    assert.deepEqual(parsed.args, ["one", "two", "three"]);
    assert.equal(parsed.env_tag, "hello");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("DEW_BINARY override: propagates non-zero exit code", () => {
  const dir = mkdtempSync(join(tmpdir(), "dew-disp-"));
  try {
    const fake = makeFakeBinary(dir, 42);
    const result = runDispatcher({ env: { DEW_BINARY: fake } });
    assert.equal(result.status, 42);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("DEW_BINARY override: missing file → exit 1 + helpful error", () => {
  const result = runDispatcher({
    env: { DEW_BINARY: "/nonexistent/path/dew" },
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /DEW_BINARY=/);
  assert.match(result.stderr, /does not exist/);
});

test("Cached binary in DEW_CACHE_DIR is reused without network", () => {
  // Pre-populate the cache dir so the dispatcher should find the binary
  // and skip the download path entirely.
  const dir = mkdtempSync(join(tmpdir(), "dew-cache-"));
  try {
    // Match the binary name the dispatcher looks for on this platform.
    const binName = process.platform === "win32" ? "dew.exe" : "dew";
    const fake = join(dir, binName);
    writeFileSync(
      fake,
      `#!/usr/bin/env node
process.stdout.write("cached:" + process.argv.slice(2).join(","));
process.exit(0);
`,
    );
    chmodSync(fake, 0o755);
    const result = runDispatcher({
      env: { DEW_CACHE_DIR: dir },
      args: ["x", "y"],
    });
    assert.equal(result.status, 0);
    assert.equal(result.stdout, "cached:x,y");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// Dispatcher integration tests — spawn npm/bin/dew with controlled env and
// assert behavior. The dispatcher itself stays small + monolithic; we get
// coverage by exercising every error path through spawnSync.

import { test } from "node:test";
import { strict as assert } from "node:assert";
import { spawnSync } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  mkdirSync,
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
  env_pwd: process.env.FAKE_TAG,
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
    assert.equal(parsed.env_pwd, "hello");
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

test("Platform package missing: error mentions package name + install hint", () => {
  // Unset DEW_BINARY so dispatcher falls through to require.resolve, which
  // will fail because the platform package is not installed in this repo's
  // node_modules (we're running tests on the dew repo itself, not from a
  // consumer install).
  const result = runDispatcher({ env: { DEW_BINARY: "" } });
  assert.equal(result.status, 1);
  // One of two errors is expected: unsupported platform OR missing platform pkg
  const stderr = result.stderr;
  const isSupportedPlatform =
    /platform package .* is missing/.test(stderr) &&
    /npm install --force --os/.test(stderr);
  const isUnsupportedPlatform = /is not a supported platform/.test(stderr);
  assert.ok(
    isSupportedPlatform || isUnsupportedPlatform,
    `unexpected stderr:\n${stderr}`,
  );
});

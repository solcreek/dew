const { execSync } = require("child_process");
const { existsSync, writeFileSync, mkdirSync, chmodSync } = require("fs");
const path = require("path");
const os = require("os");

const binDir = path.join(__dirname, "..", "bin");
const binary = path.join(binDir, "dew-bin");

if (!existsSync(binDir)) {
  mkdirSync(binDir, { recursive: true });
}

if (!existsSync(binary)) {
  const platform = os.platform();
  const arch = os.arch() === "arm64" ? "arm64" : "amd64";
  const repo = "solcreek/dew";
  const pkg = require("../package.json");
  const tag = `v${pkg.version}`;

  let asset;
  if (platform === "darwin") {
    asset = `dew-darwin-${arch}`;
  } else if (platform === "linux") {
    asset = `dew-linux-${arch}`;
  } else if (platform === "win32") {
    asset = `dew-windows-x86_64.exe`;
  } else {
    console.log(`dew: unsupported platform ${platform}`);
    process.exit(0);
  }

  const url = `https://github.com/${repo}/releases/download/${tag}/${asset}`;
  console.log(`dew: downloading ${asset}...`);

  try {
    execSync(`curl -fsSL -o "${binary}" "${url}"`, { stdio: "pipe" });
    chmodSync(binary, 0o755);
    console.log("dew: installed");
  } catch (e) {
    console.log(`dew: download failed from ${url}`);
    console.log("  GitHub Release may not exist yet.");
    console.log("  Install manually: https://github.com/solcreek/dew/releases");
    process.exit(0);
  }
}

// Show how to invoke dew based on how the user installed the package.
// npx       → `npx @solcreek/dew ...` (the binary is in a per-run cache)
// local     → `npx dew ...` or `./node_modules/.bin/dew ...`
// global    → `dew ...` (on PATH)
//
// We can't reliably detect "is this a global install" from inside
// postinstall, so we surface all three forms once.
function printInvocationHint() {
  // Skip in CI / non-interactive shells unless DEW_INSTALL_HINT is set
  if (process.env.CI && !process.env.DEW_INSTALL_HINT) return;

  const npmConfig = process.env.npm_config_global === "true";
  console.log("");
  if (npmConfig) {
    console.log("dew: installed globally — run `dew --help` from any terminal.");
  } else {
    console.log("dew: installed locally. Choose how to invoke:");
    console.log("  • One-off:   npx @solcreek/dew --help");
    console.log("  • Local pkg: npx dew --help    (inside this project)");
    console.log("  • Global:    npm i -g @solcreek/dew  → then `dew --help`");
  }
  console.log("");
}
printInvocationHint();

// macOS: codesign with virtualization entitlement
if (os.platform() === "darwin" && existsSync(binary) && !binary.endsWith(".exe")) {
  const entitlements = path.join(__dirname, "entitlements.plist");
  if (!existsSync(entitlements)) {
    writeFileSync(
      entitlements,
      `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>com.apple.security.virtualization</key>
    <true/>
</dict>
</plist>`
    );
  }

  try {
    execSync(
      `codesign --entitlements "${entitlements}" --force -s - "${binary}"`,
      { stdio: "pipe" }
    );
    console.log("dew: signed with virtualization entitlement");
  } catch (e) {
    console.log("");
    console.log("dew: ⚠️  codesign failed — VM commands (dew up, dew app run) will not work");
    console.log("dew: this happens in sandboxed environments (some IDE terminals, CI)");
    console.log("dew: try running in a regular Terminal/iTerm, or:");
    console.log(`dew:   codesign --entitlements "${entitlements}" --force -s - "${binary}"`);
    console.log("");
  }
}

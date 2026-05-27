const { execSync } = require("child_process");
const { existsSync, writeFileSync, mkdirSync } = require("fs");
const path = require("path");
const os = require("os");

if (os.platform() !== "darwin") {
  console.log("dew: skipping codesign (not macOS)");
  process.exit(0);
}

const binDir = path.join(__dirname, "..", "bin");
const binary = path.join(binDir, "dew");

if (!existsSync(binary)) {
  console.log("dew: binary not found, skipping codesign");
  process.exit(0);
}

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
  console.log("dew: codesign failed (may need manual signing)");
  console.log(`  run: codesign --entitlements "${entitlements}" --force -s - "${binary}"`);
}

// Pull VM assets on first install
const dataDir = path.join(os.homedir(), ".local", "share", "dew");
const kernel = path.join(dataDir, "vmlinuz");
if (!existsSync(kernel)) {
  console.log("dew: VM assets not found. Run 'dew assets pull' to download kernel + initramfs.");
}

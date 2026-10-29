.PHONY: build agent sign test clean

BIN := dew
AGENT := dew-agent
ENTITLEMENTS := entitlements.plist

build:
	go build -o $(BIN) ./cmd/dew/

# Guest agent (rust/dew-agent) for both VM arches. Needs rustup's cargo:
# rust/rust-toolchain.toml pins the version and the musl targets. Cargo
# always names the binary dew-agent; like initramfs/build.sh, the copies run
# in rust/, where cargo resolves a relative CARGO_TARGET_DIR.
agent:
	cd rust && cargo build --locked --release -p dew-agent --target x86_64-unknown-linux-musl
	cd rust && cargo build --locked --release -p dew-agent --target aarch64-unknown-linux-musl
	cd rust && cp "$${CARGO_TARGET_DIR:-target}/x86_64-unknown-linux-musl/release/dew-agent" "$(abspath $(AGENT))-linux-amd64"
	cd rust && cp "$${CARGO_TARGET_DIR:-target}/aarch64-unknown-linux-musl/release/dew-agent" "$(abspath $(AGENT))-linux-arm64"

sign: build
	codesign --entitlements $(ENTITLEMENTS) --force -s - $(BIN)

test:
	go test ./...

clean:
	rm -f $(BIN) $(AGENT)-linux-*

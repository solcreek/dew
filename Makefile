.PHONY: build agent sign test clean

BIN := dew
AGENT := dew-agent
ENTITLEMENTS := entitlements.plist

build:
	go build -o $(BIN) ./cmd/dew/

# Guest agent (rust/dew-agent) for both VM arches. Needs rustup's cargo:
# rust/rust-toolchain.toml pins the version and the musl targets.
agent:
	cd rust && cargo build --locked --release -p dew-agent --target x86_64-unknown-linux-musl
	cd rust && cargo build --locked --release -p dew-agent --target aarch64-unknown-linux-musl
	cp rust/target/x86_64-unknown-linux-musl/release/$(AGENT) $(AGENT)-linux-amd64
	cp rust/target/aarch64-unknown-linux-musl/release/$(AGENT) $(AGENT)-linux-arm64

sign: build
	codesign --entitlements $(ENTITLEMENTS) --force -s - $(BIN)

test:
	go test ./...

clean:
	rm -f $(BIN) $(AGENT)-linux-*

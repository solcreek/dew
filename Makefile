.PHONY: build agent sign test clean

BIN := dew
AGENT := dew-agent
ENTITLEMENTS := entitlements.plist

build:
	go build -o $(BIN) ./cmd/dew/

agent:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o $(AGENT)-linux-amd64 ./cmd/dew-agent/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(AGENT)-linux-arm64 ./cmd/dew-agent/

sign: build
	codesign --entitlements $(ENTITLEMENTS) --force -s - $(BIN)

test:
	go test ./...

clean:
	rm -f $(BIN) $(AGENT)-linux-*

BINARY     := origamy
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -ldflags "-X github.com/qubelylabs/origamy-cli/cmd/origamy/cmd.BuildVersion=$(VERSION) -s -w"
BUILD_DIR  := bin

.PHONY: build build-all release clean test

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/origamy

build-all:
	GOOS=linux  GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)_linux_amd64   ./cmd/origamy
	GOOS=linux  GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)_linux_arm64   ./cmd/origamy
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)_darwin_amd64  ./cmd/origamy
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)_darwin_arm64  ./cmd/origamy
	# SHA256SUMS lets install.sh verify the downloaded binary's integrity.
	cd $(BUILD_DIR) && { sha256sum $(BINARY)_* 2>/dev/null || shasum -a 256 $(BINARY)_*; } > SHA256SUMS
	# Detached Ed25519 signature over SHA256SUMS. The signing key lives OUTSIDE
	# the release (ORIGAMY_RELEASE_KEY), so a tampered release is detectable, not
	# just corruption. install.sh verifies it with the embedded public key.
	# Signed by tools/sign (pure Go) rather than `openssl pkeyutl`: macOS ships
	# LibreSSL 3.3, which cannot load Ed25519 keys, so releases could otherwise
	# only be cut on Linux. Same raw 64-byte signature either way.
	@if [ -n "$(ORIGAMY_RELEASE_KEY)" ]; then \
		go run ./tools/sign -key "$(ORIGAMY_RELEASE_KEY)" -in $(BUILD_DIR)/SHA256SUMS -out $(BUILD_DIR)/SHA256SUMS.sig; \
	else \
		echo "WARNING: ORIGAMY_RELEASE_KEY not set — SHA256SUMS will be UNSIGNED. Point it at your Ed25519 release private key to sign."; \
	fi

release: build-all
	@echo "Creating GitHub release $(VERSION)..."
	gh release create $(VERSION) \
		--repo qubelylabs/origamy-cli \
		--title "$(VERSION)" \
		--notes "Origamy CLI $(VERSION)" \
		$(BUILD_DIR)/$(BINARY)_linux_amd64 \
		$(BUILD_DIR)/$(BINARY)_linux_arm64 \
		$(BUILD_DIR)/$(BINARY)_darwin_amd64 \
		$(BUILD_DIR)/$(BINARY)_darwin_arm64 \
		$(BUILD_DIR)/SHA256SUMS
	@if [ -f $(BUILD_DIR)/SHA256SUMS.sig ]; then \
		gh release upload $(VERSION) --repo qubelylabs/origamy-cli $(BUILD_DIR)/SHA256SUMS.sig && \
		echo "Uploaded SHA256SUMS.sig"; \
	fi

test:
	go test ./...

# verify-release checks a downloaded release's SHA256SUMS.sig against a public
# key PEM, without depending on the host openssl (macOS LibreSSL cannot).
#   make verify-release PUB=pub.pem DIR=bin
verify-release:
	go run ./tools/sign -verify -pub "$(PUB)" -in $(or $(DIR),$(BUILD_DIR))/SHA256SUMS

clean:
	rm -rf $(BUILD_DIR)

fmt:
	gofmt -w .

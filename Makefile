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

test:
	go test ./...

clean:
	rm -rf $(BUILD_DIR)

fmt:
	gofmt -w .

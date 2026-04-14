BIN     := mcp-bridge
CMD     := ./cmd/mcp-bridge
CONFIG  := config.yaml

# ---------------------------------------------------------------------------
# Version stamping — override any of these on the command line or let them
# be derived automatically from git.
# ---------------------------------------------------------------------------
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG        := mcp-bridge/internal/version
LDFLAGS    := -s -w \
              -X $(PKG).Version=$(VERSION) \
              -X $(PKG).GitCommit=$(GIT_COMMIT) \
              -X $(PKG).BuildDate=$(BUILD_DATE)

.PHONY: build run tidy vet fmt clean version

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) $(CMD)

run: build
	./$(BIN) -config $(CONFIG)

version: build
	./$(BIN) version

tidy:
	go mod tidy

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -f $(BIN)

# Spectate host agent (protocol v2)

BINARY  := spectate-agent
PKG     := github.com/spectate/agent/v2
VERSION ?= $(shell git describe --tags --match 'v2.*' --always --dirty 2>/dev/null | sed 's|^v||' || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
API_URL ?= https://api.spectate.net
LDFLAGS := -s -w \
	-X '$(PKG)/internal/version.Version=$(VERSION)' \
	-X '$(PKG)/internal/version.Commit=$(COMMIT)' \
	-X '$(PKG)/internal/version.BuildDate=$(DATE)' \
	-X '$(PKG)/internal/version.DefaultAPIURL=$(API_URL)'
GOFLAGS := -trimpath
# Copy of the canonical schema in spectate/platform (laravel/proto). The M
# option maps it to this module whatever its go_package says.
PROTO   := spectate/agent/v2/ingest.proto

.PHONY: all build dist test race vet fmt proto golden bench release snapshot clean

all: vet test build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/spectate-agent

# Static Linux binaries (primary targets) plus macOS (best effort).
dist:
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags "$(LDFLAGS)" \
			-o dist/$(BINARY)_$${os}_$${arch} ./cmd/spectate-agent || exit 1; \
	done
	cd dist && shasum -a 256 $(BINARY)_* > checksums.txt

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...
	GOOS=linux go vet ./...

fmt:
	gofmt -w .

# Go bindings from proto/ (copy it from spectate/platform laravel/proto first).
# PHP bindings are generated in the platform repo.
proto:
	protoc --proto_path=proto --go_out=. --go_opt=module=$(PKG) \
		--go_opt=M$(PROTO)=$(PKG)/internal/proto/agentv2\;agentv2 proto/$(PROTO)

# Regenerate the cross-language fixture decoded by the platform's
# laravel/tests/Feature/HostAgentV2Test.php (copy both testdata files there).
golden:
	go test ./internal/encode -run Golden -update

bench:
	go test ./internal/encode -run '^$$' -bench . -benchmem

release:
	goreleaser release --clean

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist

GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)
GCFLAGS := -gcflags=github.com/nn1a/autogora/internal/...=-l
GCFLAGS += -gcflags=github.com/charmbracelet/...=-l
GCFLAGS += -gcflags=github.com/modelcontextprotocol/...=-l
GCFLAGS += -gcflags=github.com/google/jsonschema-go/...=-l

.PHONY: build test verify release release-musl release-plan test-release test-release-musl

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false $(GCFLAGS) -ldflags "$(LDFLAGS)" -o bin/autogora ./cmd/autogora

test:
	$(GO) test ./...

verify:
	$(GO) test -race ./...
	$(GO) vet ./...

release:
	GO="$(GO)" ./scripts/build-release.sh "$(VERSION)" release

release-musl:
	TARGETS="linux-musl/amd64 linux-musl/arm64" GO="$(GO)" ./scripts/build-release.sh "$(VERSION)" release

release-plan:
	RELEASE_PLAN_ONLY=1 GO="$(GO)" ./scripts/build-release.sh "$(VERSION)" release

test-release:
	GO="$(GO)" ./scripts/build-release_test.sh

test-release-musl:
	RELEASE_TEST_MUSL=1 GO="$(GO)" ./scripts/build-release_test.sh

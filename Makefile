GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)
GCFLAGS := -gcflags=github.com/nn1a/autogora/internal/...=-l
GCFLAGS += -gcflags=github.com/charmbracelet/...=-l
GCFLAGS += -gcflags=github.com/modelcontextprotocol/...=-l
GCFLAGS += -gcflags=github.com/google/jsonschema-go/...=-l

.PHONY: build test verify release

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false $(GCFLAGS) -ldflags "$(LDFLAGS)" -o bin/autogora ./cmd/autogora

test:
	$(GO) test ./...

verify:
	$(GO) test -race ./...
	$(GO) vet ./...

release:
	./scripts/build-release.sh "$(VERSION)" release

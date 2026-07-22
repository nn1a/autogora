GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)

.PHONY: build test verify release

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/taskcircuit ./cmd/taskcircuit

test:
	$(GO) test ./...

verify:
	$(GO) test -race ./...
	$(GO) vet ./...

release:
	./scripts/build-release.sh "$(VERSION)" release

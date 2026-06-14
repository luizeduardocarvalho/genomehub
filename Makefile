# GenomeHub — build / test / install
#
# Version metadata is injected into the binary via -ldflags so `genomehub
# version` reports the real tag/commit. A plain `go install ...@latest` skips
# these but still backfills from Go module build info (see cmd/version.go).

BINARY      := genomehub
PKG         := github.com/luizeduardocarvalho/genomehub/cmd
PREFIX      ?= /usr/local
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X $(PKG).version=$(VERSION) \
	-X $(PKG).commit=$(COMMIT) \
	-X $(PKG).date=$(DATE)

.PHONY: all build install uninstall test vet fmt clean release-snapshot version

all: build

## build: compile the binary into ./$(BINARY) with version metadata
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

## install: build and copy the binary to $(PREFIX)/bin
install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)

## uninstall: remove the installed binary
uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

## test: run the full test suite
test:
	go test ./...

## vet: run go vet
vet:
	go vet ./...

## fmt: gofmt all sources
fmt:
	gofmt -w .

## clean: remove build artifacts
clean:
	rm -f $(BINARY)
	rm -rf dist

## release-snapshot: build a local goreleaser snapshot (no publish)
release-snapshot:
	goreleaser release --snapshot --clean

## version: print the version that would be stamped into a build
version:
	@echo $(VERSION) $(COMMIT) $(DATE)

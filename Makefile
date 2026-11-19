# voxen — peer-to-peer encrypted voice for the terminal
#
# Reproducible builds: -trimpath, no CGO for the current phase, and
# explicit ldflags injecting version, commit and build date.
#
# Phase 1 has no C dependencies (no PortAudio, no libopus). Phase 2 adds Opus
# and audio backends; the `deps` target documents what that will require.

SHELL := /bin/bash
MODULE := github.com/s4chdev/voxen
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BIN_DIR := bin
DIST_DIR := dist

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(BUILD_DATE)

GOFLAGS := -trimpath
GO ?= go

.PHONY: all build voxen voxend test test-short test-race test-integration test-security \
	lint vet fmt tidy cross clean install uninstall dist deps license check ci help

all: check build

## build: compile both binaries into bin/
build: voxen voxend

voxen:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/voxen ./cmd/voxen

voxend:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/voxend ./cmd/voxend

## test: run the whole test suite
test:
	$(GO) test ./...

## test-short: unit tests only, no process spawning
test-short:
	$(GO) test -short ./internal/...

## test-race: full suite under the race detector
test-race:
	$(GO) test -race ./...

## test-integration: end-to-end tests using the real binaries
test-integration:
	$(GO) test -v ./tests/integration/...

## test-security: security acceptance scenarios
test-security:
	$(GO) test -v ./tests/security/...

## vet: go vet across all packages
vet:
	$(GO) vet ./...

## fmt: check formatting without rewriting
lint:
	@out=$$(gofmt -l . 2>/dev/null | grep -v '^$$'); \
	if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

## tidy: normalise go.mod / go.sum
tidy:
	$(GO) mod tidy
	$(GO) mod verify

## cross: compile for every supported platform (verifies build tags)
cross:
	@set -e; \
	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "building $$target"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build $(GOFLAGS) \
			-ldflags '$(LDFLAGS)' -o /dev/null ./... ; \
	done

## dist: build release archives for every supported platform
dist: cross
	@mkdir -p $(DIST_DIR)
	@set -e; \
	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		stage=$(DIST_DIR)/voxen-$(VERSION)-$$os-$$arch; \
		rm -rf $$stage; mkdir -p $$stage; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build $(GOFLAGS) \
			-ldflags '$(LDFLAGS)' -o $$stage/voxen$$ext ./cmd/voxen; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build $(GOFLAGS) \
			-ldflags '$(LDFLAGS)' -o $$stage/voxend$$ext ./cmd/voxend; \
		cp README.md LICENSE docs/security.md docs/protocol.md configs/config.example.yaml $$stage/ 2>/dev/null || true; \
		(cd $(DIST_DIR) && tar -czf voxen-$(VERSION)-$$os-$$arch.tar.gz $$(basename $$stage)) ; \
		rm -rf $$stage; \
	done
	@echo "artifacts in $(DIST_DIR)/"

## install: install into $GOBIN (or $(HOME)/go/bin)
install:
	$(GO) install -ldflags '$(LDFLAGS)' ./cmd/voxen ./cmd/voxend

## uninstall: remove installed binaries
uninstall:
	@rm -f $(shell $(GO) env GOPATH)/bin/voxen $(shell $(GO) env GOPATH)/bin/voxend

## license: list dependency licenses for audit
license:
	@$(GO) list -m -f '{{.Path}} {{.Version}}' all | tail -n +2 > $(DIST_DIR)/dependencies.txt 2>/dev/null || true
	@mkdir -p $(DIST_DIR)
	@$(GO) list -m -f '{{.Path}} {{.Version}}' all | tail -n +2 > $(DIST_DIR)/dependencies.txt
	@echo "wrote $(DIST_DIR)/dependencies.txt"

## deps: show host requirements for the current phase
deps:
	@echo "Phase 1 requires only the Go toolchain."
	@echo "Phase 2 will require:"
	@echo "  Linux:   libopus-dev, PipeWire or PulseAudio (runtime), pkg-config, gcc"
	@echo "  macOS:   Xcode command line tools, Opus (brew install opus)"
	@echo "  Windows: MinGW-w64 toolchain for cgo, Opus static library"

## check: what CI runs for a pull request
check: lint vet test-race cross

## ci: check plus integration and security suites
ci: check test-integration test-security

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | column -t -s ':' 2>/dev/null || \
		grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'

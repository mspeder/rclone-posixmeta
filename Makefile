SHELL := /bin/bash

BINARY := rclone-posixmeta
GO ?= go
HARNESS := ./test/run.sh

# Local rclone fork (not versioned): materialised from third_party/rclone-posixmeta.patch
FORK_DIR := third_party/rclone
FORK_STAMP := $(FORK_DIR)/.posixmeta-fork
FORK_PATCH := third_party/rclone-posixmeta.patch

.PHONY: all build fork test test-go test-cli test-mount test-list fmt vet clean distclean

all: build

# CGO is required on Linux for FUSE mount support.
build: $(FORK_STAMP)
	CGO_ENABLED=1 $(GO) build -o $(BINARY) .

fork: $(FORK_STAMP)

$(FORK_STAMP): $(FORK_PATCH) scripts/setup-fork.sh go.mod
	./scripts/setup-fork.sh

# Full harness: Go unit tests + CLI invariants + mount-over-crypt integration.
test: $(FORK_STAMP)
	$(HARNESS)

test-go: $(FORK_STAMP)
	$(GO) test ./backend/posixmeta/ -count=1

test-cli: build
	$(HARNESS) --no-build 01-cli

test-mount: build
	$(HARNESS) --no-build 02-mount-crypt

test-list:
	$(HARNESS) --list --no-build

vet: $(FORK_STAMP)
	$(GO) vet ./backend/posixmeta/ .

fmt:
	gofmt -w backend/posixmeta/posixmeta_test.go

clean:
	rm -f $(BINARY)

# Remove the materialised rclone fork as well (scripts/setup-fork.sh rebuilds it).
distclean: clean
	rm -rf $(FORK_DIR)

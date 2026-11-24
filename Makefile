SHELL := /usr/bin/env bash
.PHONY: all gen-types build-web build build-desktop build-all test test-desktop check-privacy clean

GO ?= go
PNPM ?= pnpm
BIN_DIR := bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.2.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(BUILT)

all: gen-types build-web build

gen-types:
	$(GO) generate ./internal/protocol/...

build-web: gen-types
	$(PNPM) --dir web build
	@touch web/dist/.gitkeep

build: build-web
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orbit-hub ./cmd/orbit-hub
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orbit-runner ./cmd/orbit-runner
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orbit ./cmd/orbit

build-desktop:
	@mkdir -p $(BIN_DIR)
	cd desktop && CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o ../$(BIN_DIR)/orbit-client .

build-all: build-web
	@mkdir -p $(BIN_DIR)/linux-amd64 $(BIN_DIR)/darwin-arm64 $(BIN_DIR)/windows-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/linux-amd64/orbit-hub ./cmd/orbit-hub
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/linux-amd64/orbit-runner ./cmd/orbit-runner
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/linux-amd64/orbit ./cmd/orbit
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/darwin-arm64/orbit-hub ./cmd/orbit-hub
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/darwin-arm64/orbit-runner ./cmd/orbit-runner
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/darwin-arm64/orbit ./cmd/orbit
	cd desktop && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o ../$(BIN_DIR)/linux-amd64/orbit-client .
	cd desktop && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o ../$(BIN_DIR)/darwin-arm64/orbit-client .
	cd desktop && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS) -H=windowsgui" -o ../$(BIN_DIR)/windows-amd64/orbit-client.exe .

test:
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

test-desktop:
	cd desktop && $(GO) vet ./... && $(GO) test -race -count=1 ./...

check-privacy:
	@bash scripts/check-privacy.sh

clean:
	rm -rf $(BIN_DIR)

SHELL := /usr/bin/env bash
.PHONY: all gen-types build-web build build-all test check-privacy clean

GO ?= go
PNPM ?= pnpm
BIN_DIR := bin

all: gen-types build-web build

gen-types:
	$(GO) generate ./internal/protocol/...

build-web: gen-types
	$(PNPM) --dir web build
	@touch web/dist/.gitkeep

build: build-web
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN_DIR)/orbit-hub ./cmd/orbit-hub
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN_DIR)/orbit-runner ./cmd/orbit-runner
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN_DIR)/orbit ./cmd/orbit

build-all: build-web
	@mkdir -p $(BIN_DIR)/linux-amd64 $(BIN_DIR)/darwin-arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o $(BIN_DIR)/linux-amd64/orbit-hub ./cmd/orbit-hub
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o $(BIN_DIR)/linux-amd64/orbit-runner ./cmd/orbit-runner
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o $(BIN_DIR)/linux-amd64/orbit ./cmd/orbit
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o $(BIN_DIR)/darwin-arm64/orbit-hub ./cmd/orbit-hub
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o $(BIN_DIR)/darwin-arm64/orbit-runner ./cmd/orbit-runner
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o $(BIN_DIR)/darwin-arm64/orbit ./cmd/orbit

test:
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

check-privacy:
	@bash scripts/check-privacy.sh

clean:
	rm -rf $(BIN_DIR)

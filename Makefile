# SPDX-License-Identifier: Apache-2.0

BINARY      := lith
PKG         := github.com/scttfrdmn/lith
CMD         := ./cmd/lith
VERSION_PKG := $(PKG)/internal/version

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).Date=$(DATE)

.PHONY: build test lint bench cover clean tidy

build: ## Build the lith binary
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

test: ## Run all tests with the race detector
	go test -race ./...

lint: ## Run go vet and golangci-lint
	go vet ./...
	golangci-lint run

bench: ## Run benchmarks
	go test -run '^$$' -bench . -benchmem ./...

cover: ## Run tests with coverage and print a summary
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

tidy: ## Tidy go.mod/go.sum
	go mod tidy

clean: ## Remove build artifacts
	rm -rf bin coverage.out

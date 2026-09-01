BINARY := context-guru-proxy
PKG := github.com/rossoctl/context-guru
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT)

# CGO is on for DEVELOPMENT, and the reason is `go test -race`, which does not work
# without it. It is NOT a requirement of the shipped binary: tree-sitter is the only cgo
# dependency in the tree and it is behind the `cg_skeleton` build tag, so a default build
# is pure Go, and `make build` now builds it that way — so the documented "no C toolchain" claim
# is true of the command the docs actually tell you to run, which it was not while this variable
# applied to every target. CI proves it with no compiler on PATH at all (the `purego` job).
#
# Reading this variable as a SHIPPING requirement is what put "install a C toolchain" at the top of
# our own quickstart, and named bifrost's tokenizer as a cgo dependency in docs/setup.md, which it
# never was. It is needed for `go test -race` and for `-tags cg_skeleton`; nothing else.
export CGO_ENABLED=1

.DEFAULT_GOAL := help

.PHONY: help
help: ## Display this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the context-guru-proxy binary into ./bin (pure Go — no C toolchain needed)
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/context-guru-proxy

.PHONY: build-static
build-static: ## Same as build, plus -trimpath — the exact build releases ship
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/context-guru-proxy
	@file bin/$(BINARY) 2>/dev/null || true

.PHONY: test
test: ## Run all tests with the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run all tests with race + cross-package coverage; write coverage.out and print total
	go test -race -coverpkg=./... -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w .
	go mod tidy

.PHONY: lint
lint: ## Run go vet and gofmt check
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:" && gofmt -l . && exit 1)

.PHONY: pre-commit
pre-commit: ## Install pre-commit hooks
	pre-commit install --install-hooks --hook-type commit-msg

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin

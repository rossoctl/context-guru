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

.PHONY: build-record
build-record: ## Build with the session recorder (--record, --record-otlp) into ./bin; the default build has none
	@mkdir -p bin
	CGO_ENABLED=0 go build -tags cg_record -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/context-guru-proxy

.PHONY: build-static
build-static: ## Same as build, plus -trimpath — the exact build releases ship
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/context-guru-proxy
	@file bin/$(BINARY) 2>/dev/null || true

# The dash package took 1140 s on CI (2026-11-11), so 20m was too close to the limit.
.PHONY: test
test: ## Run all tests with the race detector
	go test -race -timeout 40m ./...

.PHONY: datadep
datadep: ## Rebuild docs/data-dependence.md from the row files in docs/data-dependence/
	go run ./cmd/gen-datadep

.PHONY: cover
cover: ## Run all tests with race + cross-package coverage; write coverage.out and print total
	go test -race -timeout 40m -coverpkg=./... -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

# NPM_REGISTRY overrides the npm registry for this build only, e.g. when the default one rejects
# the stored login: make bob-statusbar NPM_REGISTRY=https://registry.npmjs.org/
NPM_REGISTRY ?=
.PHONY: bob-statusbar
bob-statusbar: ## Build the Bob status bar extension into bob-plugin/context-guru/statusbar/context-guru-bob.vsix
	# --ignore-scripts: no dependency's install script runs; the build and the .vsix do not need one.
	cd bob-plugin/statusbar && npm ci --ignore-scripts --no-audit --no-fund $(if $(NPM_REGISTRY),--registry=$(NPM_REGISTRY)) \
		&& npm run build && npm run package

# Both Mac binaries are pure Go, so one machine builds both.
# BOB_DIST: where the zip goes. The release workflow builds it outside dist/, before GoReleaser,
# whose --clean empties dist/.
BOB_DIST ?= dist
BOB_RELEASE_BUILD := $(BOB_DIST)/bob-build
.PHONY: bob-release
bob-release: bob-statusbar ## Build the Bob release zip into $(BOB_DIST) (both Mac binaries, plugin, status bar)
	@for arch in arm64 amd64; do \
		CGO_ENABLED=0 GOOS=darwin GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(BOB_RELEASE_BUILD)/darwin-$$arch/$(BINARY) ./cmd/context-guru-proxy || exit 1; \
	done
	sh bob-plugin/release/package.sh $(VERSION) $(BOB_DIST) $(BOB_RELEASE_BUILD)/darwin-arm64/$(BINARY) \
		$(BOB_RELEASE_BUILD)/darwin-amd64/$(BINARY) bob-plugin/context-guru

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

.PHONY: refresh-litellm-snapshot
refresh-litellm-snapshot: ## Refresh the bundled LiteLLM price snapshot used by modelinfo tests
	scripts/refresh-litellm-snapshot.sh

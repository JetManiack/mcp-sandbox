SHELL := /bin/bash

BINARY_NAME        := executor
LOCAL_BINARY_NAME  := executor-local
WORKER_BINARY_NAME := worker
BIN_DIR            := bin
CMD_DIR            := ./cmd/executor
LOCAL_CMD_DIR      := ./cmd/executor-local
WORKER_CMD_DIR     := ./cmd/worker

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GOLANGCI_LINT_VERSION := latest
STATICCHECK_VERSION   := latest
GOSEC_VERSION         := latest
GOVULNCHECK_VERSION   := latest

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help message
	@echo "Available targets:" && echo && \
	awk 'BEGIN {FS = ":.*?## "}; /^[a-zA-Z0-9_\-]+:.*?## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST) | sort

.PHONY: build
build: build-server build-worker build-local ## Build all three binaries into bin/

.PHONY: build-server
build-server: generate ## Build the multi-user HTTP server into bin/executor (regenerates the frontend bundle first)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN_DIR)/$(BINARY_NAME) $(CMD_DIR)

# No `generate` dependency either: the worker serves no HTTP at all. It holds the
# sandboxes and runs the commands; everything human-facing stays on the server.
.PHONY: build-worker
build-worker: ## Build the execution worker into bin/worker (no frontend, no database)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN_DIR)/$(WORKER_BINARY_NAME) $(WORKER_CMD_DIR)

# No `generate` dependency: the local helper has no web UI, so nothing to bundle.
.PHONY: build-local
build-local: ## Build the local stdio helper into bin/executor-local (no frontend, no database)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN_DIR)/$(LOCAL_BINARY_NAME) $(LOCAL_CMD_DIR)

# DEV_WORKER_TOKEN is shared by `run` and `run-worker` so the two halves find each
# other in a fresh checkout. It is a fixed string on purpose: these targets already
# run with stub auth and are development-only. A deployment gets a real secret.
DEV_WORKER_TOKEN := dev-worker-token

.PHONY: run
run: generate ## Run the server locally with stub auth (DB_DSN=data/executor.db, use ARGS="--flag=value" for extra flags); needs `make run-worker` alongside it to execute anything
	AUTH_STUB=true DB_DSN="data/executor.db" WORKER_TOKEN="$(DEV_WORKER_TOKEN)" go run $(CMD_DIR) $(ARGS)

# Nothing runs without this: the server holds no sandbox of its own, so a tool
# call with no worker connected reports exactly that.
.PHONY: run-worker
run-worker: ## Run a worker against the local server, sandboxes under data/sandboxes (run this in a second terminal next to `make run`)
	@mkdir -p data/sandboxes
	SERVER_URL="http://localhost:8080" WORKER_TOKEN="$(DEV_WORKER_TOKEN)" WORKER_ID="worker-dev" \
	SANDBOX_DIR="data/sandboxes" go run $(WORKER_CMD_DIR) $(ARGS)

.PHONY: run-local
run-local: ## Run the local stdio helper in this directory (it reads MCP frames on stdin, so a bare terminal will look idle)
	go run $(LOCAL_CMD_DIR) $(ARGS)

FRONTEND_VENDOR_DIR := internal/frontend/static/js/vendor
FRONTEND_FONTS_DIR  := internal/frontend/static/fonts

.PHONY: generate frontend vendor-frontend-js vendor-frontend-fonts
generate: web/node_modules vendor-frontend-js vendor-frontend-fonts ## Generate the frontend bundle (go generate -> esbuild) and vendor pinned JS/font deps
	@command -v esbuild >/dev/null || (echo "esbuild not found. Install: 'brew install esbuild' or 'npm i -g esbuild'"; exit 1)
	go generate ./internal/frontend/...

# esbuild resolves react/react-dom imports from web/node_modules; without
# this the bundle step fails on a bare checkout (CI and the Docker build).
web/node_modules: web/package-lock.json
	npm ci --prefix web
	@touch web/node_modules

frontend: generate ## Alias for generate

vendor-frontend-js: $(FRONTEND_VENDOR_DIR)/react.production.min.js $(FRONTEND_VENDOR_DIR)/react-dom.production.min.js ## Download & checksum-verify pinned JS deps (react, react-dom), served from our own origin instead of unpkg.com

$(FRONTEND_VENDOR_DIR)/react.production.min.js: URL := https://unpkg.com/react@18.3.1/umd/react.production.min.js
$(FRONTEND_VENDOR_DIR)/react.production.min.js: SHA256 := d949f1c3687aedadcedac85261865f29b17cd273997e7f6b2bfc53b2f9d4c4dd
$(FRONTEND_VENDOR_DIR)/react-dom.production.min.js: URL := https://unpkg.com/react-dom@18.3.1/umd/react-dom.production.min.js
$(FRONTEND_VENDOR_DIR)/react-dom.production.min.js: SHA256 := 35f4f974f4b2bcd44da73963347f8952e341f83909e4498227d4e26b98f66f0d

# Fetches URL to $@ and hard-fails the build on a checksum mismatch, rather
# than baking a substituted (e.g. CDN-hijacked) response into the image —
# this is the integrity guarantee that used to live in <script
# integrity="..."> tags, now enforced once at build time instead of on every
# page load.
$(FRONTEND_VENDOR_DIR)/%.js:
	@mkdir -p $(FRONTEND_VENDOR_DIR)
	curl -fsSL -o $@ $(URL)
	@actual=$$(openssl dgst -sha256 $@ | awk '{print $$NF}'); \
	if [ "$$actual" != "$(SHA256)" ]; then \
		echo "checksum mismatch for $@: expected $(SHA256), got $$actual"; rm -f $@; exit 1; \
	fi

vendor-frontend-fonts: $(FRONTEND_FONTS_DIR)/space-grotesk.woff2 $(FRONTEND_FONTS_DIR)/ibm-plex-sans.woff2 $(FRONTEND_FONTS_DIR)/jetbrains-mono.woff2 ## Download & checksum-verify pinned fonts (Space Grotesk, IBM Plex Sans, JetBrains Mono), served from our own origin instead of fonts.googleapis.com/fonts.gstatic.com

# Google Fonts serves one variable-weight woff2 per family for a multi-weight
# request like ours (:wght@500;600;700) — that's why a single file per family
# below covers every weight index.html uses, and why there's only one target
# per family rather than one per weight. Latin subset only: this app's UI text
# is English, and the design's font stack already falls back to system fonts
# for anything outside it.
$(FRONTEND_FONTS_DIR)/space-grotesk.woff2: URL := https://fonts.gstatic.com/s/spacegrotesk/v22/V8mDoQDjQSkFtoMM3T6r8E7mPbF4Cw.woff2
$(FRONTEND_FONTS_DIR)/space-grotesk.woff2: SHA256 := 0640890476fc1198ab4de571fb658de443c4d85b66466ec09534a8737ab1ce9d
$(FRONTEND_FONTS_DIR)/ibm-plex-sans.woff2: URL := https://fonts.gstatic.com/s/ibmplexsans/v23/zYXzKVElMYYaJe8bpLHnCwDKr932-G7dytD-Dmu1syxeKYY.woff2
$(FRONTEND_FONTS_DIR)/ibm-plex-sans.woff2: SHA256 := e2291e842cf5af167122a22881a740c7f2dda7716f1e8cd76680264f4a859470
$(FRONTEND_FONTS_DIR)/jetbrains-mono.woff2: URL := https://fonts.gstatic.com/s/jetbrainsmono/v24/tDbv2o-flEEny0FZhsfKu5WU4zr3E_BX0PnT8RD8yKwBNntkaToggR7BYRbKPxDcwg.woff2
$(FRONTEND_FONTS_DIR)/jetbrains-mono.woff2: SHA256 := 83c005d49d8a6a50474c73a5a36ac0468076e9c4a29da7bdb14995d80560a5be

$(FRONTEND_FONTS_DIR)/%.woff2:
	@mkdir -p $(FRONTEND_FONTS_DIR)
	curl -fsSL -o $@ $(URL)
	@actual=$$(openssl dgst -sha256 $@ | awk '{print $$NF}'); \
	if [ "$$actual" != "$(SHA256)" ]; then \
		echo "checksum mismatch for $@: expected $(SHA256), got $$actual"; rm -f $@; exit 1; \
	fi

.PHONY: test
test: generate ## Run all tests (regenerates the frontend bundle first)
	go test ./...

.PHONY: test-race
test-race: generate ## Run all tests with the race detector
	go test -race ./...

.PHONY: fmt
fmt: ## Format all Go code
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

# Where `go install` puts tool binaries. Resolving each tool through `command
# -v` with this as the fallback is what keeps these targets working on a
# machine that has never added GOPATH/bin to PATH — otherwise the install
# succeeds and the very next line fails with "no such file or directory".
GOPATH_BIN := $(shell go env GOPATH)/bin
tool = $$(command -v $(1) 2>/dev/null || echo $(GOPATH_BIN)/$(1))

# Tools have to be BUILT with a toolchain at least as new as this module's `go`
# directive, or they can't load our packages at all ("package requires newer Go
# version"). Pinning to `go env GOVERSION` is the trap: that's whatever Go
# happens to be installed locally, which is routinely older than go.mod's
# target, and the go command then quietly upgrades only as far as the *tool's*
# own requirement. So derive the pin from go.mod instead. GOTOOLCHAIN needs a
# full patch version (go1.26.0, not go1.26), hence the .0.
MODULE_GO_VERSION := $(shell go list -m -f '{{.GoVersion}}')
TOOL_GOTOOLCHAIN  := GOTOOLCHAIN=go$(MODULE_GO_VERSION)$(if $(word 3,$(subst ., ,$(MODULE_GO_VERSION))),,.0)

.PHONY: lint
lint: ## Run golangci-lint (installed automatically if missing)
	@command -v golangci-lint >/dev/null 2>&1 || \
		$(TOOL_GOTOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(call tool,golangci-lint) run ./...

.PHONY: gosec
gosec: ## Run gosec (code vulnerability scan, installed automatically if missing)
	@command -v gosec >/dev/null 2>&1 || \
		$(TOOL_GOTOOLCHAIN) go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)
	$(call tool,gosec) ./...

.PHONY: govulncheck
govulncheck: ## Run govulncheck (scans dependencies for known vulnerabilities, installed automatically if missing)
	@command -v govulncheck >/dev/null 2>&1 || \
		$(TOOL_GOTOOLCHAIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	$(call tool,govulncheck) ./...

.PHONY: staticcheck
staticcheck: ## Run staticcheck (static code analysis, installed automatically if missing)
	@command -v staticcheck >/dev/null 2>&1 || \
		$(TOOL_GOTOOLCHAIN) go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	$(call tool,staticcheck) ./...

.PHONY: security
security: gosec govulncheck staticcheck ## Run all security and static-analysis checks

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

.PHONY: vendor
vendor: tidy ## Re-vendor dependencies
	go mod vendor

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
	rm -f internal/frontend/static/js/app.bundle.js
	rm -rf $(FRONTEND_VENDOR_DIR)

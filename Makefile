BINARY      := restorelab
PKG         := github.com/restorelab/restorelab
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

GO          ?= go
BIN_DIR     := bin
DIST_DIR    := dist

# What a release ships.
#
# linux/amd64 is the one that matters: it is where Proxmox runs, and where a
# drill is launched from. The rest cost one line each.
#
# Every archive is a .tar.gz, Windows included, so this target needs nothing
# but tar and works the same on every machine. Windows has shipped bsdtar
# since Windows 10, so nothing is lost by not producing a zip.
PLATFORMS   := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# golangci-lint is run from source, with this project's own Go toolchain.
#
# It refuses to analyse a module whose Go version is newer than the one it was
# built with, so a prebuilt binary goes stale the moment go.mod moves ahead of
# it - which is exactly how CI failed the first time it ran. Building it here
# makes that impossible, and makes `make lint` the single command developers
# and CI both run.
GOLANGCI    := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the restorelab binary into bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/restorelab

.PHONY: install
install: ## Install restorelab into GOBIN
	$(GO) install -trimpath -ldflags "$(LDFLAGS)" ./cmd/restorelab

.PHONY: test
test: ## Run the test suite
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run tests with the race detector
	$(GO) test -race ./...

# The store runs one query set against two engines, and only the conformance
# suite proves they behave the same. SQLite is embedded and runs on every
# `make test`; PostgreSQL needs a server, so it is skipped unless one is
# pointed at. This target provides the server, so "I did not have a
# PostgreSQL handy" stops being the reason half the promise goes unchecked.
.PHONY: test-postgres
test-postgres: ## Run the store conformance suite against a throwaway PostgreSQL
	docker run -d --rm --name restorelab-test-pg \
		-e POSTGRES_USER=restorelab -e POSTGRES_PASSWORD=restorelab -e POSTGRES_DB=history \
		-p 55432:5432 postgres:17-alpine
	@echo "waiting for postgres..."
	@until docker exec restorelab-test-pg pg_isready -U restorelab >/dev/null 2>&1; do sleep 1; done
	-RESTORELAB_TEST_DATABASE_URL="postgres://restorelab:restorelab@127.0.0.1:55432/history?sslmode=disable" \
		$(GO) test ./internal/store/ -count=1 -v -run Postgres
	docker stop restorelab-test-pg

.PHONY: cover
cover: ## Run tests and open the coverage report
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format the codebase
	gofmt -w -s $$(find . -name '*.go' -not -path './web/*')

.PHONY: fmt-check
fmt-check: ## Fail when the codebase is not gofmt-clean
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './web/*')); \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

.PHONY: lint
lint: ## Run golangci-lint, built from source with this project's Go
	$(GO) run $(GOLANGCI) run --timeout=10m ./...

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	$(GO) mod tidy

.PHONY: check
check: fmt-check vet test ## Everything CI runs

.PHONY: dist
dist: ## Cross-compile the release archives and their checksums into dist/
	@rm -rf $(DIST_DIR) && mkdir -p $(DIST_DIR)
	@echo "building $(VERSION)"
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		name="$(BINARY)_$(VERSION)_$${os}_$${arch}"; \
		stage="$(DIST_DIR)/$$name"; \
		echo "  $$os/$$arch"; \
		mkdir -p "$$stage"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "$(LDFLAGS)" -o "$$stage/$(BINARY)$$ext" ./cmd/restorelab; \
		cp LICENSE README.md "$$stage/"; \
		( cd $(DIST_DIR) && tar -czf "$$name.tar.gz" "$$name" ); \
		rm -rf "$$stage"; \
	done
	@cd $(DIST_DIR) && sha256sum *.tar.gz > SHA256SUMS
	@echo "$(DIST_DIR)/ holds $$(ls $(DIST_DIR)/*.tar.gz | wc -l) archive(s) and their checksums"

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out

.PHONY: docker
docker: ## Build the container image
	docker build -f deployments/docker/Dockerfile -t restorelab:$(VERSION) .

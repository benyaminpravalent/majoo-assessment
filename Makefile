# Developer commands for the Majoo blog API.
#
# Run `make help` for the list. Every target here is also written out as a plain
# command in the README, because `make` is not installed everywhere — notably
# not on a stock Windows machine, which is where this project was developed.

SHELL := /bin/sh

BINARY        := blogapi
MIGRATE_BIN   := blogapi-migrate
BIN_DIR       := bin
COVERAGE_FILE := coverage.out
COVERAGE_HTML := coverage.html

# Stamped into the binary and the image. Falls back to "dev" outside a git
# checkout, so a build never claims a version it does not have.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The integration suite needs a real database and is skipped without this.
TEST_DATABASE_URL ?= postgres://blog:blog@localhost:5432/blog_test?sslmode=disable

.DEFAULT_GOAL := help

## help: list the available targets
.PHONY: help
help:
	@echo "Majoo blog API — $(VERSION)"
	@echo
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | awk -F': ' '{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

## build: compile both binaries into ./bin
.PHONY: build
build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/api
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/$(MIGRATE_BIN) ./cmd/migrate
	@echo "built $(BIN_DIR)/$(BINARY) and $(BIN_DIR)/$(MIGRATE_BIN) ($(VERSION))"

## run: run the API from source, reading .env if present
.PHONY: run
run:
	go run ./cmd/api

## clean: remove build and coverage artefacts
.PHONY: clean
clean:
	rm -rf $(BIN_DIR) $(COVERAGE_FILE) $(COVERAGE_HTML)

# ---------------------------------------------------------------------------
# Quality gates
# ---------------------------------------------------------------------------

## fmt: format all Go source
.PHONY: fmt
fmt:
	gofmt -w .

## fmt-check: fail if any file is not gofmt-clean
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "these files are not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "all files are gofmt-clean"

## vet: run go vet over every package, including the tagged integration tests
.PHONY: vet
vet:
	go vet ./...
	go vet -tags=integration ./...

## lint: run golangci-lint if it is installed, otherwise say so and continue
.PHONY: lint
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint is not installed; skipping."; \
		echo "install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
	fi

## tidy: verify go.mod and go.sum are complete and minimal
.PHONY: tidy
tidy:
	go mod tidy
	go mod verify

## check: fmt-check, vet and the unit suite — the pre-commit gate
.PHONY: check
check: fmt-check vet test

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

## test: run the unit and HTTP suites (no database required)
.PHONY: test
test:
	go test -count=1 ./...

## test-race: run the suite under the race detector (needs a C toolchain)
.PHONY: test-race
test-race:
	CGO_ENABLED=1 go test -race -count=1 ./...

## test-integration: run the database-backed suite against TEST_DATABASE_URL
.PHONY: test-integration
test-integration:
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" \
		go test -tags=integration -count=1 -v ./tests/integration/...

## coverage: produce coverage.out and print the per-package summary
.PHONY: coverage
coverage:
	go test -count=1 -covermode=atomic -coverprofile=$(COVERAGE_FILE) ./...
	@echo
	@go tool cover -func=$(COVERAGE_FILE) | tail -1

## coverage-html: render coverage.out as a browsable report
.PHONY: coverage-html
coverage-html: coverage
	go tool cover -html=$(COVERAGE_FILE) -o $(COVERAGE_HTML)
	@echo "wrote $(COVERAGE_HTML)"

# ---------------------------------------------------------------------------
# Database
# ---------------------------------------------------------------------------

## migrate-up: apply all pending migrations
.PHONY: migrate-up
migrate-up:
	go run ./cmd/migrate up

## migrate-down: revert the newest migration (override with STEPS=n)
.PHONY: migrate-down
migrate-down:
	go run ./cmd/migrate down $(or $(STEPS),1)

## migrate-status: show applied and pending migrations
.PHONY: migrate-status
migrate-status:
	go run ./cmd/migrate status

# ---------------------------------------------------------------------------
# Docker
# ---------------------------------------------------------------------------

## docker-build: build the runtime image
.PHONY: docker-build
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t majoo-blog-api:$(VERSION) -t majoo-blog-api:latest .

## up: start PostgreSQL, run migrations and start the API
.PHONY: up
up:
	docker compose up --build -d
	@echo "API on http://localhost:8080 — reference at http://localhost:8080/docs"

## down: stop the stack, keeping the database volume
.PHONY: down
down:
	docker compose down

## down-clean: stop the stack and delete the database volume
.PHONY: down-clean
down-clean:
	docker compose down -v

## logs: follow the API container's logs
.PHONY: logs
logs:
	docker compose logs -f api

# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------

## smoke: exercise the full API flow against a running server with curl
.PHONY: smoke
smoke:
	./scripts/smoke.sh

## verify: everything that can be checked without a database
.PHONY: verify
verify: fmt-check vet tidy coverage
	@echo
	@echo "static checks and the unit suite passed."
	@echo "for the database-backed checks: make up && make test-integration && make smoke"

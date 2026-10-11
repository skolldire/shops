GO ?= go
COMPOSE ?= docker compose
BIN ?= bin/api
DEV_CONFIG ?= config/config.local.yaml
SCHEMA := db/init/001_schema.sql
GOLANGCI_LINT_VERSION ?= v2.14.0
GOVULNCHECK_VERSION ?= v1.8.0

.PHONY: build run test test-integration coverage lint lint-comments lint-arch lint-decimals tidy-check vuln up up-debug down schema logs

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/api

run:
	CONFIG_FILE=$(DEV_CONFIG) $(GO) run ./cmd/api

test:
	$(GO) test -race ./...

test-integration:
	DOCKER_HOST="$${DOCKER_HOST:-$$(docker context inspect --format '{{.Endpoints.docker.Host}}')}" \
	TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE="$${TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE:-/var/run/docker.sock}" \
	$(GO) test -race -tags=integration ./...

coverage:
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

lint: lint-comments lint-arch lint-decimals tidy-check
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

lint-comments:
	@$(GO) run ./internal/tools/nocomments
	@! grep -rn -- '--' db/init || { echo "SQL comments are not allowed"; exit 1; }
	@! grep -rnE '^[[:space:]]*#' Makefile Dockerfile .gitignore .dockerignore .golangci.yml .coderabbit.yaml osv-scanner.toml compose*.yaml config deploy .github --include='*' --exclude='*.md' || { echo "# comments are not allowed"; exit 1; }
	@! grep -rn '<!--' --include='*.html' internal || { echo "HTML comments are not allowed"; exit 1; }
	@! grep -rn '/[*]' --include='*.css' internal || { echo "CSS comments are not allowed"; exit 1; }

lint-arch:
	@deps="$$($(GO) list -deps ./internal/catalog/internal/core)" || { echo "go list failed for catalog core"; exit 1; }; \
	! printf '%s\n' "$$deps" | grep -E '^(github.com/jackc/pgx|net/http$$|database/sql$$|github.com/go-chi|go.opentelemetry.io|github.com/skolldire/shops/internal/platform/(httpx|telemetry|database))' || { echo "catalog core must not depend on transport, persistence or telemetry"; exit 1; }
	@deps="$$($(GO) list -deps ./internal/platform/...)" || { echo "go list failed for platform"; exit 1; }; \
	! printf '%s\n' "$$deps" | grep -E '^github.com/skolldire/shops/internal/(catalog|web|ordering|payment)' || { echo "platform must not depend on business modules"; exit 1; }

lint-decimals:
	@! grep -rln --include='*.go' 'decimal.NewFromString' internal | grep -vE '^internal/catalog/internal/(core/decimal\.go|postgres/)' || { echo "parse decimals with catalog.ParseDecimal or core.ParseDecimal, not decimal.NewFromString"; exit 1; }

tidy-check:
	$(GO) mod tidy -diff

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

up:
	$(COMPOSE) up --build

up-debug:
	$(COMPOSE) -f compose.yaml -f compose.debug.yaml up --build

down:
	$(COMPOSE) -f compose.yaml -f compose.debug.yaml down -v --remove-orphans

schema:
	psql -v ON_ERROR_STOP=1 -f $(SCHEMA)

logs:
	$(COMPOSE) logs -f

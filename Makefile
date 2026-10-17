GO ?= go
COMPOSE ?= docker compose
BIN ?= bin/api
DEV_CONFIG ?= config/config.local.yaml
SCHEMA := db/init/001_schema.sql
GOLANGCI_LINT_VERSION ?= v2.14.0
GOVULNCHECK_VERSION ?= v1.8.0

.PHONY: build run test test-integration coverage lint lint-comments lint-arch lint-decimals tidy-check vuln up up-replicas up-debug down schema logs

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
	@! grep -rnE '^[[:space:]]*#' Makefile Dockerfile db/Dockerfile .gitignore .dockerignore .golangci.yml .coderabbit.yaml osv-scanner.toml compose*.yaml config deploy .github --include='*' --exclude='*.md' || { echo "# comments are not allowed"; exit 1; }
	@! grep -rn '<!--' --include='*.html' internal || { echo "HTML comments are not allowed"; exit 1; }
	@! grep -rn '/[*]' --include='*.css' internal || { echo "CSS comments are not allowed"; exit 1; }

CATALOG := github[.]com/skolldire/shops/internal/catalog/internal
VALIDATION := github[.]com/skolldire/shops/internal/platform/validation
DOMAIN_IMPORTS := errors|regexp|strconv|strings|time|unicode/utf8|github[.]com/shopspring/decimal|$(VALIDATION)
APP_IMPORTS := cmp|context|errors|fmt|regexp|slices|strconv|strings|$(VALIDATION)|$(CATALOG)/domain
FORBIDDEN_DEPS := github[.]com/jackc/pgx|net/http$$|net/url$$|database/sql$$|github[.]com/go-chi|go[.]opentelemetry[.]io|github[.]com/skolldire/shops/internal/platform/(httpx|telemetry|database|logger)
ORDERING := github[.]com/skolldire/shops/internal/ordering/internal
ORDERING_DOMAIN_IMPORTS := crypto/sha256|encoding/hex|errors|regexp|slices|strconv|strings|time|github[.]com/shopspring/decimal|$(VALIDATION)
ORDERING_APP_IMPORTS := context|errors|fmt|slices|time|github[.]com/shopspring/decimal|$(VALIDATION)|$(ORDERING)/domain
ORDERING_FORBIDDEN_DEPS := $(FORBIDDEN_DEPS)|github[.]com/skolldire/shops/internal/(catalog|payment)

lint-arch:
	@imports="$$($(GO) list -f '{{join .Imports "\n"}}' ./internal/catalog/internal/domain)" || { echo "go list failed for catalog domain"; exit 1; }; \
	! printf '%s\n' "$$imports" | grep -vE '^($(DOMAIN_IMPORTS))$$' || { echo "catalog domain may only import the packages in DOMAIN_IMPORTS: no use cases, ports, adapters, logging, transport or persistence"; exit 1; }
	@imports="$$($(GO) list -f '{{join .Imports "\n"}}' ./internal/catalog/internal/app)" || { echo "go list failed for catalog app"; exit 1; }; \
	! printf '%s\n' "$$imports" | grep -vE '^($(APP_IMPORTS))$$' || { echo "catalog app may only import the packages in APP_IMPORTS: the domain, but no adapters, file formats, logging, transport or persistence"; exit 1; }
	@deps="$$($(GO) list -deps ./internal/catalog/internal/domain ./internal/catalog/internal/app)" || { echo "go list failed for catalog domain and app"; exit 1; }; \
	! printf '%s\n' "$$deps" | grep -E '^($(FORBIDDEN_DEPS))' || { echo "catalog domain and app must not depend on transport, persistence, logging or telemetry"; exit 1; }
	@deps="$$($(GO) list -deps ./internal/platform/...)" || { echo "go list failed for platform"; exit 1; }; \
	! printf '%s\n' "$$deps" | grep -E '^github.com/skolldire/shops/internal/(catalog|web|ordering|payment)' || { echo "platform must not depend on business modules"; exit 1; }
	@imports="$$($(GO) list -f '{{join .Imports "\n"}}' ./internal/ordering/internal/domain)" || { echo "go list failed for ordering domain"; exit 1; }; \
	! printf '%s\n' "$$imports" | grep -vE '^($(ORDERING_DOMAIN_IMPORTS))$$' || { echo "ordering domain may only import the packages in ORDERING_DOMAIN_IMPORTS: no use cases, ports, adapters, other modules, logging, transport or persistence"; exit 1; }
	@imports="$$($(GO) list -f '{{join .Imports "\n"}}' ./internal/ordering/internal/app)" || { echo "go list failed for ordering app"; exit 1; }; \
	! printf '%s\n' "$$imports" | grep -vE '^($(ORDERING_APP_IMPORTS))$$' || { echo "ordering app may only import the packages in ORDERING_APP_IMPORTS: the domain, but no adapters, other modules, logging, transport or persistence"; exit 1; }
	@deps="$$($(GO) list -deps ./internal/ordering/internal/domain ./internal/ordering/internal/app)" || { echo "go list failed for ordering domain and app"; exit 1; }; \
	! printf '%s\n' "$$deps" | grep -E '^($(ORDERING_FORBIDDEN_DEPS))' || { echo "ordering domain and app must not depend on catalog, payment, transport, persistence, logging or telemetry"; exit 1; }
	@deps="$$($(GO) list -deps ./internal/catalog/... ./internal/payment/...)" || { echo "go list failed for catalog and payment"; exit 1; }; \
	! printf '%s\n' "$$deps" | grep -E '^github[.]com/skolldire/shops/internal/ordering' || { echo "catalog and payment must not depend on ordering"; exit 1; }
	@deps="$$($(GO) list -deps ./internal/payment/...)" || { echo "go list failed for payment"; exit 1; }; \
	! printf '%s\n' "$$deps" | grep -E '^github[.]com/skolldire/shops/internal/(catalog|web)' || { echo "payment must not depend on catalog"; exit 1; }

lint-decimals:
	@! grep -rln --include='*.go' 'decimal.NewFromString' internal | grep -vE '^internal/(catalog/internal/(domain/decimal\.go|postgres/)|ordering/internal/postgres/|payment/internal/fakestore/)' || { echo "parse decimals with catalog.ParseDecimal or domain.ParseDecimal, not decimal.NewFromString"; exit 1; }

tidy-check:
	$(GO) mod tidy -diff

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

up:
	$(COMPOSE) up --build

up-replicas:
	$(COMPOSE) --profile replicas up --build

up-debug:
	$(COMPOSE) -f compose.yaml -f compose.debug.yaml up --build

down:
	$(COMPOSE) -f compose.yaml -f compose.debug.yaml --profile replicas down -v --remove-orphans

schema:
	psql -v ON_ERROR_STOP=1 -f $(SCHEMA)

logs:
	$(COMPOSE) logs -f

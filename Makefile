GO ?= go
COMPOSE ?= docker compose
BIN ?= bin/api
DEV_CONFIG ?= config/config.local.yaml
SCHEMA := db/init/001_schema.sql

.PHONY: build run test test-integration lint up up-debug down schema logs

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

lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...

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

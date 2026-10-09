# shops

E-commerce application built as a modular monolith in Go.

This is phase 1: the platform layer (`internal/platform`) and the runnable skeleton. Business modules (catalog, ordering, payment) and the web UI arrive in later phases.

## Requirements

- Docker with the Compose v2 and Buildx plugins (Docker Desktop includes both)
- Go 1.26 or newer, only to run tests or the binary outside Docker
- `make`

## Run with Docker

```sh
make up
```

| Service | Purpose |
|---|---|
| `secrets-init` | Generates the database password into a named volume on first run, then exits |
| `db` | PostgreSQL 16. `db/init/001_schema.sql` is applied when the data volume is created |
| `app` | The API on `http://localhost:8080` |

```sh
curl -i localhost:8080/health/ready
```

`make down` removes containers, data and the generated password together, so the next `make up` starts clean and the schema is applied again.

### Debug stack

```sh
make up-debug
```

Adds, bound to `127.0.0.1` only:

| Port | What |
|---|---|
| `5432` | PostgreSQL |
| `9100` | Application metrics (`/metrics`) |
| `9090` | Prometheus, scraping the app every 15s |

## Run locally

With the debug stack running:

```sh
DB_PASSWORD="$(docker compose exec -T db cat /run/secrets/db_password)" make run
```

`make run` uses `config/config.local.yaml`.

## Tests and checks

```sh
make lint               # golangci-lint (style, gosec, errorlint...), no comments in code or config, go mod tidy
make test               # unit tests with the race detector
make coverage           # unit tests with a coverage report
make test-integration   # PostgreSQL via Testcontainers
make vuln               # govulncheck
```

Integration tests need a running Docker daemon. When `DOCKER_HOST` is unset, the Makefile uses the endpoint of the active Docker context, which also covers Colima.

## Continuous integration

Every pull request to `master` runs:

| Check | What it enforces |
|---|---|
| Lint and style | golangci-lint, no comments, tidy modules, hadolint on the Dockerfile |
| Unit tests | Race detector on the `go.mod` floor and on the latest stable Go |
| Integration tests | Repositories and transactions against PostgreSQL |
| govulncheck | Known vulnerabilities in reachable code |
| Image scan and end-to-end smoke | Trivy on the image, then the compose stack: readiness, problem+json errors, schema, no password in logs or `docker inspect`, graceful shutdown |
| Dependency review | New dependencies with high-severity advisories |
| CodeQL | Static security analysis, also weekly |

CodeRabbit reviews every pull request with the rules in `.coderabbit.yaml` and approves it once its findings are resolved. Dependabot keeps Go modules, actions and base images up to date.

## Endpoints

| Port | Method and path | Response |
|---|---|---|
| 8080 | `GET /health/live` | `200 {"status":"up"}` while the process responds |
| 8080 | `GET /health/ready` | `200` or `503` with `{"status":"up\|down","checks":{"postgres":"up\|down"}}` |
| 8080 | anything else | `404` or `405` as `application/problem+json` (RFC 9457), with `Allow` on 405 |
| 9100 | `GET /metrics` | Prometheus metrics: HTTP server, Go runtime and process |

## Configuration

The app reads `/etc/shop/config.yaml`, or the path given by `-config` or `CONFIG_FILE`.

A value can be a literal or exactly one placeholder:

| Placeholder | Resolves to |
|---|---|
| `${VAR}` | Environment variable; startup fails if it is unset or empty |
| `${VAR:-default}` | Environment variable, or `default` |
| `${file:/path}` | File contents without the trailing newline |

Secret fields (the database password) must be a placeholder. Unknown keys, invalid values and every failed check are reported together, with the YAML path and without the resolved value.

| Variable | Default in `config.yaml` |
|---|---|
| `HTTP_ADDR` | `:8080` |
| `METRICS_ADDR` | `:9100` |
| `DB_HOST` / `DB_PORT` | `db` / `5432` |
| `LOG_LEVEL` | `info` |
| `TRACING_ENABLED` / `OTLP_ENDPOINT` / `OTLP_INSECURE` | `false` / empty / `false` |
| `APP_VERSION` | `dev` |
| `SEED_DEMO` | `true` |

## Observability

- **Logs:** JSON on stdout with `timestamp`, `severity` and `message`. Every request line carries `request_id`, `trace_id` and `span_id`. Sensitive fields and values (passwords, tokens, bearer credentials, card numbers) are redacted. The configuration is never logged.
- **Metrics:** OpenTelemetry exported in Prometheus format on a separate port that `compose.yaml` does not publish.
- **Traces:** OpenTelemetry over OTLP gRPC, disabled by default. Incoming `traceparent` headers are honoured, and spans are named after the matched route.

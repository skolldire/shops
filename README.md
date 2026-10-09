# shops

E-commerce application built as a modular monolith in Go.

It currently ships the platform layer (`internal/platform`), the product catalog with its JSON API (`internal/catalog`) and a server-rendered UI to browse and manage products (`internal/web`). Ordering, payments and CSV import arrive in later phases.

## Requirements

- Docker with the Compose v2 and Buildx plugins (Docker Desktop includes both)
- Go as declared in `go.mod` (currently 1.27.2), only to run tests or the binary outside Docker
- `make`

## Run with Docker

```sh
make up
```

| Service | Purpose |
|---|---|
| `secrets-init` | Generates the database password into a named volume on first run, then exits |
| `db` | PostgreSQL 18. `db/init/001_schema.sql` is applied when the data volume is created |
| `app` | The store, the admin UI and the API on `http://localhost:8080` |

```sh
curl -i localhost:8080/health/ready
```

Open `http://localhost:8080` for the store and `http://localhost:8080/admin/products` to manage products.

`make down` removes containers, data and the generated password together, so the next `make up` starts clean and the schema is applied again.

### Upgrading an existing stack

There are no data migrations: `db/init/001_schema.sql` only runs when the data volume is created. Phase 2 adds the `version` column to `products` and moves to PostgreSQL 18, so a stack created before it must be recreated:

```sh
make down && make up
```

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
| Lint and style | golangci-lint, no comments (Go, SQL, YAML, HTML, CSS), module boundaries, tidy modules, hadolint on the Dockerfile |
| Unit tests | Race detector on the `go.mod` version and on the latest stable Go |
| Integration tests | Repositories, transactions and an HTTP acceptance test of the product lifecycle against PostgreSQL |
| govulncheck | Known vulnerabilities in reachable code |
| Image scan and end-to-end smoke | Trivy on the image, then the compose stack: readiness, problem+json errors, schema, no password in logs or `docker inspect`, graceful shutdown |
| Dependency review | OSV-Scanner compares the base branch with the pull request and fails on newly introduced vulnerable dependencies. Accepted exceptions live in `osv-scanner.toml` with a reason and an expiry date |
| CodeQL | Static security analysis, also weekly |

CodeRabbit reviews every pull request with the rules in `.coderabbit.yaml` and approves it once its findings are resolved. Dependabot keeps Go modules, actions and base images up to date.

## Endpoints

| Port | Method and path | Response |
|---|---|---|
| 8080 | `GET /health/live` | `200 {"status":"up"}` while the process responds |
| 8080 | `GET /health/ready` | `200` or `503` with `{"status":"up\|down","checks":{"postgres":"up\|down"}}` |
| 8080 | `/api/v1/...` | The product API, described below. Unknown routes answer `404` or `405` as `application/problem+json` (RFC 9457), with `Allow` on 405 |
| 8080 | anything else | The web UI. Unknown pages answer an HTML `404` |
| 9100 | `GET /metrics` | Prometheus metrics: HTTP server, Go runtime and process |

## Product API

| Method and path | Success | Notes |
|---|---|---|
| `GET /api/v1/products` | `200` with a page | Search parameters below |
| `POST /api/v1/products` | `201` with the product, `Location` and `ETag` | |
| `GET /api/v1/products/{id}` | `200` with the product and `ETag: "<version>"` | |
| `PATCH /api/v1/products/{id}` | `200` with the product and the new `ETag` | Requires `If-Match`. Only the fields sent are changed |
| `DELETE /api/v1/products/{id}` | `204` | Requires `If-Match`. Deletion is logical |
| `GET /api/v1/categories` | `200` with `["Audio","Books",...]` | Distinct categories of active products |

A product:

```json
{
  "id": "2b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b",
  "sku": "RS-001",
  "name": "Running Shoes",
  "description": "Lightweight trainers",
  "category": "Sports",
  "price": "29.99",
  "stock": 10,
  "weight_kg": "0.850",
  "version": 3,
  "created_at": "2026-10-09T17:00:00Z",
  "updated_at": "2026-10-09T17:05:00Z"
}
```

`price` and `weight_kg` are returned as strings with a fixed scale (2 and 3 decimals). Requests accept them as strings or JSON numbers, and they are never converted to floating point.

### Rules

| Field | Rule |
|---|---|
| `sku` | Required, trimmed and upper-cased, 1 to 64 characters `A-Z 0-9 - _`. Unique even after deletion, and immutable |
| `name` | Required, at most 200 characters |
| `description` | Optional, at most 2000 characters |
| `category` | Required, at most 100 characters. Inner spaces are collapsed; case is kept, so `TV` stays `TV` |
| `price` | Greater than 0, at most 2 decimals, at most `9999999999.99` |
| `stock` | Integer between 0 and 2147483647 |
| `weight_kg` | At least 0, at most 3 decimals, less than `10000000` |

Every rule is checked at once and all field errors are returned together.

### Search

`GET /api/v1/products?q=&category=&min_price=&max_price=&in_stock=true&sort=-price&page=1&page_size=20`

| Parameter | Meaning |
|---|---|
| `q` | Up to 100 characters, matched with `ILIKE` against name, SKU and description. `%`, `_` and `\` are literal, so `q=50%25` finds "50%" |
| `category` | Case-insensitive equality |
| `min_price`, `max_price` | Decimal bounds; `min_price` must not exceed `max_price` |
| `in_stock` | `true` returns only products with stock |
| `sort` | `name` (default), `price` or `created_at`, prefixed with `-` for descending. Ties are broken by id so pages are stable |
| `page`, `page_size` | `page` starts at 1; `page_size` is 1 to 100, 20 by default |

The response is `{"items": [...], "page": 1, "page_size": 20, "total": 137}`.

### Optimistic concurrency

Every product has a `version`, exposed as the `ETag`. Updates and deletions must send it back:

```sh
curl -i -X PATCH localhost:8080/api/v1/products/$ID \
  -H 'If-Match: "3"' -H 'Content-Type: application/json' -d '{"stock":7}'
```

If someone changed the product in the meantime, the request fails with `412` instead of overwriting their change.

### Errors

All errors are `application/problem+json` with a stable `code`:

| Case | Status | `code` |
|---|---|---|
| Malformed JSON or unknown fields | 400 | `invalid_request` |
| Body larger than 1 MB | 413 | `request_too_large` |
| Domain rules, with an `errors` list per field | 422 | `validation_failed` |
| Unknown, deleted or malformed product id | 404 | `not_found` |
| SKU already used | 409 | `sku_taken` |
| `If-Match` does not match the current version | 412 | `version_conflict` |
| `PATCH` or `DELETE` without `If-Match` | 428 | `precondition_required` |

Field errors use stable codes: `required`, `too_long`, `not_positive`, `too_many_decimals`, `out_of_range`, `invalid_format` and `immutable`.

## Web UI

| Page | What it does |
|---|---|
| `/` | Store with search, category, price range, in-stock filter, sorting and pagination. Results update with htmx without reloading, and the filters stay in the URL so searches can be shared and the Back button works |
| `/products/{id}` | Product details |
| `/admin/products` | Product table with search, edit and delete actions |
| `/admin/products/new` | Create a product. Validation errors appear next to each field |
| `/admin/products/{id}/edit` | Edit a product. The form sends the version it showed; if the product changed meanwhile, the page explains it and offers to reload |
| `/admin/products/{id}/delete` | Server-rendered confirmation before deleting |

The UI uses `html/template` and htmx 2.0.11, both embedded in the binary, so it works without internet access. Pages are served with a strict Content-Security-Policy (no inline scripts or styles), and htmx is configured so it does not inject inline styles.

## Known limitations

- **Search does not use an index.** `ILIKE '%q%'` scans the active products. This is fine for thousands of products; a larger catalog would need full-text search or trigram indexes.
- **No CSRF protection.** There is no authentication and no session cookie yet, so there are no credentials another site could ride on. CSRF tokens become necessary as soon as authentication is added.
- **No data migrations, by design.** Pre-production data is disposable: it is recreated from an initial data load after `make down && make up`. Production skips that initial load.

## Configuration

The app reads `/etc/shop/config.yaml`, or the path given by `-config` or `CONFIG_FILE`.

A value can be a literal or exactly one placeholder:

| Placeholder | Resolves to |
|---|---|
| `${VAR}` | Environment variable; startup fails if it is unset or empty |
| `${VAR:-default}` | Environment variable, or `default` |
| `${file:/path}` | File contents without the trailing newline |

Secret fields (the database password) must be a placeholder without a default, so a password can never be written in the file. Unknown keys, invalid values and every failed check are reported together, with the YAML path and without the resolved value.

| Variable | Default in `config.yaml` |
|---|---|
| `HTTP_ADDR` | `:8080`. The container healthcheck (`/api healthcheck`) probes `/health/live` on this address, or on the URL given with `-url` |
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

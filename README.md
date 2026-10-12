# shops

E-commerce application built as a modular monolith in Go.

It currently ships the platform layer (`internal/platform`), the product catalog with its JSON API and CSV import (`internal/catalog`) and a server-rendered UI to browse and manage products (`internal/web`). Ordering and payments arrive in later phases.

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

Open `http://localhost:8080` for the store and `http://localhost:8080/admin/products` to manage products. The first start loads the demo catalog, 82 products from the official CSV of the challenge (see [Demo data](#demo-data)).

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
| Lint and style | golangci-lint, no comments (Go, SQL, YAML, HTML, CSS), the dependency rules described in [Architecture](#architecture), tidy modules, hadolint on the Dockerfile |
| Unit tests | Race detector on the `go.mod` version and on the latest stable Go |
| Integration tests | Repositories, transactions and an HTTP acceptance test of the product lifecycle against PostgreSQL |
| govulncheck | Known vulnerabilities in reachable code |
| Image scan and end-to-end smoke | Trivy on the image, then the compose stack: readiness, problem+json errors, schema, no password in logs or `docker inspect`, graceful shutdown |
| Dependency review | OSV-Scanner compares the base branch with the pull request and fails on newly introduced vulnerable dependencies. Accepted exceptions live in `osv-scanner.toml` with a reason and an expiry date |
| CodeQL | Static security analysis, also weekly |

CodeRabbit reviews every pull request with the rules in `.coderabbit.yaml` and approves it once its findings are resolved. Dependabot keeps Go modules, actions and base images up to date.

## Architecture

A modular monolith. Each business module is one directory under `internal/` with a public facade (`module.go` and `api.go`) and its own `internal/` packages, so the compiler keeps other modules out of its implementation.

Inside the catalog module the dependencies point towards the business rules:

| Package | Role | May import |
|---|---|---|
| `domain` | Products, search criteria, the decimal and whole number grammars, and their invariants. No ports, no I/O | A closed list: a few standard packages, `shopspring/decimal` and `platform/validation` |
| `app` | Use cases (product service, import) and the ports they need: `Repository`, `ProductUpserter`, `ImportRecorder`, `ImportFailureReporter` | A closed list: `domain`, a few standard packages and `platform/validation` |
| `httpapi`, `httpquery`, `csvimport` | Input adapters: JSON, query strings and CSV files are decoded here and handed over as plain values | `app`, `domain` and `platform` |
| `postgres`, `metrics`, `importlog` | Output adapters: they implement the ports of `app` for PostgreSQL, metrics and operational logs | `app`, `domain` and `platform` |
| `metrics`, `tracing` | Decorators around the import: they time it and wrap it in spans without the use case knowing | `app` and `platform` |
| `module.go` | Composition: builds the adapters, wires them into the use cases and exposes the facade | Everything in the module |

`domain` and `app` do not read files, parse formats, log, measure time or call a telemetry SDK:

- The CSV adapter reads the upload, enforces the size and row limits, hashes the file, maps the columns and reads the numbers. The import use case receives typed product values, together with the fields the adapter could not read, and never sees a CSV cell. The errors that reject a whole file (another header, malformed CSV, too many rows, too many bytes) belong to the CSV adapter; the HTTP handler and the facade translate them.
- When the import recovers from a failed batch, or the database refuses a row, the use case reports it through `ImportFailureReporter` and the `importlog` adapter writes the log line.
- The duration of an import is measured by a decorator in the `metrics` adapter around the whole operation, so it also covers files that are rejected before any row is applied.
- Spans come from decorators in the `tracing` adapter and from the database package of the platform. `domain` and `app` do not import OpenTelemetry.
- The facade takes search parameters as plain text and the seed as a reader; query strings stay in the HTTP adapters and the seed file is opened by `cmd/api`.

`make lint-arch` enforces this on every pull request. It fails if `domain` or `app` import anything outside their closed lists (so `domain` cannot import `app`, and neither can import an adapter), if they reach transport, persistence, logging or telemetry packages even indirectly, or if `internal/platform` imports a business module.

## Endpoints

| Port | Method and path | Response |
|---|---|---|
| 8080 | `GET /health/live` | `200 {"status":"up"}` while the process responds |
| 8080 | `GET /health/ready` | `200` or `503` with `{"status":"up\|down","checks":{"postgres":"up\|down"}}` |
| 8080 | `/api/v1/...` | The product API, described below. Unknown routes answer `404` or `405` as `application/problem+json` (RFC 9457), with `Allow` on 405 |
| 8080 | `/health/...` | Unknown health routes answer `404` as `application/problem+json` |
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

Every decimal the app reads (`price`, `weight_kg`, `min_price`, `max_price`, in the API and in the web forms) must be a plain decimal number such as `29.99`: an optional `-`, 1 to 13 integer digits and, optionally, a `.` followed by 1 to 6 decimals. Surrounding spaces are ignored. Exponents (`1e3`, `2.5E-1`), a leading `+`, `.5`, `5.`, thousands separators, `NaN` and `Infinity` are rejected with `invalid_format`. This also applies to JSON numbers, so `"price": 2.999e1` is rejected even though it is valid JSON.

Whole numbers follow the same idea. `stock`, `page` and `page_size` must be a plain whole number such as `25`: an optional `-` followed by digits, with surrounding spaces ignored. A leading `+`, decimals (`1.0`), exponents (`1e3`) and separators are rejected with `invalid_format`, and a value that does not fit in 32 bits is rejected with `out_of_range`. The API, the web forms and the CSV import share this rule, so the same input gets the same answer everywhere.

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
| `min_price`, `max_price` | Plain decimal bounds; `min_price` must not exceed `max_price` |
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

`If-Match` is compared strongly, as RFC 9110 requires, so a weak entity tag such as `W/"3"` never matches and is rejected with `412`.

### Errors

All errors are `application/problem+json` with a stable `code`:

| Case | Status | `code` |
|---|---|---|
| Malformed JSON or unknown fields | 400 | `invalid_request` |
| Body larger than 1 MB (`/api/v1/imports` has its own limit, see [CSV import](#csv-import)) | 413 | `request_too_large` |
| Domain rules, with an `errors` list per field | 422 | `validation_failed` |
| Unknown, deleted or malformed product id | 404 | `not_found` |
| SKU already used | 409 | `sku_taken` |
| `If-Match` does not match the current version | 412 | `version_conflict` |
| `If-Match` carries a weak entity tag such as `W/"3"` | 412 | `precondition_failed` |
| `PATCH` or `DELETE` without `If-Match` | 428 | `precondition_required` |

Field errors use stable codes: `required`, `too_long`, `not_positive`, `too_many_decimals`, `out_of_range`, `invalid_format` and `immutable`. The import adds `wrong_field_count`, `duplicate_in_file`, `sku_deleted` and `rejected_by_database`.

## CSV import

Products can be loaded from a CSV file through the API or from `/admin/imports`:

```sh
curl -F file=@testdata/products.csv localhost:8080/api/v1/imports
```

The request is `multipart/form-data` with the file in a field named `file`. The file is read as a stream and is never written to disk. The import is synchronous: the response is the report, and nothing about an import is stored.

### The file

- The first line must be exactly `name,sku,description,category,price,stock,weight_kg`, in that order. A UTF-8 byte order mark before it is tolerated.
- UTF-8, comma-separated, RFC 4180: double quotes around fields that contain commas, quotes or line breaks.
- At most 10 MB (`import.max_bytes`) and 10,000 data rows (`import.max_rows`).
- Every row follows the same rules as the API and the admin forms (see [Rules](#rules)), with the same error codes. `price` and `weight_kg` are plain decimals such as `29.99`; `stock` is a whole number such as `25`.
- Line numbers are the physical line where a record starts, counting the header as line 1, so they match what an editor shows even when a field spans several lines.

| Situation | Result |
|---|---|
| Another header, invalid UTF-8, unbalanced quotes or a limit exceeded | The whole file is rejected with `400` or `413` and nothing is applied |
| Row with every field empty after trimming | Ignored and counted in `skipped` |
| Row with a number of fields other than 7 | Rejected with `field: "row"` and `code: "wrong_field_count"` |
| Row that breaks the product rules | Rejected with every field error it has |
| SKU repeated in the file, compared after trimming and upper-casing | Every row of the group is rejected with `code: "duplicate_in_file"`; the message names the other lines |
| SKU of a deleted product | Rejected with `code: "sku_deleted"`; a deleted SKU is never reused |
| SKU of an active product | Updated. `stock` replaces the current value; it is not added to it |
| New SKU | Created |
| Active product that is not in the file | Left as it is |

The file is validated completely before anything is written. The valid rows are then applied in batches of 500, each in its own transaction, so a large file is not all-or-nothing: if the request is cancelled halfway, the batches already committed stay applied. A row that is identical to the stored product is left untouched: its `version` and `updated_at` do not change. So importing the same file again reports as `unchanged` the rows that the first import created or updated, as long as nobody changed those products in between; rows that were skipped or rejected are skipped or rejected again.

An import overrides whatever is stored: it does not use `If-Match`, and it increases `version` when it changes a product. An administrator who has that product open gets the usual conflict when saving.

### The report

`POST /api/v1/imports` answers `200` with the report, also when rows were rejected:

```json
{
  "file": {"name": "products.csv", "bytes": 8930, "sha256": "6fc46b84badb1ac37b8b19a2d0741aa7728d6c93a9fe949b19f676892dc8fd24"},
  "records": 97,
  "skipped": 2,
  "created": 82,
  "updated": 0,
  "unchanged": 0,
  "rejected": 13,
  "errors": [
    {"line": 2, "sku": "RS-001", "field": "sku", "code": "duplicate_in_file", "message": "also used on line 36"},
    {"line": 4, "sku": "WM-042", "field": "price", "code": "invalid_format", "message": "must be a plain decimal number such as 29.99"}
  ]
}
```

`records` is always `skipped + created + updated + unchanged + rejected`. `errors` is sorted by line and then by field. A row with several invalid fields appears once per field and counts once in `rejected`. `sku` is the normalized value, and it is empty for rows with the wrong number of fields. Messages never include the content of the row. The API returns every error; the admin page shows the first 200 and says how many more there are.

Besides the field codes of the product rules, the import uses these codes:

| `code` | `field` | Meaning |
|---|---|---|
| `wrong_field_count` | `row` | The row does not have 7 fields |
| `duplicate_in_file` | `sku` | The SKU is used by more than one row of the file |
| `sku_deleted` | `sku` | The SKU belongs to a deleted product |
| `rejected_by_database` | `row` | The database refused the row. The cause is logged, not reported |

When the whole file is rejected, the answer is `application/problem+json`:

| Case | Status | `code` |
|---|---|---|
| The request is not multipart or has no `file` field | 400 | `invalid_request` |
| Another header | 400 | `invalid_header` |
| Invalid UTF-8 or malformed CSV, with the line in `detail` | 400 | `invalid_csv` |
| More rows than `import.max_rows` | 400 | `too_many_rows` |
| File larger than `import.max_bytes` | 413 | `request_too_large` |

### Demo data

`testdata/products.csv` is the official file of the challenge, unmodified. It was downloaded on October 7, 2026 and its SHA-256 is `6fc46b84badb1ac37b8b19a2d0741aa7728d6c93a9fe949b19f676892dc8fd24`. It has 97 records: 2 empty rows, 82 valid products and 13 rejected rows, among them the repeated SKUs `RS-001` (2 rows) and `BS-021` (3 rows). The acceptance tests import it and check those numbers.

The image ships that file. When `demo.seed` is `true`, the app imports `demo.seed_file` at startup, before it starts serving, and only if the `products` table has no rows at all, deleted ones included. So `make down && make up` gives a populated store, while restarting the app neither imports again nor brings back products that were deleted. The seed goes through the same import as the API, and its summary is logged at `info` level as `demo seed imported`. Rejected rows are not a failure; a seed file that is missing or cannot be read stops the startup.

## Web UI

| Page | What it does |
|---|---|
| `/` | Store with search, category, price range, in-stock filter, sorting and pagination. Results update with htmx without reloading, and the filters stay in the URL so searches can be shared and the Back button works |
| `/products/{id}` | Product details |
| `/admin/products` | Product table with search, edit and delete actions |
| `/admin/products/new` | Create a product. Validation errors appear next to each field |
| `/admin/products/{id}/edit` | Edit a product. The form sends the version it showed; if the product changed meanwhile, the page explains it and offers to reload |
| `/admin/products/{id}/delete` | Server-rendered confirmation before deleting |
| `/admin/imports` | Upload a CSV file. The page shows the counters of the import and a table with the rejected rows: line, SKU, field, code and message. A file that is rejected as a whole is explained on the same page |

The UI uses `html/template` and htmx 2.0.11, both embedded in the binary, so it works without internet access. Pages are served with a strict Content-Security-Policy (no inline scripts or styles), and htmx is configured so it does not inject inline styles.

## Known limitations

- **Search does not use an index.** `ILIKE '%q%'` scans the active products. This is fine for thousands of products; a larger catalog would need full-text search or trigram indexes.
- **The search total is not transactionally consistent with the items.** They come from two separate queries, so with concurrent writes the `total` can differ from the items of a page.
- **An import sets stock to the value in the file.** Stock is absolute, not a delta: once purchases exist, an import can overwrite stock that recent sales had just reduced.
- **No authentication.** Anyone who can reach port 8080 can create, edit, delete and import products through `/api/v1` and `/admin`. Do not expose the published listener to untrusted clients without access control in front of it, such as an authenticating reverse proxy or a private network.
- **No CSRF protection.** There is no session cookie yet, so there are no credentials another site could ride on. CSRF tokens become necessary as soon as authentication is added.
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
| `SEED_DEMO` | `true`. Imports the demo catalog at startup when there are no products |
| `SEED_FILE` | `/etc/shop/seed.csv`, the official CSV shipped in the image. `config.local.yaml` uses `testdata/products.csv` |

## Observability

- **Logs:** JSON on stdout with `timestamp`, `severity` and `message`. Every request line carries `request_id`, `trace_id` and `span_id`. Sensitive fields and values (passwords, tokens, bearer credentials, card numbers) are redacted. The configuration is never logged.
- **Metrics:** OpenTelemetry exported in Prometheus format on a separate port that `compose.yaml` does not publish. Imports record `import_rows_total{outcome}`, with `created`, `updated`, `unchanged`, `skipped` and `rejected`, and `import_duration_seconds`, which times every import including the ones rejected as a whole.
- **Traces:** OpenTelemetry over OTLP gRPC, disabled by default. Incoming `traceparent` headers are honoured, and spans are named after the matched route. Under the request span, an import adds `catalog.import` for the whole operation and `catalog.import.apply` for validating and applying the rows, and every statement sent to PostgreSQL adds a `postgres <OPERATION>` span, such as `postgres SELECT` or `postgres COMMIT`. That separates the time spent reading the file, processing the rows and waiting for the database. Spans carry the operation and a row count; never the SQL text, its arguments, a file name or a product value. A file rejected as a whole is an expected rejection: the caller still gets the error, but the span is not marked as failed.

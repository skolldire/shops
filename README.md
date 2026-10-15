# shops

E-commerce application built as a modular monolith in Go.

It currently ships the platform layer (`internal/platform`), the product catalog with its JSON API, CSV import and inventory audit (`internal/catalog`), the cart, checkout and purchases (`internal/ordering`), a simulated payment (`internal/payment`) and a server-rendered UI to browse, buy and manage products (`internal/web`).

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

There are no data migrations: `db/init/001_schema.sql` only runs when the data volume is created. Every schema change requires recreating the stack, which deletes the demo data:

```sh
make down && make up
```

Phase 2 added the `version` column to `products` and moved to PostgreSQL 18. Phase 4 adds `inventory_movements` with its triggers, the `payment_token` column of `purchases`, the `reserved` units of `products` and the reserved columns of `inventory_movements`. At startup the app checks that the columns it needs exist and stops with `database: the schema is out of date, recreate the volume with make down && make up` when one is missing. The message does not name the column. The app never runs DDL.

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
| Integration tests | Repositories, transactions, the inventory audit and HTTP acceptance tests of the product lifecycle, the checkout (concurrent buyers, idempotency, rejections, declined payments, recovery) and the UI purchase flow against PostgreSQL |
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
| `httpapi`, `httpquery`, `csvimport` | Input adapters: JSON, query strings and CSV files are decoded here and handed over as plain values | `app`, `domain` and `platform`; `httpapi` also imports `httpquery` to read the search parameters and `csvimport` to translate its file errors into a status and a code |
| `postgres` | Output adapter: implements `Repository` and `ProductUpserter` for PostgreSQL | `app`, `domain` and `platform` |
| `observe` | What the module tells operators about an import. `ImportRecorder` and `FailureReporter` implement the ports `ImportRecorder` and `ImportFailureReporter`; `InstrumentedImporter` decorates the import with its duration and its span | `app` and `platform` |
| `module.go` | Composition: builds the adapters, wires them into the use cases and exposes the facade | Everything in the module |

`domain` and `app` do not read files, parse formats, log, measure time or call a telemetry SDK:

- The CSV adapter reads the upload, enforces the size and row limits, hashes the file, maps the columns and reads the numbers. The import use case receives typed product values, together with the fields the adapter could not read, and never sees a CSV cell. The errors that reject a whole file (another header, malformed CSV, too many rows, too many bytes) belong to the CSV adapter; the HTTP handler turns them into a status and a code, and the facade into public errors.
- When the import recovers from a failed batch, or the database refuses a row, the use case reports it through `ImportFailureReporter` and `observe.FailureReporter` writes one log line: a warning for the recovery, an error for the row that is finally rejected.
- `observe.InstrumentedImporter` wraps the whole import, reading included. It opens the `catalog.import` span, and records `import_duration_seconds` after the span closes, also for files rejected before any row is applied and for cancelled requests. `domain` and `app` do not import OpenTelemetry.
- The facade takes search parameters as plain text and the seed as a reader; query strings stay in the HTTP adapters and the seed file is opened by `cmd/api`.

The import is composed in `module.go`, from the outside in:

```
HTTP handler or facade
  -> observe.InstrumentedImporter   duration and the catalog.import span
       -> csvimport.Importer        reads, limits, hashes, maps columns, reads numbers
            -> app.Importer         validates every row, then applies the valid ones in batches
                 -> postgres        one transaction per batch, one span per statement
```

The ordering module follows the same layout:

| Package | Role | May import |
|---|---|---|
| `domain` | Purchases and their states, the checkout rules, the canonical request hash, cart limits and `ParseID` | A closed list: `crypto/sha256`, `encoding/hex`, `errors`, `regexp`, `slices`, `strconv`, `strings`, `time`, `shopspring/decimal` and `platform/validation` |
| `app` | Checkout, finalization, recovery, cart and purchase queries, and the ports `Tx`, `Stock`, `Payments`, `Purchases`, `Carts` and `CheckoutRecorder` | A closed list: `context`, `errors`, `fmt`, `slices`, `time`, `shopspring/decimal`, `domain` and `platform/validation` |
| `httpapi` | The checkout and purchase endpoints. Reads `X-Demo-User-ID` and `Idempotency-Key`, decodes JSON and parses prices with `catalog.ParseDecimal` | `app`, `domain`, `platform` and the catalog facade for number parsing |
| `postgres` | `Tx`, `Purchases` and `Carts` on `purchases`, `purchase_items` and `cart_items` | `app`, `domain` and `platform` |
| `catalogstock` | Implements `Stock` with the catalog facade (`ReserveStock`, `ReleaseStock`, `CommitStock`, `Get`) | `app`, `domain` and the catalog facade |
| `paymentgw` | Implements `Payments` with the payment facade and translates its errors | `app` and the payment facade |
| `observe` | Implements `CheckoutRecorder`: the checkout metrics and the one log line per checkout | `domain` and `platform` |

`payment` has no `internal/` packages because its only piece is the simulated charge; a real provider adapter would go to `payment/internal` behind the same facade. Each module owns its tables: ordering never reads `products`, it goes through the catalog facade, and catalog and payment do not know ordering.

`make lint-arch` enforces this on every pull request. It fails if `domain` or `app` of catalog or ordering import anything outside their closed lists (so `domain` cannot import `app`, and neither can import an adapter), if they reach transport, persistence, logging or telemetry packages even indirectly, if ordering's `domain` or `app` reach `internal/catalog` or `internal/payment`, if catalog or payment depend on ordering, if payment depends on catalog, or if `internal/platform` imports a business module. `make lint-decimals` only allows `decimal.NewFromString` where PostgreSQL text is read back (`catalog/internal/postgres`, `ordering/internal/postgres`) and in the decimal grammar itself; everything else parses with `catalog.ParseDecimal`.

## Endpoints

| Port | Method and path | Response |
|---|---|---|
| 8080 | `GET /health/live` | `200 {"status":"up"}` while the process responds |
| 8080 | `GET /health/ready` | `200` or `503` with `{"status":"up\|down","checks":{"postgres":"up\|down"}}` |
| 8080 | `/api/v1/...` | The product API and the checkout and purchase API, described below. Unknown routes answer `404` or `405` as `application/problem+json` (RFC 9457), with `Allow` on 405 |
| 8080 | `/health/...` | Unknown health routes answer `404` as `application/problem+json` |
| 8080 | anything else | The web UI. Unknown pages answer an HTML `404` |
| 9100 | `GET /metrics` | Prometheus metrics: HTTP server, Go runtime, process, imports and checkouts |

## Product API

| Method and path | Success | Notes |
|---|---|---|
| `GET /api/v1/products` | `200` with a page | Search parameters below |
| `POST /api/v1/products` | `201` with the product, `Location` and `ETag` | |
| `GET /api/v1/products/{id}` | `200` with the product and `ETag: "<version>"` | |
| `PATCH /api/v1/products/{id}` | `200` with the product and the new `ETag` | Requires `If-Match`. Only the fields sent are changed |
| `DELETE /api/v1/products/{id}` | `204` | Requires `If-Match`. Deletion is logical |
| `GET /api/v1/products/{id}/movements` | `200` with a page of stock movements, newest first | `page` and `page_size` as in the search. Also lists the movements of a deleted product; `404` only if the product never existed |
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
  "reserved": 2,
  "available": 8,
  "weight_kg": "0.850",
  "version": 3,
  "created_at": "2026-10-09T17:00:00Z",
  "updated_at": "2026-10-09T17:05:00Z"
}
```

`stock` is the physical units in the warehouse. `reserved` is the units held by `PENDING` purchases, and `available = stock - reserved` is what can still be bought. Before phase 4 `stock` already had the reservations subtracted; now it does not (see [Physical and reserved stock](#d1-physical-and-reserved-stock)). Only `stock` can be written; `reserved` changes only through checkouts.

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
| `in_stock` | `true` returns only products with available units (`stock - reserved > 0`) |
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

A sale changes the version too: an approved payment lowers the physical stock, which is part of what the `ETag` protects, so an administrator who edits a product with an `ETag` read before the sale gets `412` instead of overwriting the stock the sale lowered. A reservation and its release do not change the version: they only move `reserved`, which nobody edits, so a cart being checked out does not invalidate a form that is open. The `ETag` therefore identifies the editable state of the product, not every byte of the representation: two responses with the same `ETag` can show different `reserved` and `available` values.

An edit cannot leave the stock below the reserved units. The condition is part of the same `UPDATE` (`… AND $stock >= reserved`), so there is no window between a check and the write. The answer is `409 below_reserved` on the field `stock`, with the reserved units in the message, and nothing of the edit is applied.

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
| `stock` below the units reserved by pending purchases, on the field `stock` | 409 | `below_reserved` |
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
| SKU of an active product | Updated. `stock` replaces the physical units; it is not added to them, and the reserved units stay as they are |
| SKU of an active product, `stock` below its reserved units | Rejected with `field: "stock"` and `code: "below_reserved"`; nothing of the row is applied |
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
| `below_reserved` | `stock` | The stock in the file is below the units reserved by pending purchases |
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

## Inventory audit

Every change of the physical stock or the reserved units is recorded in `inventory_movements`, written in the same transaction as the change. Both can be rebuilt from the movements:

```
stock    = COALESCE(SUM(delta), 0)
reserved = COALESCE(SUM(reserved_delta), 0)
```

Every movement also stores `stock_after` and `reserved_after`, and a `CHECK` requires that it changes at least one of the two.

Creating a product with a positive stock records one movement with `delta` equal to that stock; there is no other starting balance. A product created with stock 0 has no movement (`delta <> 0`), and the formula still holds. An update records `delta = new stock - old stock`, and no movement when the stock does not change, so reimporting `unchanged` rows records nothing. The product and its movement commit or roll back together.

**Captured by a trigger, not by hand.** `AFTER INSERT OR UPDATE OF stock, reserved ON products` inserts the movement whenever either changes. The guarantee does not depend on every code path remembering to write it: even a manual `UPDATE` from `psql` is recorded. Each writer sets `shop.stock_reason` and `shop.stock_reference` with `set_config(..., true)` inside its transaction, always both of them, with an empty reference when there is none, so a writer never inherits the reason or reference of an earlier statement in the same transaction. The create and edit paths now run in a transaction for that reason.

| Path | `reason` | `reference` |
|---|---|---|
| Path | `reason` | `delta` | `reserved_delta` | `reference` |
|---|---|---|---|---|
| Create with stock above 0 (API or UI) | `initial_stock` | +n | 0 | none |
| Edit that changes the stock (API or UI) | `manual_adjustment` | ±n | 0 | none |
| Import and demo seed, only rows created or updated | `import_replace` | ±n | 0 | SHA-256 of the file |
| Checkout, when the units are reserved | `reservation` | 0 | +q | purchase id |
| Approved payment | `sale` | −q | −q | purchase id |
| Declined payment | `reservation_release` | 0 | −q | purchase id |
| Direct SQL | `unattributed` | any | any | none |

Meaning changes in phase 4: `sale` used to be written when the checkout took the stock; it is now written when the payment is approved, and it lowers the physical stock and the reserved units together. `sale_reversal` is no longer written: a declined payment releases a reservation instead (`reservation_release`) and never touches the physical stock. The reason `import` became `import_replace`, ahead of the `import_add` mode. There are no migrations, so a fresh volume never contains the old reasons.

A product created by CSV gets one `import_replace` movement, not an `initial_stock` plus an `import_replace`. The file hash identifies the content, not one run: two imports of the same file share the reference. `db_user` is the database role of the connection (`shop` for the application), not the demo user. `tx_id` is the PostgreSQL transaction id.

**Immutable in the database.** A `BEFORE UPDATE OR DELETE` trigger on `inventory_movements` and a statement-level `BEFORE TRUNCATE` trigger raise an error, so the application cannot change or remove history. The limit: the owner of the table or a superuser can disable triggers, and the application connects as the owner. In production the table would belong to a role other than the application role, with `REVOKE UPDATE, DELETE` (see [Evolution](#evolution)).

`GET /api/v1/products/{id}/movements` lists them newest first (`id` descending), and `/admin/products/{id}/movements` shows the same history from the product table.

## Purchases

### Demo users

There is no authentication. The identity is a demo user taken from `demo.users` (`alice`, `bob` and `carol` by default). An empty list, an empty name or a repeated name stops the startup.

- **API:** `X-Demo-User-ID` is required on `/api/v1/checkout` and `/api/v1/purchases`. A missing or unknown user answers `400 invalid_demo_user`; the error does not list the valid users.
- **UI:** the selector in the header stores the user in the `demo_user` cookie (`HttpOnly`, `SameSite=Lax`, `Path=/`). Without a cookie, or with an unknown value, the UI uses the first user of the list.

This is not authentication: any client can pick any demo user. The use cases receive the user as a plain value and never read headers or cookies.

**CodeRabbit exception.** The repository rule that no configuration value reaches a response has one deliberate exception: the UI header renders `demo.users` so the evaluator can switch users. Nothing else exposes them, and secrets and internal settings stay private. `.coderabbit.yaml` states the exception.

### Same-origin check for forms

Because the UI falls back to the first demo user without a cookie, `SameSite=Lax` alone is not enough: another site could send a form that runs as that user. Every UI request other than `GET`, `HEAD` and `OPTIONS`, the admin forms included, must come from the same origin:

- If `Origin` is present it must match the scheme and the authority (host and port, default ports included) of the request. The values are parsed as URLs, never compared as prefixes. `Origin: null` and unparsable values are rejected.
- Without `Origin`, `Referer` is checked with the same rule.
- With neither header the request is rejected. Current browsers send `Origin` on form posts.
- `X-Forwarded-*` and other proxy headers are ignored.

A rejection answers `403` with the HTML error page. The compose stack serves the app directly; behind a reverse proxy the public host would have to be configured. The JSON API does not use cookies, so it does not have this check.

### Checkout

```sh
curl -i localhost:8080/api/v1/checkout \
  -H 'X-Demo-User-ID: alice' \
  -H "Idempotency-Key: $(uuidgen)" \
  -H 'Content-Type: application/json' \
  -d '{"items":[{"product_id":"<id>","quantity":2,"expected_unit_price":"29.99"}],"payment_token":"pm_approved"}'
```

Totals sent by the client are ignored and the currency is always `USD`. The fake payment accepts `pm_approved` and declines `pm_declined` with `card_declined`; any other token is declined with `unknown_payment_token`.

| Rule | |
|---|---|
| `Idempotency-Key` | A UUID in its canonical form with hyphens. Upper case is normalized; `urn:`, braces and the form without hyphens are rejected |
| `items` | 1 to 50, each `product_id` a canonical UUID, not repeated once normalized |
| `quantity` | 1 to 99 |
| `expected_unit_price` | A plain decimal, greater than 0, at most 2 decimals |
| `payment_token` | Required, at most 64 characters |
| Total | Computed by the server with exact decimals, at most `9999999999.99` |

All UUIDs (product ids, the key and the purchase id of the queries) go through one validator in the ordering domain, so the API, the UI and the facade accept the same values.

**Request hash.** The key is bound to a SHA-256 of a canonical, length-prefixed representation: the items sorted by normalized product id, each with its quantity and its price at 2 decimals, plus the token and the currency. Two bodies with the same meaning (another order of items, `29.9` instead of `29.90`, upper-case ids) have the same hash.

### States and flow

A purchase is `PENDING`, `CONFIRMED` or `FAILED`, and only `PENDING → CONFIRMED` and `PENDING → FAILED` exist. `PENDING` means its units are reserved and there is no definitive payment result yet; the UI never shows it as confirmed.

1. **Reservation (one transaction).** The purchase is inserted as `PENDING` with `ON CONFLICT (user_id, idempotency_key) DO NOTHING`, already with its server-side total. If the key exists, the transaction is rolled back without touching stock and the stored purchase answers. Otherwise catalog reserves the units of every item, in product id order, in the same transaction: `reserved = reserved + q` only if the product is active and `stock - reserved >= q`, in one statement. Ordering then compares each current price with `expected_unit_price`. Any shortage, unavailable product or price change rolls back the purchase, the reservations and the movements together. The items are stored with a snapshot of SKU, name, unit price, quantity and line total.
2. **Charge.** Outside any transaction and without product locks, with the purchase id as the reference.
3. **Finalization (one transaction, shared with the recovery).** `UPDATE purchases SET status = ... WHERE id = $1 AND status = 'PENDING'`. If no row changes, another finalization already won and nothing else is applied. Otherwise, in the same transaction: approved → `CONFIRMED` with the payment reference, the sale (`stock - q` and `reserved - q`, through `catalog.CommitStock`) and the cart cleanup; declined → `FAILED` with the reason and the reservation released (`reserved - q`). Both act on deleted products too, so a product deleted while a purchase is pending still settles it. If the reserved units are fewer than the purchase needs, catalog answers `ErrStockInvariant`: the finalization rolls back, the purchase stays `PENDING` and the error is logged. It is never corrected silently.

Only a definitive decline leads to `FAILED`. A technical error, an unknown outcome, an unavailable provider or a failed finalization after the charge keep the purchase `PENDING` with its units reserved; the [recovery](#recovery-at-startup) settles it. There is no atomicity between PostgreSQL and the payment; the recovery covers the interruption windows.

**Lock order.** Every path acquires locks in this order and none inverts it:

| Path | Order |
|---|---|
| Add to cart | Advisory lock of the user's cart → cart row |
| Change quantity, remove, accept price | Cart row |
| Reservation | Purchase row → product rows by product id |
| Finalization, confirmed | Purchase row → product rows by product id → cart rows by product id |
| Finalization, declined | Purchase row → product rows by product id |
| Import | Product rows by SKU |
| Product edit | One product row |

The confirmed finalization now locks the products too, because the sale lowers their stock. The cart never takes a lock and then one of a purchase or a product, so it forms no cycle with the checkout. This does not remove every deadlock: the import locks by SKU and the checkout by product id, so an import running at the same time as a checkout or a finalization can end in `deadlock_detected`, and PostgreSQL aborts one of them. There are no SQL retries. A reservation that fails with `40P01` or `55P03`, with its rollback confirmed, answers `503 try_again` with `Retry-After: 1`, and the client can retry with the same key. A finalization that fails that way leaves the purchase `PENDING` and answers `202`.

### Idempotency

Keys are scoped by user.

| Situation | Effect | Answer |
|---|---|---|
| New key, approved | Created, units reserved and sold once | `201`, `Location` and the `CONFIRMED` purchase |
| New key, declined | Created, units reserved and released once | `201`, `Location` and the `FAILED` purchase with its reason |
| New key, technical error, unknown outcome or failed finalization after the reservation | The purchase and its reservation are kept | `202`, `Location` and the `PENDING` purchase |
| Same key and hash, finished purchase | Reads the stored result, no charge and no effects | `200` with the same body as the first answer |
| Same key and hash, `PENDING` purchase | No new charge from this request | `202` with the `PENDING` purchase |
| Same key, other hash | The purchase is not changed | `409 idempotency_conflict` |
| Recoverable lock error in the reservation, rolled back | No purchase and no units reserved | `503 try_again`, `Retry-After: 1` |

Concurrent requests with the same key wait on the unique index until the first reservation finishes, and then land in one of these rows. If a commit fails in a way that leaves unknown whether it was applied (for example the connection dropped), the answer is `500 internal_error`; if the purchase was persisted, a retry with the same key finds it and the recovery settles it.

### Errors

All errors are `application/problem+json` with a stable `code`. Item details use the `errors` list, with fields such as `items[0].quantity`.

| Case | Status | `code` |
|---|---|---|
| Missing or unknown `X-Demo-User-ID` | 400 | `invalid_demo_user` |
| Missing `Idempotency-Key` or not a UUID | 400 | `invalid_idempotency_key` |
| Malformed JSON | 400 | `invalid_request` |
| Body larger than 1 MB | 413 | `request_too_large` |
| Field rules, with an `errors` list | 422 | `validation_failed` |
| Unknown or deleted product, on `items[i].product_id` | 422 | `product_unavailable` |
| Not enough available units (`stock - reserved`), on `items[i].quantity` with the available units in the message | 409 | `insufficient_stock` |
| Changed price, on `items[i].expected_unit_price` with the current price in the message | 409 | `price_changed` |
| Same key with another body | 409 | `idempotency_conflict` |
| Deadlock or lock not available in the reservation | 503 | `try_again` |
| Unknown purchase, or a purchase of another user | 404 | `not_found` |
| Unexpected technical failure | 500 | `internal_error`, without tokens, SQL, configuration or provider details |

### Purchase representation and queries

```json
{"id":"…","status":"CONFIRMED","currency":"USD","total":"59.98","payment_ref":"fake_…","failure_reason":null,
 "created_at":"…","items":[{"product_id":"…","sku":"RS-001","name":"Running Shoes","unit_price":"29.99","quantity":2,"line_total":"59.98"}]}
```

| Method and path | Answer |
|---|---|
| `POST /api/v1/checkout` | See above |
| `GET /api/v1/purchases` | The purchases of the user, by `created_at` and `id` descending, with `page` and `page_size` (20 by default, 100 at most) and the page format of the products |
| `GET /api/v1/purchases/{id}` | One purchase of the user; another user's purchase is `404`, like one that does not exist |

The payment token is stored in `purchases.payment_token` because the recovery repeats the charge with it. It is a demo token, not card data: it is never logged and never returned by the API. This does not decide how a real provider's credentials would be stored.

### Monetary invariants

A purchase keeps its own copy of what was sold: `purchase_items` holds the SKU, name, unit price, quantity and line total, and `purchases.total` the amount. The history, the purchase detail (API and UI), the recovery and the charge read only that copy, never `products`, so no later import, edit or deletion changes what a purchase shows or charges. A `PENDING` purchase recovered after a price change charges the stored amount. The cart and the checkout do not change: a changed price is a warning in the cart and `409 price_changed` at checkout, so the client never pays a price it did not see.

| Invariant | Where it is guaranteed | Proved by |
|---|---|---|
| `line_total = unit_price × quantity`, exact to the cent | `domain.Snapshot` with `decimal`; prices are validated to 2 decimals and no `float` is used on any money path; `numeric(12,2)` in the schema | `TestSnapshotLineTotalsAreExactToTheCent` |
| `purchases.total = SUM(purchase_items.line_total)` | The total is the sum of the expected prices, and the reservation rejects the purchase unless every expected price equals the current one, in the same transaction that writes the items | Aggregation query in `requireMoneyInvariants` (integration) |
| No half-persisted purchase | Purchase, stock, items and movements are written in one transaction; a price change, a short stock, a deleted product or a database error rolls all of it back | `TestAFailedReservationLeavesNoPurchaseStockChangeOrMovement` |
| A repetition or a recovery charges the same reference, token, amount and currency | Every charge reads them from the persisted purchase; a repeated key replays the stored purchase without charging | `TestEveryChargeAttemptMatchesThePersistedPurchase`, with a double that records every attempt |
| The price does not change after buying | The copy above, plus the triggers below | `TestAConfirmedPurchaseKeepsItsPriceAfterTheCatalogChanges`, `TestARecoveredPurchaseChargesTheStoredAmountAfterAPriceChange` |
| The copy cannot be rewritten | `purchase_items_immutable` and `purchase_items_no_truncate` reject `UPDATE`, `DELETE` and `TRUNCATE` of items; `purchases_snapshot_immutable` rejects an `UPDATE` that changes `id`, `user_id`, `idempotency_key`, `request_hash`, `currency`, `total`, `payment_token` or `created_at` (SQLSTATE `42501`). The outcome (`status`, `payment_ref`, `failure_reason`, `updated_at`) can still change | `TestPurchaseSnapshotsAreImmutable` |

`total = SUM(line_total)` is checked by the tests, not by a trigger: the items are inserted after the purchase, so a row-level check would need a deferred constraint trigger for a rule the transaction already guarantees.

What stays outside these guarantees: the owner of the tables or a superuser can disable the triggers (`ALTER TABLE … DISABLE TRIGGER`), and a session with `session_replication_role = replica` skips them, which is what the test helper `dbtest.Truncate` does. The application connects as the owner. A direct `INSERT` can still write a purchase whose total does not match its items, and a direct `UPDATE` of `status` is not checked against the allowed transitions. In production the tables would belong to another role, as described for the movements in [Evolution](#evolution).

### Payment contract (provisional)

`payment.Charge(ctx, ChargeRequest{Reference, Token, Amount, Currency})` returns `APPROVED` with a provider reference, or `DECLINED` with a reason. Errors never mean a decline. Two classifiable errors let ordering decide without knowing the provider: `ErrOutcomeUnknown` (the request may have reached the provider and it is unknown whether it charged) and `ErrUnavailable` (the provider is not available). After the reservation both keep the purchase `PENDING`.

Any provider behind this contract must be idempotent by reference: the same reference with the same content does not charge twice and returns the original result; two concurrent charges with the same reference produce one charge; the same reference with other content is a conflict without a new charge. The recovery relies on it. The contract is provisional until a provider is chosen. The in-process fake keeps no state and never returns an error, so it does not prove this idempotency by itself; the tests prove the ordering side with a stateful double that tells requests from effective charges and returns unknown outcomes.

### Time budgets

| Setting | Default | Covers |
|---|---|---|
| `checkout.reserve_timeout` | 3s | The reservation transaction. Derived from the request context |
| `checkout.charge_timeout` | 6s | The charge. Starts when the reservation commits, from the request context without its cancellation, so a client that disconnects does not leave the purchase half done |
| `checkout.finalize_timeout` | 3s | The finalization. A fresh context, also without the client's cancellation; it never reuses the charge context, which may have expired |
| `checkout.recovery_timeout` | 60s | The whole startup recovery. Each purchase uses the charge and finalize budgets inside it, so exhausting the total also stops the purchase in flight |

All of them must be positive, and `reserve + charge + finalize + 2s` must not exceed `http.write_timeout` nor `http.shutdown_timeout`, or the app does not start. A graceful shutdown waits for the requests in flight; one that does not finish in time stays `PENDING` and the next startup settles it, if the database and the payment provider are available. These budgets do not guarantee that the client receives an answer. What they guarantee is correctness: if the client gets no answer, the idempotency key and the recovery bring the purchase to its right state.

### Cart

The cart lives on the server and is used from the UI only; there are no cart endpoints in the JSON API.

- Adding a product stores its current catalog price as the price seen. Adding it again adds the quantities, up to 99. A deleted or unknown product is an error the UI shows.
- At most 50 lines, because a checkout accepts at most 50 items. Adding a line takes `pg_advisory_xact_lock` on the user's cart and then counts, so two concurrent adds cannot both take the 50th slot. A hash collision of that advisory lock can make two different users wait for each other; it does not affect correctness.
- The cart page checks every line against the catalog and warns about unavailable products, available units below the quantity and prices that changed, with a "Use current price" button.
- The checkout from the cart builds the same items, with the price seen as the expected price, and calls the same use case as the API.
- A confirmed purchase removes a cart line only if its quantity and price seen are still the ones that were bought, so a line changed in another tab during the payment stays as it was left. If the user removes a line and adds it back identical during the payment, that line is removed. A declined payment does not touch the cart.

### Recovery at startup

Before it starts serving, after the demo seed, the app takes every `PENDING` purchase, repeats the charge with the same reference, token, amount and currency, and applies the same finalization as the checkout. The contract guarantees that this does not charge twice. A purchase whose outcome is still unknown, or whose provider is unavailable, stays `PENDING`. The summary is logged at `info` (`pending purchases recovered`), every purchase that is still pending is logged at `warn` with its cause, and those purchases do not stop the startup. A database error reading the pending purchases does, like the seed.

Decision and assumptions: the RFC proposed taking only purchases pending for more than a minute. The recovery runs before the server accepts requests and there is one instance, so there is no purchase in flight to protect. With several replicas this becomes a periodic job with a minimum age and a lock. A purchase interrupted while the app keeps running stays `PENDING` until the next start, because there is no periodic recovery yet. With the in-process fake the provider is never unavailable, so no readiness policy exists for that case; it will be defined with a real provider.

## Web UI

| Page | What it does |
|---|---|
| `/` | Store with search, category, price range, in-stock filter, sorting and pagination. Results update with htmx without reloading, and the filters stay in the URL so searches can be shared and the Back button works |
| `/products/{id}` | Product details, with "Add to cart" and a quantity when the product has stock |
| `/cart` | The cart: lines, warnings (unavailable, not enough stock, changed price with "Use current price"), quantity changes, removal and the estimated total |
| `/checkout` | The order and a test payment method (`pm_approved` or `pm_declined`). The page carries a fresh idempotency key in a hidden field, so a double click or a resubmission of the same attempt reuses it; disabling the button while the request runs is only a visual aid. If the cart changed and the checkout answers `idempotency_conflict`, the page is generated again with a new key. A price change goes back to the cart with its warnings |
| `/purchases` | The purchase history of the current demo user |
| `/purchases/{id}` | One purchase: confirmed, declined with its reason and the cart kept, or pending with a link to the history |
| `/admin/products` | Product table with search, edit and delete actions |
| `/admin/products/new` | Create a product. Validation errors appear next to each field |
| `/admin/products/{id}/edit` | Edit a product. The form sends the version it showed; if the product changed meanwhile, the page explains it and offers to reload |
| `/admin/products/{id}/delete` | Server-rendered confirmation before deleting |
| `/admin/products/{id}/movements` | The stock movements of a product, newest first |
| `/admin/imports` | Upload a CSV file. The page shows the counters of the import and a table with the rejected rows: line, SKU, field, code and message. A file that is rejected as a whole is explained on the same page |

The UI uses `html/template` and htmx 2.0.11, both embedded in the binary, so it works without internet access. Pages are served with a strict Content-Security-Policy (no inline scripts or styles), htmx is configured so it does not inject inline styles, and there is no custom JavaScript. The header shows the demo user selector and a link to the cart with its number of lines; if the cart cannot be read, the page is rendered without the counter and the failure is logged once. Error pages always use that reduced header.

## Design decisions

Each decision states its context, the alternatives that were considered with their consequences, what was chosen, an example and when it should be revisited.

### D1. Physical and reserved stock

**Context.** `stock` used to be one absolute value: the import set it, the reservation subtracted from it and a declined payment added back. With 1 unit reserved, an import that set 10 and a declined payment ended with 11 units that never existed.

**Alternatives.**

| Option | Consequence |
|---|---|
| Keep one value and document that an import must account for pending purchases | No schema change, but the person who writes the file has to know what is pending at the moment it is applied, which nobody can know. The 11-unit result stays possible |
| Physical stock plus reserved units | One more column, three operations instead of two, and the confirmed finalization also locks products. The import and the edit describe the warehouse; checkouts only move `reserved` until the payment is decided |

**Decision.** `products.stock` is the physical units and `products.reserved` the units held by `PENDING` purchases, with `CHECK (reserved >= 0 AND reserved <= stock)`. Available is `stock - reserved`. The catalog facade offers `ReserveStock` (`reserved + q` if active and available), `ReleaseStock` (`reserved - q`) and `CommitStock` (`stock - q` and `reserved - q`), all in the caller's transaction and in product id order. A release or commit that finds fewer reserved units than it needs is `ErrStockInvariant`, never a silent fix. A deleted product still settles its reservations.

**Example.** Stock 5, a pending purchase of 1 (stock 5, reserved 1, available 4). An import sets 10 (stock 10, reserved 1). Approved: stock 9, reserved 0. Declined: stock 10, reserved 0. Never 11.

**Revisit when** reservations need to expire on their own (a cart hold with a timeout), or stock is split by warehouse.

### D3. Never less physical stock than reserved

**Context.** With D1, an import or an edit could set fewer physical units than are already reserved, and the later sale would break the `CHECK` or be impossible.

**Alternatives.** Clamp the value to the reserved units (silently changes what the administrator wrote); reject in a query before the write (a reservation can land between the check and the write); reject in the write itself.

**Decision.** The import in replace mode, `PATCH /api/v1/products/{id}` and the admin form reject a stock below `reserved` with `below_reserved`, on the field `stock`. The condition is part of the same `UPDATE` (`… AND $stock >= reserved`) or of the `ON CONFLICT DO UPDATE … WHERE EXCLUDED.stock >= products.reserved` of the import, so there is no race. When the import skips a row for that reason, PostgreSQL still locks it until the import commits, so the report label is exact.

**Example.** Stock 5 with 3 reserved: an edit to 2 answers `409 below_reserved` with "must be at least 3, the units reserved by pending purchases", and neither the stock nor the name or price sent with it change. An edit to 3 is accepted.

**Revisit when** an administrator needs to write off units that are physically gone while purchases are pending; that would need a way to cancel those purchases first.

### D4. What changes the product version

**Context.** The `version` protects administrative edits from overwriting each other and from overwriting sales. With D1, checkouts write `reserved` far more often than anybody edits a product.

**Alternatives.** Bump the version on every write to the row (every cart that reaches checkout invalidates an open form, for no benefit: the form does not edit `reserved`); bump it only for what the form edits.

**Decision.** `version` increases when the physical stock or an attribute changes, including an approved payment, because it lowers the physical stock. It does not increase when only `reserved` changes (reservation and release). The `ETag` therefore identifies the editable state, not every field of the representation.

**Example.** An administrator opens the form at version 1; a checkout reserves 2 units and is declined; the form saves with version 1. If the payment had been approved, the save would answer `412`, because the stock it shows is no longer true.

**Revisit when** clients start caching product representations with `If-None-Match`: `reserved` and `available` would then need their own validator or a weak `ETag`.

### D6. Schema changes without migrations

**Context.** The project is not in production, and phase 4 changes the schema several times.

**Alternatives.** Migration scripts (`002_…`) and a migration tool (more moving parts for data nobody needs to keep); edit `001_schema.sql` and recreate the volume.

**Decision.** `db/init/001_schema.sql` is edited and the stack is recreated with `make down && make up`. The app never runs DDL. At startup it checks that the columns it needs exist and stops with a message that tells to recreate the volume, without naming the column.

**Revisit when** there is data that must survive a deployment; from then on every change needs a migration.

### D9. A purchase keeps the price it was bought at

See [Monetary invariants](#monetary-invariants). Alternatives were reading prices from `products` when showing or charging a purchase (a later import or edit would change what was paid and what is charged again by the recovery), and a copy guarded by triggers. The copy is in `purchase_items` and `purchases.total`; triggers reject changes to it. Revisit when purchases can be amended (partial refunds, returns): that needs new rows that refer to the original, not changes to it.

## Known limitations

- **Search does not use an index.** `ILIKE '%q%'` scans the active products. This is fine for thousands of products; a larger catalog would need full-text search or trigram indexes.
- **The search total is not transactionally consistent with the items.** They come from two separate queries, so with concurrent writes the `total` can differ from the items of a page.
- **An import sets the physical stock to the value in the file.** It is absolute, not a delta, so the file must describe the warehouse at the time it is applied: a file written before a sale that was approved later still sets the units it lists. Reservations are kept apart, so an import no longer brings back or loses reserved units, and a row below the reserved units is rejected.
- **No authentication.** Anyone who can reach port 8080 can create, edit, delete and import products through `/api/v1` and `/admin`, and act as any demo user. Do not expose the published listener to untrusted clients without access control in front of it, such as an authenticating reverse proxy or a private network.
- **Forms are protected by a same-origin check, not by CSRF tokens.** Behind a reverse proxy that changes the host, the public host would have to be configured.
- **An import and a checkout can deadlock.** They lock products in different orders (SKU and product id). PostgreSQL aborts one of them; a reservation answers `503 try_again`, a finalization leaves the purchase `PENDING`.
- **Pending purchases wait for the next start.** There is no periodic recovery and the recovery assumes a single instance.
- **The payment is an in-process fake.** It never times out or fails, so the unknown and unavailable outcomes are exercised only by the tests.
- **Movements and purchase snapshots are immutable only for the application.** The table owner or a superuser can disable their triggers, and `session_replication_role = replica` skips them.
- **No data migrations, by design.** Pre-production data is disposable: it is recreated from an initial data load after `make down && make up`. Production skips that initial load.

## Evolution

Pieces left out of this phase, with what would justify them:

| Piece | Trigger |
|---|---|
| Real HTTP payment provider (phase 4b: simulator, HTTP adapter, retries, circuit breaker, remote lookup by reference) | Choosing a provider, or demonstrating a timeout without a double charge. It plugs in behind the payment facade without changing ordering |
| Periodic recovery with a minimum age and a lock | Running more than one replica, or pending purchases that cannot wait for a restart |
| `inventory_movements`, `purchases` and `purchase_items` owned by a separate role, with `REVOKE UPDATE, DELETE` (column-level `UPDATE` grants for the purchase outcome) for the application role | Production, where the audit must hold even against the application's own credentials |
| Real authentication and sessions instead of demo users | Anyone other than an evaluator using the store |
| Configurable public host for the same-origin check | Serving the app behind a reverse proxy |
| Outbox and message broker | Phase 5 |

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

Keys without an environment variable:

| Key | Default |
|---|---|
| `demo.users` | `[alice, bob, carol]`. The first one is used when the UI has no `demo_user` cookie |
| `import.max_bytes`, `import.max_rows` | `10485760` (10 MB) and `10000`. See [The file](#the-file) |
| `import.preview_ttl` | `1h`. An import preview older than this expires: the next upload cancels it and frees its content, and confirming it answers `410 preview_expired` |
| `import.max_active_previews` | `20`. Previews that keep their content at the same time, across every replica; one more answers `429 too_many_previews` |
| `checkout.reserve_timeout`, `checkout.charge_timeout`, `checkout.finalize_timeout`, `checkout.recovery_timeout` | `3s`, `6s`, `3s` and `60s`. See [Time budgets](#time-budgets) |

## Observability

- **Logs:** JSON on stdout with `timestamp`, `severity` and `message`. Every request line carries `request_id`, `trace_id` and `span_id`. Sensitive fields and values (passwords, tokens, bearer credentials, card numbers) are redacted. The configuration is never logged. An import that recovers from a failed batch logs it once as a warning; a row the database finally rejects is logged once as an error.
- **Metrics:** OpenTelemetry exported in Prometheus format on a separate port that `compose.yaml` does not publish (the image has no shell, so use `make up-debug`, which publishes it on `127.0.0.1:9100`). Imports record `import_rows_total{outcome}`, with `created`, `updated`, `unchanged`, `skipped` and `rejected`, and `import_duration_seconds`, which times every import including the ones rejected as a whole. Every checkout that reaches the use case counts once in `checkout_total{outcome}`: `confirmed`, `declined`, `pending` (a stored purchase without a definitive payment result), `insufficient_stock`, `price_changed`, `product_unavailable`, `invalid` (field rules and the idempotency key, which the use case validates), `idempotency_conflict`, `replayed`, `in_progress`, `try_again` (only `40P01` and `55P03`) and `error` (unexpected failures); a failure counts as `try_again` or as `error`, never both. Requests rejected before the use case (unknown demo user, unreadable JSON, a body too large) are not counted there; the HTTP server metrics cover them. The startup recovery counts `checkout_recovery_total{outcome}` with `confirmed`, `declined` and `still_pending`. No label carries users, purchases, tokens or error text.
- **Checkout logs:** every checkout that ends with a purchase writes one line with `purchase_id`, `user_id` and `status`, at `warn` with its cause when the purchase stays `PENDING`. The payment token is never logged. An unexpected failure is logged once, by the HTTP adapter.
- **Traces:** OpenTelemetry over OTLP gRPC, disabled by default. Incoming `traceparent` headers are honoured, and spans are named after the matched route. Under the request span, an import adds one `catalog.import` span for the whole operation, and every statement sent to PostgreSQL adds a `postgres <OPERATION>` child span, such as `postgres SELECT` or `postgres COMMIT`. The trace shows how long the import took, which statements ran, how long each one took and which one failed; the time not spent in PostgreSQL is reading, validating and preparing the rows. Spans carry the operation name only: never the SQL text, its arguments, a file name or a product value. The checkout adds no spans of its own; its PostgreSQL statements hang from the request span. A file rejected as a whole is an expected rejection: the caller still gets the error, but the span is not marked as failed.

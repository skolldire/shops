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

`make up-replicas` starts two replicas of the app behind a proxy (`caddy`) on `http://localhost:8081`, besides the first replica on `8080`. Both replicas share the database: they seed once, recover purchases without duplicating charges and answer the same idempotency keys. The proxy keeps the `Host` header, so the same-origin check needs no configuration.

### Upgrading an existing stack

There are no data migrations: `db/init/001_schema.sql` only runs when the data volume is created. Every schema change requires recreating the stack, which deletes the demo data:

```sh
make down && make up
```

Phase 2 added the `version` column to `products` and moved to PostgreSQL 18. Phase 4 adds `inventory_movements` with its triggers, the `payment_token` column of `purchases`, the `reserved` units of `products`, the reserved columns of `inventory_movements`, the `imports` table, the recovery columns of `purchases` (`recovery_attempts`, `next_attempt_at`, `lease_until`, `lease_owner`, `last_recovery_error`) and `payment_fake_charges`. At startup the app checks that the columns it needs exist and stops with `database: the schema is out of date, recreate the volume with make down && make up` when one is missing. The message does not name the column. The app never runs DDL.

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
| `app` | Checkout, finalization, recovery, cart and purchase queries, and small ports per consumer: `Tx`; `StockReservation`, `StockSettlement` and `ProductReader`; `PaymentCharger` and `Payments`; `PurchaseCreation`, `PurchaseLookup`, `PurchaseReader`, `PurchaseFinisher` and `PendingPurchases`; `CartEditor` and `PurchasedCartRemover`; `CheckoutRecorder` and `RecoveryRecorder`. Each use case receives only the ones it calls | A closed list: `context`, `errors`, `fmt`, `slices`, `time`, `shopspring/decimal`, `domain` and `platform/validation` |
| `httpapi` | The checkout and purchase endpoints. Reads `X-Demo-User-ID` and `Idempotency-Key`, decodes JSON and parses prices with `catalog.ParseDecimal` | `app`, `domain`, `platform` and the catalog facade for number parsing |
| `postgres` | `Tx`, the purchase ports and the cart ports on `purchases`, `purchase_items` and `cart_items` | `app`, `domain` and `platform` |
| `catalogstock` | Implements the stock and product ports with the catalog facade (`ReserveStock`, `ReleaseStock`, `CommitStock`, `Get`) | `app`, `domain` and the catalog facade |
| `paymentgw` | Implements `Payments` with the payment facade and translates its errors | `app` and the payment facade |
| `observe` | Implements `CheckoutRecorder` and `RecoveryRecorder`: the checkout and recovery metrics and logs | `domain` and `platform` |

`payment` has no `internal/` packages because its only piece is the simulated charge; a real provider adapter would go to `payment/internal` behind the same facade. Each module owns its tables: ordering never reads `products`, it goes through the catalog facade, and catalog and payment do not know ordering.

`make lint-arch` enforces this on every pull request. It fails if `domain` or `app` of catalog or ordering import anything outside their closed lists (so `domain` cannot import `app`, and neither can import an adapter), if they reach transport, persistence, logging or telemetry packages even indirectly, if ordering's `domain` or `app` reach `internal/catalog` or `internal/payment`, if catalog or payment depend on ordering, if payment depends on catalog, or if `internal/platform` imports a business module. `make lint-decimals` only allows `decimal.NewFromString` where PostgreSQL text is read back (`catalog/internal/postgres`, `ordering/internal/postgres`, `payment/internal/fakestore`) and in the decimal grammar itself; everything else parses with `catalog.ParseDecimal`.

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
| Creating a product while an import is being applied, with `Retry-After: 5` | 409 | `import_in_progress` |
| `If-Match` does not match the current version | 412 | `version_conflict` |
| `stock` below the units reserved by pending purchases, on the field `stock` | 409 | `below_reserved` |
| `If-Match` carries a weak entity tag such as `W/"3"` | 412 | `precondition_failed` |
| `PATCH` or `DELETE` without `If-Match` | 428 | `precondition_required` |

Field errors use stable codes: `required`, `too_long`, `not_positive`, `too_many_decimals`, `out_of_range`, `invalid_format` and `immutable`. The import adds `wrong_field_count`, `duplicate_in_file`, `sku_deleted` and `rejected_by_database`.

## CSV import

Products can be loaded from a CSV file through the API or from `/admin/imports`:

```sh
curl -F file=@testdata/products.csv localhost:8080/api/v1/imports
curl -F file=@delivery.csv -H "Idempotency-Key: $(uuidgen)" 'localhost:8080/api/v1/imports?mode=add'
```

The request is `multipart/form-data` with the file in a field named `file`. The import is applied in that same request and the response is the final report: there is no review or confirmation step (see [D2](#d2-import-modes-applied-in-the-request)). The file is read as a stream and its content is never stored, neither on disk nor in the database. What is stored, in the `imports` table, is the metadata of each import: its id, key, mode, file name, size and hash, status, counters, rejected rows and the before and after values of every existing product.

**Mode.** `?mode=add` adds the units in the file to the physical stock: there are 50 and the file says 10, so 60 remain. `?mode=replace`, the default when `mode` is missing, sets the physical stock to the units in the file: 10 remain. In both modes the price, name, description, category and weight are replaced by the file; the mode only decides what happens to the stock. Another value answers `400 invalid_mode`.

**Idempotency.** `Idempotency-Key` (a UUID) is required with `add` and optional with `replace`:

| Same key, and… | Answer |
|---|---|
| Same file and mode, import applied | The original report, without applying anything again |
| Same file and mode, import interrupted (`APPLYING`) | The whole file is applied again from the upload, under the imports lock, and it finishes `APPLIED`. The lock of the interrupted request is released when PostgreSQL ends its connection, so a retry sent at that very moment can still get `409 import_in_progress`; the retry after `Retry-After` resumes it |
| Another file or mode | `409 idempotency_conflict` |

**One import at a time.** Before applying anything, an import takes `pg_try_advisory_lock` on a single key shared by every import, on one pinned connection, and the whole application runs on that connection. If another import holds it, the request answers at once with `409 import_in_progress` and `Retry-After: 5`: it writes no `imports` row, no product and no movement, and the same request works when it is sent again. A key that already has an applied import is answered from its stored report before trying the lock, and a key used with another file or mode answers `409 idempotency_conflict` there too, so repeating a request never meets `import_in_progress` once its import is done. Two replicas that receive the same key at the same time apply it once: one applies it and the other answers `import_in_progress`, and its retry gets the stored report. Every status change is an `UPDATE … WHERE status = <expected>`. See [D10](#d10-one-import-at-a-time).

**Repeated file.** If a file with the same hash was already applied with `add`, a new `add` of it is not applied and answers `409 duplicate_file` with the date of the earlier import, unless the request sends `allow_duplicate=true`. Adding the same delivery twice is almost always a mistake, but sometimes it is a real second delivery. A `replace` with the same file is harmless and needs no confirmation.

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
| SKU of an active product, `replace` | Updated. `stock` becomes the physical units and the reserved units stay as they are |
| SKU of an active product, `add` | Updated. `stock` is added to the physical units |
| SKU of an active product, `replace`, `stock` below its reserved units | Rejected with `field: "stock"` and `code: "below_reserved"`; nothing of the row is applied |
| New SKU | Created |
| Active product that is not in the file | Left as it is |

The file is validated completely before anything is written. Then:

- **`add` is all or nothing.** One transaction creates the `imports` row, applies every valid row and marks the import `APPLIED`. If anything fails, nothing is left: no product change, no movement and no `imports` row. The cost is that an `add` locks every product of the file until the whole file is applied, so a large `add` makes checkouts of those products wait for it. Retrying an `add` that was half applied would add the units twice, which is why it cannot be applied in parts.
- **`replace` is applied in batches of 500.** The `imports` row is committed as `APPLYING` in its own transaction before the first batch, each batch commits on its own, and the import becomes `APPLIED` after the last one. A request cancelled halfway leaves the batches already committed and the import `APPLYING`, which marks it as interrupted. Uploading the same file again with the same key applies all of it again and finishes it; this is safe because replacing with the same values changes nothing twice. A row that is identical to the stored product is left untouched: its `version` and `updated_at` do not change. So importing the same file again reports as `unchanged` the rows that the first import created or updated, as long as nobody changed those products in between; rows that were skipped or rejected are skipped or rejected again.

An import overrides whatever is stored: it does not use `If-Match`, and it increases `version` when it changes a product. An administrator who has that product open gets the usual conflict when saving.

### The report

`POST /api/v1/imports` answers `200` with the report, also when rows were rejected:

```json
{
  "import_id": "5a0b2c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b",
  "mode": "replace",
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
  ],
  "changes": [
    {"sku": "RS-001", "stock_before": 50, "stock_after": 10, "price_before": "29.99", "price_after": "34.99"}
  ]
}
```

`changes` lists every existing product of the file that was applied, changed or not, with its stock and price before and after, in SKU order. They come from the same statement that applies the row (`RETURNING old.…, new.…` in PostgreSQL 18), so they are what happened, not an estimate. New products are counted in `created` and not listed.

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
| `mode` other than `add` or `replace` | 400 | `invalid_mode` |
| `Idempotency-Key` that is not a UUID, or missing with `add` | 400 | `invalid_idempotency_key` |
| `allow_duplicate` other than `true` or `false` | 400 | `invalid_request` |
| Same key with another file or mode | 409 | `idempotency_conflict` |
| `add` of a file already added, without `allow_duplicate=true`; the date is in `detail` | 409 | `duplicate_file` |
| Another import is being applied, with `Retry-After: 5` | 409 | `import_in_progress` |

None of these creates an `imports` row: a file rejected as a whole has no effects to trace.

### Demo data

`testdata/products.csv` is the official file of the challenge, unmodified. It was downloaded on October 7, 2026 and its SHA-256 is `6fc46b84badb1ac37b8b19a2d0741aa7728d6c93a9fe949b19f676892dc8fd24`. It has 97 records: 2 empty rows, 82 valid products and 13 rejected rows, among them the repeated SKUs `RS-001` (2 rows) and `BS-021` (3 rows). The acceptance tests import it and check those numbers.

The image ships that file. When `demo.seed` is `true`, the app imports `demo.seed_file` at startup, before it starts serving, and only if the `products` table has no rows at all, deleted ones included. So `make down && make up` gives a populated store, while restarting the app neither imports again nor brings back products that were deleted. Replicas that start together take a session `pg_advisory_lock` on a dedicated connection before checking: the others wait, then find the products and skip. The seed goes through the same import as the API, in `replace` mode, and its summary is logged at `info` level as `demo seed imported`. Rejected rows are not a failure; a seed file that is missing or cannot be read stops the startup.

## Inventory audit

Every change of the physical stock or the reserved units is recorded in `inventory_movements`, written in the same transaction as the change. Both can be rebuilt from the movements:

```
stock    = COALESCE(SUM(delta), 0)
reserved = COALESCE(SUM(reserved_delta), 0)
```

Every movement also stores `stock_after` and `reserved_after`, and a `CHECK` requires that it changes at least one of the two.

Creating a product with a positive stock records one movement with `delta` equal to that stock; there is no other starting balance. A product created with stock 0 has no movement (`delta <> 0`), and the formula still holds. An update records `delta = new stock - old stock`, and no movement when the stock does not change, so reimporting `unchanged` rows records nothing. The product and its movement commit or roll back together.

**Captured by a trigger, not by hand.** `AFTER INSERT OR UPDATE OF stock, reserved ON products` inserts the movement whenever either changes. The guarantee does not depend on every code path remembering to write it: even a manual `UPDATE` from `psql` is recorded. Each writer sets `shop.stock_reason` and `shop.stock_reference` with `set_config(..., true)` inside its transaction, always both of them, with an empty reference when there is none, so a writer never inherits the reason or reference of an earlier statement in the same transaction. The create and edit paths now run in a transaction for that reason.

| Path | `reason` | `delta` | `reserved_delta` | `reference` |
|---|---|---|---|---|
| Create with stock above 0 (API or UI) | `initial_stock` | +n | 0 | none |
| Edit that changes the stock (API or UI) | `manual_adjustment` | ±n | 0 | none |
| Import in `replace` mode and demo seed, only rows created or updated | `import_replace` | ±n | 0 | import id |
| Import in `add` mode, only rows created or updated | `import_add` | +n | 0 | import id |
| Checkout, when the units are reserved | `reservation` | 0 | +q | purchase id |
| Approved payment | `sale` | −q | −q | purchase id |
| Declined payment | `reservation_release` | 0 | −q | purchase id |
| Direct SQL | `unattributed` | any | any | none |

Meaning changes in phase 4: `sale` used to be written when the checkout took the stock; it is now written when the payment is approved, and it lowers the physical stock and the reserved units together. `sale_reversal` is no longer written: a declined payment releases a reservation instead (`reservation_release`) and never touches the physical stock. The reason `import` became `import_replace`, and `import_add` is new. The reference of an import movement is now the import id instead of the file hash, so two runs of the same file can be told apart. There are no migrations, so a fresh volume never contains the old reasons.

A product created by CSV gets one import movement, not an `initial_stock` plus an import movement. `db_user` is the database role of the connection (`shop` for the application), not the demo user. `tx_id` is the PostgreSQL transaction id.

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

A rejection answers `403` with the HTML error page. Behind a reverse proxy that keeps the `Host` header, as the `replicas` compose profile does, nothing has to be configured. A proxy that rewrites it needs `http.public_origins`: a list of origins such as `https://shop.example` that are accepted besides the request's own. Each one must be exactly `scheme://host[:port]` with `http` or `https`; anything else stops the startup, and the error names the position in the list, never the value. The JSON API does not use cookies, so it does not have this check.

### Checkout

```sh
curl -i localhost:8080/api/v1/checkout \
  -H 'X-Demo-User-ID: alice' \
  -H "Idempotency-Key: $(uuidgen)" \
  -H 'Content-Type: application/json' \
  -d '{"items":[{"product_id":"<id>","quantity":2,"expected_unit_price":"29.99"}],"payment_token":"pm_approved"}'
```

Totals sent by the client are ignored and the currency is always `USD`. The simulated payment accepts `pm_approved`, declines `pm_declined` with `card_declined`, and charges `pm_unknown` but answers as if the response were lost, so the purchase stays `PENDING` until the recovery confirms it; any other token is declined with `unknown_payment_token`.

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

Only a definitive decline leads to `FAILED`. A technical error, an unknown outcome, an unavailable provider or a failed finalization after the charge keep the purchase `PENDING` with its units reserved; the [recovery](#periodic-recovery) settles it. There is no atomicity between PostgreSQL and the payment; the recovery covers the interruption windows.

**Lock order.** Every path acquires locks in this order and none inverts it:

| Path | Order |
|---|---|
| Add to cart | Advisory lock of the user's cart → cart row |
| Change quantity, remove, accept price | Cart row |
| Reservation | Purchase row → product rows by product id |
| Finalization, confirmed | Purchase row → product rows by product id → cart rows by product id |
| Finalization, declined | Purchase row → product rows by product id |
| Recovery claim | Purchase rows, `FOR UPDATE SKIP LOCKED`, in one short statement; nothing is held during the charge |
| Import | Advisory lock of the imports → existing product rows by product id (`SELECT … WHERE sku = ANY($1) ORDER BY id FOR UPDATE`) → the upsert, which inserts new products by SKU. An `add` locks every existing product of the file before its first batch and keeps them until the whole file is applied; a `replace` locks the existing products of each batch at the start of its transaction |
| Product edit | One product row |

The confirmed finalization now locks the products too, because the sale lowers their stock. The cart never takes a lock and then one of a purchase or a product, so it forms no cycle with the checkout. The import locks the products it updates in the same order as the checkout, by product id, before it changes any of them, so an import and a checkout wait for each other instead of forming a cycle: while the import waits for a product that a reservation holds, it holds no product. New products are inserted afterwards in SKU order, and no checkout can hold a product that does not exist yet. A test that runs the old order, by SKU, against a reservation gets `40P01`; the same scenario with the import now finishes without it. There are no SQL retries. A reservation that fails with `40P01` or `55P03`, with its rollback confirmed, answers `503 try_again` with `Retry-After: 1`, and the client can retry with the same key. A finalization that fails that way leaves the purchase `PENDING` and answers `202`.

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

A second request with the same key while the first is still reserving waits on the unique index until the first reservation finishes or until `checkout.lock_timeout`, whichever comes first. If the first finishes in time, the second lands in one of these rows. If the wait reaches `checkout.lock_timeout`, the second answers `503 try_again` with `Retry-After: 1` and leaves nothing of its own: no purchase, item, reservation, movement, charge or cart change. The client sends it again with the same key and the same body: once the first purchase is finished it gets that result with `200` and no new charge; while it is still `PENDING` it gets `202` with the `PENDING` purchase and starts no charge of its own, so a definitive result only comes from the checkout that created the purchase or from the recovery. If a commit fails in a way that leaves unknown whether it was applied (for example the connection dropped), the answer is `500 internal_error`; if the purchase was persisted, a retry with the same key finds it and the recovery settles it.

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
| `GET /api/v1/purchases` | The purchases of the user, by `created_at` and `id` descending, with `page` and `page_size` and the page format of the products. An absent or empty parameter takes the default (page 1, 20 per page); any value that is present must be a whole number, `page` from 1 to 100000 and `page_size` from 1 to 100, so `0` is `422 out_of_range`. A malformed value is `422 invalid_format` and the purchases are not read. The purchases page of the UI applies the same rules |
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

### Payment contract

The `payment` port has three operations, all by the purchase id as reference:

| Operation | Answer |
|---|---|
| `Charge(ctx, ChargeRequest{Reference, Token, Amount, Currency})` | `APPROVED` with a provider reference, or `DECLINED` with a reason |
| `Lookup(ctx, reference)` | `APPROVED` (with provider reference, amount and currency), `DECLINED` (with reason) or `NOT_FOUND` |
| `Void(ctx, reference)` | `VOIDED`, after which no charge with that reference can succeed, or the real result if a charge arrived first |

Errors never mean a decline. `ErrOutcomeUnknown` (the request may have reached the provider and it is unknown whether it charged) and `ErrUnavailable` (the provider does not answer) keep the purchase `PENDING`. `ErrReferenceConflict` (the reference was already used with other content) is treated as an unknown outcome and logged as an error.

**Idempotency by reference is a non-negotiable requirement of any provider.** `paymenttest.Contract` is the acceptance test every implementation must pass:

- two concurrent charges with the same reference produce one charge;
- repeating a charge with the same content returns the original result;
- repeating it with other content returns `ErrReferenceConflict` without a new charge;
- an unknown reference is `NOT_FOUND`;
- a voided reference is never charged, and voiding a charged reference returns the real result.

The demo provider is a fake persisted in `payment_fake_charges`, with the reference as primary key: a charge and a void both `INSERT … ON CONFLICT DO NOTHING` and read the row, so exactly one wins. It stores the SHA-256 of the token, never the token, and uses the pool directly, never the transaction of ordering. Its tokens are `pm_approved`, `pm_declined` and `pm_unknown`, which records the approval and then answers `ErrOutcomeUnknown` once, like a response lost on the way back. The fake runs the contract in its integration tests.

**Requirements for phase 4b (a real gateway).** `Lookup` and `Void` by reference become part of the gateway contract, and the provider chosen has to support them. Some providers only void authorizations, not captured charges; with one of those, resolving a stuck purchase goes back to being a manual procedure.

### Time budgets

| Setting | Default | Covers |
|---|---|---|
| `checkout.reserve_timeout` | 3s | The reservation transaction. Derived from the request context |
| `checkout.lock_timeout` | 1s | How long the reservation waits for a product row that another transaction holds, applied with `SET LOCAL lock_timeout`. Must be less than `reserve_timeout`. A checkout that waits longer, for example behind an `add` import, answers `503 try_again` with `Retry-After: 1` and leaves no purchase, reservation, movement, charge or cart change |
| `checkout.charge_timeout` | 6s | The charge. Starts when the reservation commits, from the request context without its cancellation, so a client that disconnects does not leave the purchase half done |
| `checkout.finalize_timeout` | 3s | The finalization. A fresh context, also without the client's cancellation; it never reuses the charge context, which may have expired |
| `checkout.max_pending_age` | 30m | A purchase older than this is no longer charged again: the recovery resolves it with the provider (see [Periodic recovery](#periodic-recovery)) |

All of them must be positive, and `reserve + charge + finalize + 2s` must not exceed `http.write_timeout`, or the app does not start. There is no `checkout.recovery_timeout` any more: it was the total budget of the recovery that ran before serving, which no longer exists. The periodic recovery has its own deadlines, described in [Periodic recovery](#periodic-recovery), and the shutdown budget covers both. A graceful shutdown waits for the requests in flight; one that does not finish in time stays `PENDING` and the recovery of any replica settles it. These budgets do not guarantee that the client receives an answer. What they guarantee is correctness: if the client gets no answer, the idempotency key and the recovery bring the purchase to its right state.

### Cart

The cart lives on the server and is used from the UI only; there are no cart endpoints in the JSON API.

- Adding a product stores its current catalog price as the price seen. Adding it again adds the quantities, up to 99. A deleted or unknown product is an error the UI shows.
- At most 50 lines, because a checkout accepts at most 50 items. Adding a line takes `pg_advisory_xact_lock` on the user's cart and then counts, so two concurrent adds cannot both take the 50th slot. A hash collision of that advisory lock can make two different users wait for each other; it does not affect correctness.
- The cart page checks every line against the catalog and warns about unavailable products, available units below the quantity and prices that changed, with a "Use current price" button.
- The checkout from the cart builds the same items, with the price seen as the expected price, and calls the same use case as the API.
- A confirmed purchase removes a cart line only if its quantity and price seen are still the ones that were bought, so a line changed in another tab during the payment stays as it was left. If the user removes a line and adds it back identical during the payment, that line is removed. A declined payment does not touch the cart.

### Periodic recovery

Every replica runs one recovery worker. It starts with the servers without delaying them, runs its first cycle at once and then one every `recovery.interval`. All the coordination between replicas goes through PostgreSQL.

**A cycle.**

1. **Claim.** One statement takes a batch: `UPDATE purchases SET lease_until = now() + lease, lease_owner = <worker> WHERE id IN (SELECT … WHERE status = 'PENDING' AND created_at < now() - min_age AND next_attempt_at <= now() AND (lease_until IS NULL OR lease_until < now()) ORDER BY next_attempt_at LIMIT batch FOR UPDATE SKIP LOCKED)`. Each worker has its own id, generated when it starts. `min_age` is longer than a whole checkout, so the recovery never competes with a checkout in progress.
2. **Renew, one purchase at a time.** Before charging each purchase, the worker renews its lease: `UPDATE … SET lease_until = now() + lease WHERE id = $1 AND lease_owner = <worker> AND status = 'PENDING'`. If no row changes, another replica took the purchase or it was settled: it is skipped without charging and logged at `info`.
3. **Settle.** The purchase is charged again with the stored reference, token, amount and currency, and finalized with the same finalization as the checkout. No transaction or lock is open during the charge.
4. **Without a definitive result** (unknown outcome, unavailable provider, reference conflict, failed finalization), one statement adds one attempt, sets `next_attempt_at` with exponential backoff, jitter and a ceiling, stores the sanitized cause and releases the lease (`lease_until` and `lease_owner` to `NULL`). A purchase never becomes `FAILED` this way.

**Why the lease is renewed per purchase.** A lease set once for the whole batch runs while the batch is processed in series, so the last purchases of a batch could start after their lease expired, be claimed by another replica, and receive a second charge request. Renewing each purchase right before its charge makes the lease cover exactly one purchase, and the conditional renewal is what stops a worker from charging a purchase it no longer owns. The lease is measured with PostgreSQL's `now()`.

**Deadlines.** Every statement of the recovery and every call to the provider has a deadline, and each purchase has one fixed deadline for all its work:

| Step | Budget | Context |
|---|---|---|
| Claim | `recovery.db_timeout` | The worker's, so a shutdown cancels it |
| Renew | `recovery.db_timeout` | The worker's: a renewal that has not finished when the shutdown starts is cancelled, and that purchase is not charged |
| Charge | `checkout.charge_timeout` | The purchase deadline |
| Lookup, Void | `checkout.charge_timeout` each, the same budget as a charge because each one is a single call to the provider | The purchase deadline |
| Finalization | `checkout.finalize_timeout` | The purchase deadline |
| Reschedule | `recovery.db_timeout` | The purchase deadline |
| Gauges | `recovery.db_timeout` | The worker's; skipped once the worker is stopping |

Each step takes the shorter of its own budget and the time left to the purchase. The purchase deadline does not depend on the worker being cancelled, so a purchase that has started finishes, and it is counted from just before the renewal, on the process's monotonic clock:

```text
purchase = renew + max(charge, lookup + void) + finalize + reschedule
         = 2 × db_timeout + 2 × charge_timeout + finalize_timeout        (19s with the defaults)
lease    > purchase + 2s
```

PostgreSQL starts the lease at its `now()` inside the renewal, which is later than the moment the process starts counting, so a slow renewal is paid from the purchase deadline and never extends the work past the lease. Nothing compares the clocks of the app and the database. The 2s margin absorbs the commit of the last statement and the difference in clock rate, not the renewal, which already has its own term. The lease is never renewed in the middle of a purchase. With the defaults, a 30s lease leaves 9s of slack; a 12s lease with a 6s charge and a 3s finalization is rejected at startup.

**When a deadline expires.**

| Step | What happens |
|---|---|
| Claim | The cycle ends with the error; the next tick tries again |
| Renew | The purchase is not charged. The failure is logged with its cause and the worker moves on to the next purchase if it is still running. The lease of the claim expires on its own |
| Charge, Lookup, Void | The outcome is unknown, never a decline: the purchase stays `PENDING` and is rescheduled if there is time left |
| Finalization | Its transaction rolls back, so there are no partial effects |
| Any step once the purchase deadline has passed | No financial operation starts. The purchase stays `PENDING` and, without time left to reschedule, its lease expires |
| Reschedule | Logged; the lease expires. The attempt is not counted, but `checkout.max_pending_age` still bounds the purchase |
| Gauges | The last published values stay, and the cycle reports that they were not updated |

A failed renewal or reschedule is logged at `info` with its cause below the threshold, like any attempt without a definitive result.

**Stuck and overdue purchases.** When a purchase reaches `recovery.stuck_after_attempts`, or is older than `checkout.max_pending_age`, the worker stops charging it and resolves it with the provider within its renewed lease:

| Provider answer | Result |
|---|---|
| `Lookup` → `APPROVED`, same amount and currency | `CONFIRMED` with the provider reference |
| `Lookup` → `APPROVED`, another amount or currency | Stays `PENDING`, logged at `error`: a different charge is a defect and is never confirmed |
| `Lookup` → `DECLINED` | `FAILED` with the reason, reservation released |
| `Lookup` → `NOT_FOUND`, then `Void` → `VOIDED` | `FAILED` with `payment_not_completed`, reservation released |
| `Lookup` → `NOT_FOUND`, then `Void` → a real result | The row of that result |
| `Lookup` or `Void` does not answer | Stays `PENDING` with backoff; the only case left open |

A purchase becomes `FAILED` only with a `DECLINED` or a `VOIDED` confirmed by the provider. Everything is applied with the checkout's finalization, so releasing or selling the stock and cleaning the cart follow the same rules. Two workers that reach the same purchase apply one effect: the finalization only changes a `PENDING` purchase.

**Logs.** One line per attempt with `purchase_id`, attempt and outcome, never the token:

| Situation | Level |
|---|---|
| Purchase skipped because another worker took or settled it | `info` |
| Attempt without a definitive result, below the threshold | `info` |
| The attempt whose reschedule returns exactly `stuck_after_attempts` | **one** `error`, with the attempts and the last sanitized cause |
| Later attempts | `warn` |
| A stuck purchase that gets resolved | `info`, with the total of attempts |
| Each resolution with the provider | `info` with its result (`error` for a different amount) |

The attempt counter is returned by the database (`UPDATE … RETURNING recovery_attempts`), so the `error` is written once even with several replicas. **Alerts are defined on the metric `checkout_recovery_stuck`, not on the logs**: the gauge keeps the signal while a stuck purchase exists, and a single log line can be missed.

**Shutdown.** SIGTERM reaches the worker and the HTTP servers at the same time:

1. The worker stops claiming at once and does not start the next purchase of its batch.
2. The purchase in progress finishes within its own deadline, while the HTTP servers drain the requests in flight.
3. Once the servers are drained, the closers run in reverse order: the worker is awaited, then the pool and the telemetry are closed.
4. `http.shutdown_timeout` is the single budget for all of it. It must be at least the longer of a checkout request (`reserve + charge + finalize + 2s`, 14s) and a recovered purchase (19s), plus 2s for the pool and the telemetry, so 21s, and greater than `recovery.lease` (30s). The default is 35s, and `stop_grace_period` in `compose.yaml` is 40s, so the container is not killed before the budget ends. An import request in flight is drained like any other request; releasing its imports lock has its own 5s deadline, and a release that fails or runs out of time closes the connection instead of returning it to the pool.
5. If the budget runs out, the pool is not closed under a worker that still holds a connection. Its close is abandoned, and the app exits with an error that says the pool did not close in time. A purchase that did not finish stays `PENDING` and its lease expires.

**Runbook, only for the open case (the provider does not answer).** Find the purchases with `checkout_recovery_stuck` and `checkout_recovery_stuck_reserved_units`, or with `SELECT id, recovery_attempts, last_recovery_error FROM purchases WHERE status = 'PENDING' ORDER BY created_at`. Check the provider by reference (the purchase id) and wait for the recovery to resolve them once it answers. **Never finalize a purchase with direct SQL**: it would skip the stock and cart rules and could contradict what the provider charged.

## Web UI

| Page | What it does |
|---|---|
| `/` | Store with search, category, price range, in-stock filter, sorting and pagination. Results update with htmx without reloading, and the filters stay in the URL so searches can be shared and the Back button works |
| `/products/{id}` | Product details, with "Add to cart" and a quantity when the product has stock |
| `/cart` | The cart: lines, warnings (unavailable, not enough stock, changed price with "Use current price"), quantity changes, removal and the estimated total |
| `/checkout` | The order and a test payment method (`pm_approved`, `pm_declined` or `pm_unknown`). The page carries a fresh idempotency key in a hidden field, so a double click or a resubmission of the same attempt reuses it; disabling the button while the request runs is only a visual aid. If the cart changed and the checkout answers `idempotency_conflict`, the page is generated again with a new key. A price change goes back to the cart with its warnings |
| `/purchases` | The purchase history of the current demo user |
| `/purchases/{id}` | One purchase: confirmed, declined with its reason and the cart kept, or pending with a link to the history |
| `/admin/products` | Product table with search, edit and delete actions |
| `/admin/products/new` | Create a product. Validation errors appear next to each field |
| `/admin/products/{id}/edit` | Edit a product. The form sends the version it showed; if the product changed meanwhile, the page explains it and offers to reload |
| `/admin/products/{id}/delete` | Server-rendered confirmation before deleting |
| `/admin/products/{id}/movements` | The stock movements of a product, newest first |
| `/admin/imports` | Import a CSV file. Choosing "Add to the current inventory" or "Replace the inventory" is required and nothing is chosen in advance; each option shows an example, and the button names the action ("Import adding" or "Import replacing", switched with CSS `:has()`, without JavaScript). The import is applied when the form is sent, and the page shows the counters, the existing products with their stock and price before and after, and the rejected rows. An `add` of a file already added shows the date of the earlier import and asks to choose the file again and tick "Add again" |

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

### D2. Import modes applied in the request

**Context.** With D1, an import that sets the stock is right after a stock count and wrong for a delivery, which should add to what is there. The CSV format of the challenge cannot change.

**Alternatives.**

| Option | Consequence |
|---|---|
| Only a delta (every import adds) | Right for deliveries, but a stock count would need a file of differences that nobody writes by hand |
| Reject imports while there are reservations | Simple, but the store would have to stop selling to load a file |
| Leave reserved products out of the file | The products that sell best would never be updated |
| Two modes, `add` and `replace`, chosen per import | One parameter more, and two rules to explain; each file means what its author meant |

**Decision.** Two modes, a parameter of the import and not a column of the CSV. `replace` is the default in the API so existing clients keep working; the UI requires the choice. `add` adds the units and replaces the other attributes, in one transaction; `replace` sets the units, in batches. Both are applied in the request that sends the file.

A stored preview was also built and discarded: upload, see a server-rendered summary of what would change, then confirm. Confirming from any replica meant keeping the file content in the database between the two requests, and without authentication anybody could fill the database with stored previews; it needed a lifetime, a purge and a limit only to clean up after itself. Applying in the request keeps no content at all, and the report shows afterwards what changed, from the same statement that changed it.

**Example.** There are 50 tents and 1 reserved. A delivery file says 10: with `add` there are 60, with `replace` 10. A count file says 0: `replace` rejects the row with `below_reserved`, because 1 tent is reserved.

**Revisit when** an import needs approval by someone other than the person who sends it, or files become large enough that a mistake is expensive to undo: then a review step is worth its storage, with authentication to protect it.

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

### D5. Several replicas coordinated through PostgreSQL

**Context.** The app has to run as several replicas against one database. Every piece with shared state (the recovery, the payment fake, the seed and the metrics) has to agree without a broker or a coordinator of its own.

**Alternatives for the recovery.**

| Option | Consequence |
|---|---|
| A leader elected with an advisory lock that recovers everything | Simple, but one replica does all the work, a leader that hangs holds the lock with its session, and failover depends on that session ending |
| A lease per purchase, claimed with `FOR UPDATE SKIP LOCKED` and renewed before each charge | Every replica works, a frozen worker only holds the purchases it claimed until their leases expire, and each purchase is protected one by one |

**Decision.** Leases per purchase, with an owner per worker and a conditional renewal before every charge. The rest also goes through PostgreSQL: idempotency keys and the payment reference are unique, the seed takes an advisory lock, one advisory lock lets a single import run at a time, and the gauges are read from the database every cycle.

**Example.** Worker A claims two purchases and stalls on the first. The lease of the second expires and worker B charges and confirms it. When A gets to the second one, its renewal changes no row, so it skips it without charging.

**Revisit when** the volume of pending purchases makes a database poll per replica expensive, or a message broker is introduced (phase 5): a queue with visibility timeouts would play the role of the lease.

### D6. Schema changes without migrations

**Context.** The project is not in production, and phase 4 changes the schema several times.

**Alternatives.** Migration scripts (`002_…`) and a migration tool (more moving parts for data nobody needs to keep); edit `001_schema.sql` and recreate the volume.

**Decision.** `db/init/001_schema.sql` is edited and the stack is recreated with `make down && make up`. The app never runs DDL. At startup it checks that the columns it needs exist and stops with a message that tells to recreate the volume, without naming the column.

**Revisit when** there is data that must survive a deployment; from then on every change needs a migration.

### D9. A purchase keeps the price it was bought at

See [Monetary invariants](#monetary-invariants). Alternatives were reading prices from `products` when showing or charging a purchase (a later import or edit would change what was paid and what is charged again by the recovery), and a copy guarded by triggers. The copy is in `purchase_items` and `purchases.total`; triggers reject changes to it. Revisit when purchases can be amended (partial refunds, returns): that needs new rows that refer to the original, not changes to it.

### D10. One import at a time

**Context.** An import locks the existing products by id before it changes them, the same order as the checkout. Two imports could still wait on each other: a product created by one transaction after another import took its locks is locked later, by SKU, during the upsert. That needs a third transaction committing in the middle, and an `add` holds its transaction for the whole file, so the window grows with the file.

**Alternatives.**

| Option | Consequence |
|---|---|
| Let imports run in parallel and rely on `40P01` | A failed `add` is rolled back entirely and has to be sent again; a `replace` retries the batch row by row, which hides the cause |
| Make the second import wait for the first (`pg_advisory_lock`) | No error, but the HTTP request of the second one stays open for as long as the first file takes, beyond the write timeout for a large file |
| One import at a time, the others answered at once with `409 import_in_progress` | The administrator sends the file again a few seconds later; imports are rare administrative operations |

**Decision.** One import at a time, with `pg_try_advisory_lock` and `409 import_in_progress` with `Retry-After: 5`, in the API and in the UI. A finished key is answered before the lock. Creating a product, in the API and in the UI, takes the same lock in shared mode for its transaction (`pg_try_advisory_xact_lock_shared`), so a creation and an import never run at the same time: whichever comes second answers `409 import_in_progress`. Editing and deleting a product do not take it. The rows of each file are still sorted by SKU before the batches, so the inserts keep one global order even if the serialization is removed one day. The lock per import id that a resumed `replace` used is gone: the single lock already prevents two replicas from applying the same import.

**Example.** An `add` of 10,000 rows is being applied. A second administrator uploads a count: they see "Another import is being applied" and nothing changes; ten seconds later the same upload is applied.

**Revisit when** imports become frequent or automated, for example a supplier feed every few minutes: then a queue of imports applied by a worker is better than asking clients to retry.

## Known limitations

- **Search does not use an index.** `ILIKE '%q%'` scans the active products. This is fine for thousands of products; a larger catalog would need full-text search or trigram indexes.
- **The search total is not transactionally consistent with the items.** They come from two separate queries, so with concurrent writes the `total` can differ from the items of a page.
- **An import sets the physical stock to the value in the file.** It is absolute, not a delta, so the file must describe the warehouse at the time it is applied: a file written before a sale that was approved later still sets the units it lists. Reservations are kept apart, so an import no longer brings back or loses reserved units, and a row below the reserved units is rejected.
- **No authentication.** Anyone who can reach port 8080 can create, edit, delete and import products through `/api/v1` and `/admin`, and act as any demo user. Do not expose the published listener to untrusted clients without access control in front of it, such as an authenticating reverse proxy or a private network.
- **Forms are protected by a same-origin check, not by CSRF tokens.** Behind a reverse proxy that changes the host, the public host would have to be configured.
- **There is no review before an import is applied.** The report shows afterwards what changed; a wrong file is fixed with another import. An `add` sent by mistake is undone with a `replace` of the right values.
- **An interrupted `replace` stays `APPLYING` until someone sends the same file again with the same key.** In the UI that means sending the same form again; a new form has a new key, and the interrupted import then remains `APPLYING` as a record. The stock is not wrong for it: the batches applied hold the values of the file.
- **A large `add` holds the locks of all its products until it finishes**, so checkouts of those products wait for it.
- **Creating a product waits for imports.** No product can be created while an import is applied, and no import starts while a creation is in its transaction; the second one answers `409 import_in_progress` with `Retry-After: 5`. That removes the last cycle between an import and a checkout. A deadlock or a lock timeout from any other contention is still handled: a reservation answers `503 try_again` (`40P01` and `55P03`), a finalization leaves the purchase `PENDING`, an `add` is rolled back entirely and a `replace` retries that batch row by row.
- **An `add` holds its products until the file is applied.** A checkout of one of them waits at most `checkout.lock_timeout` and then answers `503 try_again` without reserving anything; the client sends it again with the same key.
- **The lease reduces double charge requests but cannot remove them.** A worker frozen after renewing a lease and before charging, for longer than the lease, can charge at the same time as the replica that took the purchase over. The purchase deadline narrows this window, because no step starts once the deadline has passed, but it cannot close it: the process can pause with a request already on its way, or resume and send one before its timers fire. Only the provider can close this, by rejecting the second request: that is why idempotency by reference is a non-negotiable requirement of any gateway. The database stays right either way (`Finish … WHERE status = 'PENDING'`), and the demo fake already rejects it with its primary key.
- **A long outage of the provider keeps purchases open.** While `Lookup` and `Void` do not answer, a stuck purchase stays `PENDING` and its units stay reserved; `checkout_recovery_stuck_reserved_units` shows how many. See the runbook in [Periodic recovery](#periodic-recovery).
- **A gateway that does not meet the payment contract can charge twice.** The contract test is the acceptance criterion for any provider.
- **The payment is a simulated provider.** It is persisted and meets the contract, but it never times out on its own; unavailability is exercised by the tests.
- **Movements and purchase snapshots are immutable only for the application.** The table owner or a superuser can disable their triggers, and `session_replication_role = replica` skips them.
- **No data migrations, by design.** Pre-production data is disposable: it is recreated from an initial data load after `make down && make up`. Production skips that initial load.

## Evolution

Pieces left out of this phase, with what would justify them:

| Piece | Trigger |
|---|---|
| Real HTTP payment provider (phase 4b: HTTP adapter, retries, circuit breaker) | Choosing a provider. It plugs in behind the payment port without changing ordering, if it passes `paymenttest.Contract` and supports `Lookup` and `Void` by reference |
| `inventory_movements`, `purchases` and `purchase_items` owned by a separate role, with `REVOKE UPDATE, DELETE` (column-level `UPDATE` grants for the purchase outcome) for the application role | Production, where the audit must hold even against the application's own credentials |
| Real authentication and sessions instead of demo users | Anyone other than an evaluator using the store |
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
| `checkout.reserve_timeout`, `checkout.charge_timeout`, `checkout.finalize_timeout` | `3s`, `6s` and `3s`. See [Time budgets](#time-budgets) |
| `checkout.lock_timeout` | `1s`. Must be less than `checkout.reserve_timeout` |
| `checkout.max_pending_age` | `30m`. Must be greater than `recovery.min_age` and `recovery.lease`. See [Periodic recovery](#periodic-recovery) |
| `recovery.interval`, `recovery.batch_size` | `10s` and `20` |
| `recovery.min_age` | `30s`. Must be greater than `reserve + charge + finalize + 2s`, so the recovery never takes a checkout in progress |
| `recovery.lease` | `30s`. Must be greater than `2 × db_timeout + 2 × charge + finalize + 2s`, the deadline of one purchase plus the margin |
| `recovery.db_timeout` | `2s`. The deadline of each recovery statement. See [Periodic recovery](#periodic-recovery) |
| `http.shutdown_timeout` | `35s`. Must be at least the longer of a checkout request and a recovered purchase, plus 2s, and greater than `recovery.lease` |
| `recovery.base_backoff`, `recovery.max_backoff` | `5s` and `5m`; the base cannot exceed the ceiling |
| `recovery.stuck_after_attempts` | `10` |
| `http.public_origins` | `[]`. See [Same-origin check for forms](#same-origin-check-for-forms) |

## Observability

- **Logs:** JSON on stdout with `timestamp`, `severity` and `message`. Every request line carries `request_id`, `trace_id` and `span_id`. Sensitive fields and values (passwords, tokens, bearer credentials, card numbers) are redacted. The configuration is never logged. An import that recovers from a failed batch logs it once as a warning; a row the database finally rejects is logged once as an error.
- **Metrics:** OpenTelemetry exported in Prometheus format on a separate port that `compose.yaml` does not publish (the image has no shell, so use `make up-debug`, which publishes it on `127.0.0.1:9100`). Imports record `import_rows_total{outcome}`, with `created`, `updated`, `unchanged`, `skipped` and `rejected`, and `import_duration_seconds`, which times every import including the ones rejected as a whole. Every checkout that reaches the use case counts once in `checkout_total{outcome}`: `confirmed`, `declined`, `pending` (a stored purchase without a definitive payment result), `insufficient_stock`, `price_changed`, `product_unavailable`, `invalid` (field rules and the idempotency key, which the use case validates), `idempotency_conflict`, `replayed`, `in_progress`, `try_again` (only `40P01` and `55P03`) and `error` (unexpected failures); a failure counts as `try_again` or as `error`, never both. Requests rejected before the use case (unknown demo user, unreadable JSON, a body too large) are not counted there; the HTTP server metrics cover them. The recovery counts every attempt in `checkout_recovery_total{outcome}` (`confirmed`, `declined`, `still_pending`, `error`, `skipped`) and every resolution with the provider in `checkout_recovery_resolutions_total{result}` (`confirmed`, `declined`, `voided`, `mismatch`, `error`); every series is recorded with 0 at startup so it exists before the first attempt. After each cycle it publishes the gauges `checkout_pending_purchases`, `checkout_oldest_pending_seconds`, `checkout_recovery_stuck` and `checkout_recovery_stuck_reserved_units`. Every replica publishes the same values from the same database, so the gauges are aggregated across replicas with `max`, never summed. No label carries users, purchases, tokens or error text.
- **Checkout logs:** every checkout that ends with a purchase writes one line with `purchase_id` and `status`, at `warn` with its cause when the purchase stays `PENDING`. The payment token is never logged. An unexpected failure is logged once, by the HTTP adapter.
- **Traces:** OpenTelemetry over OTLP gRPC, disabled by default. Incoming `traceparent` headers are honoured, and spans are named after the matched route. Under the request span, an import adds one `catalog.import` span for the whole operation, and every statement sent to PostgreSQL adds a `postgres <OPERATION>` child span, such as `postgres SELECT` or `postgres COMMIT`. The trace shows how long the import took, which statements ran, how long each one took and which one failed; the time not spent in PostgreSQL is reading, validating and preparing the rows. Spans carry the operation name only: never the SQL text, its arguments, a file name or a product value. The checkout adds no spans of its own; its PostgreSQL statements hang from the request span. A file rejected as a whole is an expected rejection: the caller still gets the error, but the span is not marked as failed.

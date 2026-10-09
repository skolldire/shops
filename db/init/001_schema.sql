BEGIN;

CREATE TABLE products (
    id          uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    sku         text          NOT NULL UNIQUE,
    name        text          NOT NULL,
    description text          NOT NULL DEFAULT '',
    category    text          NOT NULL,
    price       numeric(12,2) NOT NULL CHECK (price > 0),
    stock       integer       NOT NULL CHECK (stock >= 0),
    weight_kg   numeric(10,3) NOT NULL CHECK (weight_kg >= 0),
    status      text          NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DELETED')),
    created_at  timestamptz   NOT NULL DEFAULT now(),
    updated_at  timestamptz   NOT NULL DEFAULT now(),
    deleted_at  timestamptz   NULL
);

CREATE INDEX products_status_category_idx ON products (status, category);
CREATE INDEX products_status_price_idx ON products (status, price);

CREATE TABLE purchases (
    id              uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         text          NOT NULL,
    idempotency_key uuid          NOT NULL,
    request_hash    text          NOT NULL,
    status          text          NOT NULL CHECK (status IN ('PENDING', 'CONFIRMED', 'FAILED')),
    currency        char(3)       NOT NULL DEFAULT 'USD',
    total           numeric(12,2) NOT NULL CHECK (total >= 0),
    payment_ref     text          NULL,
    failure_reason  text          NULL,
    created_at      timestamptz   NOT NULL DEFAULT now(),
    updated_at      timestamptz   NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key)
);

CREATE INDEX purchases_status_created_at_idx ON purchases (status, created_at);
CREATE INDEX purchases_user_id_created_at_idx ON purchases (user_id, created_at DESC);

CREATE TABLE purchase_items (
    purchase_id uuid          NOT NULL REFERENCES purchases (id),
    product_id  uuid          NOT NULL,
    sku         text          NOT NULL,
    name        text          NOT NULL,
    unit_price  numeric(12,2) NOT NULL CHECK (unit_price > 0),
    quantity    integer       NOT NULL CHECK (quantity BETWEEN 1 AND 99),
    line_total  numeric(12,2) NOT NULL CHECK (line_total >= 0),
    PRIMARY KEY (purchase_id, product_id)
);

CREATE TABLE cart_items (
    user_id         text          NOT NULL,
    product_id      uuid          NOT NULL,
    quantity        integer       NOT NULL CHECK (quantity BETWEEN 1 AND 99),
    unit_price_seen numeric(12,2) NOT NULL CHECK (unit_price_seen > 0),
    created_at      timestamptz   NOT NULL DEFAULT now(),
    updated_at      timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, product_id)
);

COMMIT;

BEGIN;

CREATE TABLE products (
    id          uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    sku         text          NOT NULL,
    name        text          NOT NULL,
    description text          NOT NULL DEFAULT '',
    category    text          NOT NULL,
    price       numeric(12,2) NOT NULL CHECK (price > 0),
    stock       integer       NOT NULL CHECK (stock >= 0),
    reserved    integer       NOT NULL DEFAULT 0,
    weight_kg   numeric(10,3) NOT NULL CHECK (weight_kg >= 0),
    status      text          NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DELETED')),
    created_at  timestamptz   NOT NULL DEFAULT now(),
    updated_at  timestamptz   NOT NULL DEFAULT now(),
    deleted_at  timestamptz   NULL,
    version     integer       NOT NULL DEFAULT 1 CHECK (version >= 1),
    CONSTRAINT products_sku_key UNIQUE (sku),
    CONSTRAINT products_reserved_check CHECK (reserved >= 0 AND reserved <= stock)
);

CREATE INDEX products_status_category_idx ON products (status, category);
CREATE INDEX products_status_price_idx ON products (status, price);

CREATE TABLE inventory_movements (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    product_id     uuid        NOT NULL REFERENCES products (id),
    delta          integer     NOT NULL,
    stock_after    integer     NOT NULL CHECK (stock_after >= 0),
    reserved_delta integer     NOT NULL DEFAULT 0,
    reserved_after integer     NOT NULL CHECK (reserved_after >= 0),
    reason         text        NOT NULL,
    reference      text        NULL,
    db_user        text        NOT NULL DEFAULT current_user,
    tx_id          xid8        NOT NULL DEFAULT pg_current_xact_id(),
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT inventory_movements_change_check CHECK (delta <> 0 OR reserved_delta <> 0)
);

CREATE INDEX inventory_movements_product_id_id_idx ON inventory_movements (product_id, id);

CREATE FUNCTION record_stock_movement() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    change          integer;
    reserved_change integer;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        change := NEW.stock - OLD.stock;
        reserved_change := NEW.reserved - OLD.reserved;
    ELSE
        change := NEW.stock;
        reserved_change := NEW.reserved;
    END IF;
    IF change <> 0 OR reserved_change <> 0 THEN
        INSERT INTO inventory_movements (product_id, delta, stock_after, reserved_delta, reserved_after, reason, reference)
        VALUES (
            NEW.id,
            change,
            NEW.stock,
            reserved_change,
            NEW.reserved,
            COALESCE(NULLIF(current_setting('shop.stock_reason', true), ''), 'unattributed'),
            NULLIF(current_setting('shop.stock_reference', true), '')
        );
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER products_stock_movement
AFTER INSERT OR UPDATE OF stock, reserved ON products
FOR EACH ROW EXECUTE FUNCTION record_stock_movement();

CREATE FUNCTION reject_inventory_movement_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'inventory_movements is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER inventory_movements_immutable
BEFORE UPDATE OR DELETE ON inventory_movements
FOR EACH ROW EXECUTE FUNCTION reject_inventory_movement_change();

CREATE TRIGGER inventory_movements_no_truncate
BEFORE TRUNCATE ON inventory_movements
FOR EACH STATEMENT EXECUTE FUNCTION reject_inventory_movement_change();

CREATE TABLE purchases (
    id                  uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             text          NOT NULL,
    idempotency_key     uuid          NOT NULL,
    request_hash        text          NOT NULL,
    status              text          NOT NULL CHECK (status IN ('PENDING', 'CONFIRMED', 'FAILED')),
    currency            char(3)       NOT NULL DEFAULT 'USD',
    total               numeric(12,2) NOT NULL CHECK (total >= 0),
    payment_token       text          NOT NULL,
    payment_ref         text          NULL,
    failure_reason      text          NULL,
    created_at          timestamptz   NOT NULL DEFAULT now(),
    updated_at          timestamptz   NOT NULL DEFAULT now(),
    recovery_attempts   integer       NOT NULL DEFAULT 0 CHECK (recovery_attempts >= 0),
    next_attempt_at     timestamptz   NOT NULL DEFAULT now(),
    lease_until         timestamptz   NULL,
    lease_owner         uuid          NULL,
    last_recovery_error text          NULL,
    UNIQUE (user_id, idempotency_key),
    CONSTRAINT purchases_lease_check CHECK ((lease_until IS NULL) = (lease_owner IS NULL))
);

CREATE INDEX purchases_status_created_at_idx ON purchases (status, created_at);
CREATE INDEX purchases_recovery_idx ON purchases (next_attempt_at) WHERE status = 'PENDING';
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

CREATE FUNCTION reject_purchase_item_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'purchase_items is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER purchase_items_immutable
BEFORE UPDATE OR DELETE ON purchase_items
FOR EACH ROW EXECUTE FUNCTION reject_purchase_item_change();

CREATE TRIGGER purchase_items_no_truncate
BEFORE TRUNCATE ON purchase_items
FOR EACH STATEMENT EXECUTE FUNCTION reject_purchase_item_change();

CREATE FUNCTION guard_purchase_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.user_id, NEW.idempotency_key, NEW.request_hash, NEW.currency, NEW.total, NEW.payment_token, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.user_id, OLD.idempotency_key, OLD.request_hash, OLD.currency, OLD.total, OLD.payment_token, OLD.created_at) THEN
        RAISE EXCEPTION 'purchases keep their amount, identity and payment token: only the outcome may change'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER purchases_snapshot_immutable
BEFORE UPDATE ON purchases
FOR EACH ROW EXECUTE FUNCTION guard_purchase_snapshot();

CREATE TABLE cart_items (
    user_id         text          NOT NULL,
    product_id      uuid          NOT NULL,
    quantity        integer       NOT NULL CHECK (quantity BETWEEN 1 AND 99),
    unit_price_seen numeric(12,2) NOT NULL CHECK (unit_price_seen > 0),
    created_at      timestamptz   NOT NULL DEFAULT now(),
    updated_at      timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, product_id)
);

CREATE TABLE imports (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key uuid        NULL,
    mode            text        NOT NULL CHECK (mode IN ('add', 'replace')),
    status          text        NOT NULL CHECK (status IN ('APPLYING', 'APPLIED')),
    file_name       text        NOT NULL,
    file_bytes      bigint      NOT NULL CHECK (file_bytes >= 0),
    file_hash       text        NOT NULL,
    records         integer     NOT NULL DEFAULT 0,
    skipped         integer     NOT NULL DEFAULT 0,
    created         integer     NOT NULL DEFAULT 0,
    updated         integer     NOT NULL DEFAULT 0,
    unchanged       integer     NOT NULL DEFAULT 0,
    rejected        integer     NOT NULL DEFAULT 0,
    errors          jsonb       NOT NULL DEFAULT '[]',
    changes         jsonb       NOT NULL DEFAULT '[]',
    created_at      timestamptz NOT NULL DEFAULT now(),
    applied_at      timestamptz NULL,
    CONSTRAINT imports_idempotency_key_key UNIQUE (idempotency_key),
    CONSTRAINT imports_applied_check CHECK ((status = 'APPLIED') = (applied_at IS NOT NULL))
);

CREATE INDEX imports_applied_add_hash_idx ON imports (file_hash) WHERE mode = 'add' AND status = 'APPLIED';

CREATE TABLE outbox (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id        uuid        NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    aggregate_type  text        NOT NULL CHECK (aggregate_type IN ('Purchase')),
    aggregate_id    uuid        NOT NULL,
    type            text        NOT NULL CHECK (type IN ('PurchaseConfirmed', 'PurchaseFailed')),
    version         integer     NOT NULL CHECK (version >= 1),
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    correlation_id  text        NOT NULL,
    payload         jsonb       NOT NULL,
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner     uuid        NULL,
    lease_until     timestamptz NULL,
    published_at    timestamptz NULL,
    last_error      text        NULL,
    CONSTRAINT outbox_lease_check CHECK ((lease_owner IS NULL) = (lease_until IS NULL))
);

CREATE UNIQUE INDEX outbox_one_event_per_purchase ON outbox (aggregate_id) WHERE aggregate_type = 'Purchase';
CREATE INDEX outbox_pending_idx ON outbox (next_attempt_at, id) WHERE published_at IS NULL;

CREATE TABLE inbox (
    consumer     text        NOT NULL,
    event_id     uuid        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

CREATE TABLE product_sales (
    product_id uuid        PRIMARY KEY,
    units_sold bigint      NOT NULL DEFAULT 0 CHECK (units_sold >= 0),
    purchases  bigint      NOT NULL DEFAULT 0 CHECK (purchases >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE payment_fake_charges (
    reference    text          PRIMARY KEY,
    status       text          NOT NULL CHECK (status IN ('APPROVED', 'DECLINED', 'VOIDED')),
    amount       numeric(12,2) NULL,
    currency     char(3)       NULL,
    token_hash   text          NULL,
    provider_ref text          NULL,
    reason       text          NULL,
    created_at   timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT payment_fake_charges_charge_check
        CHECK (status = 'VOIDED' OR (amount IS NOT NULL AND currency IS NOT NULL AND token_hash IS NOT NULL))
);

COMMIT;

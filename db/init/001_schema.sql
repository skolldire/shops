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
    id              uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         text          NOT NULL,
    idempotency_key uuid          NOT NULL,
    request_hash    text          NOT NULL,
    status          text          NOT NULL CHECK (status IN ('PENDING', 'CONFIRMED', 'FAILED')),
    currency        char(3)       NOT NULL DEFAULT 'USD',
    total           numeric(12,2) NOT NULL CHECK (total >= 0),
    payment_token   text          NOT NULL,
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

COMMIT;

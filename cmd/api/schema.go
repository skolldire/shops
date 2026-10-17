package main

import "github.com/skolldire/shops/internal/platform/database"

var requiredColumns = []database.Column{
	{Table: "products", Name: "reserved"},
	{Table: "inventory_movements", Name: "reserved_delta"},
	{Table: "inventory_movements", Name: "reserved_after"},
	{Table: "imports", Name: "status"},
	{Table: "imports", Name: "errors"},
	{Table: "imports", Name: "changes"},
	{Table: "purchases", Name: "recovery_attempts"},
	{Table: "purchases", Name: "next_attempt_at"},
	{Table: "purchases", Name: "lease_until"},
	{Table: "purchases", Name: "lease_owner"},
	{Table: "purchases", Name: "last_recovery_error"},
	{Table: "payment_fake_charges", Name: "token_hash"},
	{Table: "outbox", Name: "aggregate_type"},
	{Table: "outbox", Name: "lease_owner"},
	{Table: "inbox", Name: "event_id"},
	{Table: "product_sales", Name: "units_sold"},
}

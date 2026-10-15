package main

import "github.com/skolldire/shops/internal/platform/database"

var requiredColumns = []database.Column{
	{Table: "products", Name: "reserved"},
	{Table: "inventory_movements", Name: "reserved_delta"},
	{Table: "inventory_movements", Name: "reserved_after"},
	{Table: "imports", Name: "status"},
	{Table: "imports", Name: "cancel_reason"},
	{Table: "imports", Name: "content"},
	{Table: "imports", Name: "preview"},
}

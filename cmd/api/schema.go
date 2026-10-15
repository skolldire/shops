package main

import "github.com/skolldire/shops/internal/platform/database"

var requiredColumns = []database.Column{
	{Table: "products", Name: "reserved"},
	{Table: "inventory_movements", Name: "reserved_delta"},
	{Table: "inventory_movements", Name: "reserved_after"},
}

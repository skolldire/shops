package database

import (
	"context"
	"errors"
)

var ErrSchemaOutdated = errors.New("database: the schema is out of date, recreate the volume with make down && make up")

type Column struct {
	Table string
	Name  string
}

func RequireColumns(ctx context.Context, q Querier, columns []Column) error {
	if len(columns) == 0 {
		return nil
	}
	tables, names := make([]string, len(columns)), make([]string, len(columns))
	for i, c := range columns {
		tables[i], names[i] = c.Table, c.Name
	}
	var missing bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM unnest($1::text[], $2::text[]) AS required(table_name, column_name)
		     WHERE NOT EXISTS (
		         SELECT 1 FROM information_schema.columns c
		         WHERE c.table_schema = current_schema()
		           AND c.table_name = required.table_name
		           AND c.column_name = required.column_name))`, tables, names).Scan(&missing)
	if err != nil {
		return safeError("check schema", err)
	}
	if missing {
		return ErrSchemaOutdated
	}
	return nil
}

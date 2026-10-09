package database

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/secret"
)

func TestDescribe(t *testing.T) {
	auth := &pgconn.PgError{Code: "28P01", Message: `password authentication failed for user "shop"`}
	tests := map[string]struct {
		err  error
		want string
	}{
		"authentication": {fmt.Errorf("failed to connect to `user=shop database=shop`: %w", auth), "authentication failed (SQLSTATE 28P01)"},
		"missing db":     {&pgconn.PgError{Code: "3D000"}, "database does not exist (SQLSTATE 3D000)"},
		"other sqlstate": {&pgconn.PgError{Code: "XX000"}, "server rejected the request (SQLSTATE XX000)"},
		"timeout":        {fmt.Errorf("dial 10.0.0.5:5432: %w", context.DeadlineExceeded), "timed out"},
		"canceled":       {context.Canceled, "canceled"},
		"unknown":        {errors.New("user=shop host=10.0.0.5 weird"), "connection failed"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tt.want, describe(tt.err))
		})
	}
}

func TestOpenErrorsHideConnectionDetails(t *testing.T) {
	tests := map[string]struct {
		host string
		want string
	}{
		"refused": {"127.0.0.1", "database: ping: dial failed: connection refused"},
		"dns":     {"nonexistent.invalid", "database: ping: host lookup failed: "},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Host, cfg.Port, cfg.User, cfg.Name = tt.host, 1, "shop_user", "shop_db"
			cfg.Password = secret.New("pw-123")
			cfg.ConnectTimeout = 3 * time.Second

			_, err := Open(context.Background(), cfg)

			require.ErrorContains(t, err, tt.want)
			for _, leak := range []string{"shop_user", "shop_db", "pw-123", tt.host, ":1"} {
				require.NotContains(t, err.Error(), leak)
			}
			var connectErr *pgconn.ConnectError
			require.ErrorAs(t, err, &connectErr)
		})
	}

	_, err := Open(context.Background(), Config{Host: "127.0.0.1", Port: 1, Name: "x", User: "x", SSLMode: "disable", MaxConns: 1, ConnectTimeout: time.Second})
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
}

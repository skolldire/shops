package database

import (
	"context"
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

type connectionError struct {
	msg string
	err error
}

func (e *connectionError) Error() string { return e.msg }

func (e *connectionError) Unwrap() error { return e.err }

func safeError(op string, err error) error {
	return &connectionError{msg: "database: " + op + ": " + describe(err), err: err}
}

func describe(err error) string {
	var (
		pgErr  *pgconn.PgError
		dnsErr *net.DNSError
		opErr  *net.OpError
	)
	switch {
	case errors.As(err, &pgErr):
		return serverError(pgErr.Code) + " (SQLSTATE " + pgErr.Code + ")"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dnsErr):
		return "host lookup failed: " + dnsErr.Err
	case errors.As(err, &opErr):
		return opErr.Op + " failed: " + rootCause(opErr.Err)
	}
	return "connection failed"
}

func serverError(code string) string {
	switch code {
	case "28P01", "28000":
		return "authentication failed"
	case "3D000":
		return "database does not exist"
	case "53300":
		return "too many connections"
	case "57P03":
		return "server is starting up or shutting down"
	}
	return "server rejected the request"
}

func rootCause(err error) string {
	for next := errors.Unwrap(err); next != nil; next = errors.Unwrap(err) {
		err = next
	}
	return err.Error()
}

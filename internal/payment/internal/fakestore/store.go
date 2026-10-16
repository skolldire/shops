package fakestore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

const (
	Approved = "APPROVED"
	Declined = "DECLINED"
	Voided   = "VOIDED"
)

type Row struct {
	Reference   string
	Status      string
	Amount      decimal.Decimal
	Currency    string
	TokenHash   string
	ProviderRef string
	Reason      string
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("payment: the simulated provider requires a pool")
	}
	return &Store{pool: pool}, nil
}

const columns = `reference, status, COALESCE(amount::text, '0'), COALESCE(currency, ''), COALESCE(token_hash, ''),
COALESCE(provider_ref, ''), COALESCE(reason, '')`

func (s *Store) Charge(ctx context.Context, r Row) (Row, bool, error) {
	row, err := scan(s.pool.QueryRow(ctx,
		`INSERT INTO payment_fake_charges (reference, status, amount, currency, token_hash, provider_ref, reason)
		 VALUES ($1, $2, $3::numeric, $4, $5, NULLIF($6, ''), NULLIF($7, ''))
		 ON CONFLICT (reference) DO NOTHING
		 RETURNING `+columns,
		r.Reference, r.Status, r.Amount.String(), r.Currency, r.TokenHash, r.ProviderRef, r.Reason))
	if err == nil {
		return row, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Row{}, false, fmt.Errorf("payment: record the simulated charge: %w", err)
	}
	row, found, err := s.Get(ctx, r.Reference)
	switch {
	case err != nil:
		return Row{}, false, err
	case !found:
		return Row{}, false, errors.New("payment: the simulated charge disappeared")
	}
	return row, false, nil
}

func (s *Store) Get(ctx context.Context, reference string) (Row, bool, error) {
	row, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM payment_fake_charges WHERE reference = $1`, reference))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Row{}, false, nil
	case err != nil:
		return Row{}, false, fmt.Errorf("payment: read the simulated charge: %w", err)
	}
	return row, true, nil
}

func (s *Store) Void(ctx context.Context, reference string) (Row, error) {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO payment_fake_charges (reference, status) VALUES ($1, 'VOIDED') ON CONFLICT (reference) DO NOTHING`, reference); err != nil {
		return Row{}, fmt.Errorf("payment: void the simulated reference: %w", err)
	}
	row, found, err := s.Get(ctx, reference)
	switch {
	case err != nil:
		return Row{}, err
	case !found:
		return Row{}, errors.New("payment: the voided reference disappeared")
	}
	return row, nil
}

func scan(row pgx.Row) (Row, error) {
	var (
		r      Row
		amount string
	)
	if err := row.Scan(&r.Reference, &r.Status, &amount, &r.Currency, &r.TokenHash, &r.ProviderRef, &r.Reason); err != nil {
		return Row{}, err
	}
	var err error
	if r.Amount, err = decimal.NewFromString(amount); err != nil {
		return Row{}, fmt.Errorf("payment: parse the simulated amount: %w", err)
	}
	return r, nil
}

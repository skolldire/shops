package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/payment/internal/fakestore"
)

const (
	tokenApproved = "pm_approved"
	tokenDeclined = "pm_declined"
	tokenUnknown  = "pm_unknown"
	refPrefix     = "fake_"
)

type Module struct {
	store *fakestore.Store
}

func NewModule(pool *pgxpool.Pool) (*Module, error) {
	store, err := fakestore.New(pool)
	if err != nil {
		return nil, err
	}
	return &Module{store: store}, nil
}

func (m *Module) Charge(ctx context.Context, r ChargeRequest) (ChargeResult, error) {
	attempt := fakestore.Row{Reference: r.Reference, Amount: r.Amount, Currency: r.Currency, TokenHash: tokenHash(r.Token)}
	switch r.Token {
	case tokenApproved, tokenUnknown:
		attempt.Status, attempt.ProviderRef = fakestore.Approved, refPrefix+uuid.NewString()
	case tokenDeclined:
		attempt.Status, attempt.Reason = fakestore.Declined, ReasonCardDeclined
	default:
		attempt.Status, attempt.Reason = fakestore.Declined, ReasonUnknownPaymentToken
	}
	row, inserted, err := m.store.Charge(ctx, attempt)
	switch {
	case err != nil:
		return ChargeResult{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	case row.Status == fakestore.Voided:
		return ChargeResult{Status: StatusDeclined, Reason: ReasonVoided}, nil
	case !inserted && (!row.Amount.Equal(r.Amount) || row.Currency != r.Currency || row.TokenHash != attempt.TokenHash):
		return ChargeResult{}, ErrReferenceConflict
	case inserted && r.Token == tokenUnknown:
		return ChargeResult{}, ErrOutcomeUnknown
	}
	return ChargeResult{Status: Status(row.Status), ProviderRef: row.ProviderRef, Reason: row.Reason}, nil
}

func (m *Module) Lookup(ctx context.Context, reference string) (Record, error) {
	row, found, err := m.store.Get(ctx, reference)
	switch {
	case err != nil:
		return Record{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	case !found:
		return Record{Status: StatusNotFound}, nil
	}
	return record(row), nil
}

func (m *Module) Void(ctx context.Context, reference string) (Record, error) {
	row, err := m.store.Void(ctx, reference)
	if err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return record(row), nil
}

func record(row fakestore.Row) Record {
	return Record{Status: Status(row.Status), ProviderRef: row.ProviderRef, Reason: row.Reason, Amount: row.Amount, Currency: row.Currency}
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

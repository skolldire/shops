package app

import (
	"context"
	"errors"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

var (
	ErrImportNotFound         = errors.New("catalog: import not found")
	ErrInvalidIdempotencyKey  = errors.New("catalog: the idempotency key must be a UUID")
	ErrIdempotencyKeyRequired = errors.New("catalog: an add import requires an idempotency key")
	ErrIdempotencyConflict    = errors.New("catalog: the idempotency key was used with another file or mode")
	ErrDuplicateFile          = errors.New("catalog: the same file was already added")
	ErrImportInProgress       = errors.New("catalog: another import is being applied")
)

type DuplicateFileError struct {
	Previous domain.ImportStamp
}

func (e *DuplicateFileError) Error() string {
	return "catalog: the same file was already added to the stock"
}

func (e *DuplicateFileError) Is(target error) bool {
	return target == ErrDuplicateFile
}

type ImportRecord struct {
	ID     string
	Key    string
	Mode   domain.ImportMode
	Status domain.ImportStatus
	File   ImportFile
	Report ImportReport
	Stamp  domain.ImportStamp
}

type ImportLog interface {
	Create(ctx context.Context, rec ImportRecord) (ImportRecord, bool, error)
	Get(ctx context.Context, id string) (ImportRecord, error)
	LastAppliedAdd(ctx context.Context, hash, exceptID string) (domain.ImportStamp, bool, error)
	Finish(ctx context.Context, id string, from domain.ImportStatus, report ImportReport) (bool, error)
	ByKey(ctx context.Context, key string) (ImportRecord, bool, error)
	LockImports(ctx context.Context) (context.Context, func(), bool, error)
}

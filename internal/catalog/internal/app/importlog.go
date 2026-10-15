package app

import (
	"context"
	"errors"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

var (
	ErrImportNotFound         = errors.New("catalog: import not found")
	ErrTooManyPreviews        = errors.New("catalog: too many import previews are waiting for a confirmation")
	ErrInvalidIdempotencyKey  = errors.New("catalog: the idempotency key must be a UUID")
	ErrIdempotencyKeyRequired = errors.New("catalog: an add import requires an idempotency key")
	ErrIdempotencyConflict    = errors.New("catalog: the idempotency key was used with another file or mode")
	ErrImportCancelled        = errors.New("catalog: the import was cancelled")
	ErrImportNotApplicable    = errors.New("catalog: the import cannot be applied")
	ErrDuplicateFile          = errors.New("catalog: the same file was already added")
	ErrPreviewExpired         = errors.New("catalog: the import preview expired, upload the file again")
	ErrImportNotCancellable   = errors.New("catalog: only an import preview can be cancelled")
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
	ID           string
	Key          string
	Mode         domain.ImportMode
	Status       domain.ImportStatus
	CancelReason string
	File         ImportFile
	Content      []byte
	Preview      []domain.PreviewLine
	Report       ImportReport
	Stamp        domain.ImportStamp
	Expired      bool
}

type ImportLog interface {
	CreatePreview(ctx context.Context, rec ImportRecord) (ImportRecord, bool, error)
	Create(ctx context.Context, rec ImportRecord) (ImportRecord, bool, error)
	Get(ctx context.Context, id string) (ImportRecord, error)
	LastAppliedAdd(ctx context.Context, hash, exceptID string) (domain.ImportStamp, bool, error)
	StartApplying(ctx context.Context, id string) (bool, error)
	Finish(ctx context.Context, id string, from domain.ImportStatus, report ImportReport) (bool, error)
	Cancel(ctx context.Context, id, reason string) (bool, error)
	Lock(ctx context.Context, id string) (context.Context, func(), error)
}

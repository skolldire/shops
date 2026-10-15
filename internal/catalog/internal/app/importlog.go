package app

import (
	"context"
	"errors"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

var (
	ErrImportNotFound  = errors.New("catalog: import not found")
	ErrTooManyPreviews = errors.New("catalog: too many import previews are waiting for a confirmation")
)

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
	Lock(ctx context.Context, id string) (func(), error)
}

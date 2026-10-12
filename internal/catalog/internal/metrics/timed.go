package metrics

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/skolldire/shops/internal/catalog/internal/core"
)

type FileImporter interface {
	Import(ctx context.Context, r io.Reader, fileName string) (core.ImportReport, error)
}

type TimedImporter struct {
	next        FileImporter
	instruments Instruments
}

func NewTimedImporter(next FileImporter, instruments Instruments) (*TimedImporter, error) {
	switch {
	case next == nil:
		return nil, errors.New("catalog: timed importer requires an importer")
	case instruments == nil:
		return nil, errors.New("catalog: timed importer requires instruments")
	}
	return &TimedImporter{next: next, instruments: instruments}, nil
}

func (t *TimedImporter) Import(ctx context.Context, r io.Reader, fileName string) (core.ImportReport, error) {
	start := time.Now()
	defer func() {
		t.instruments.Histogram(context.WithoutCancel(ctx), importDuration, time.Since(start).Seconds())
	}()
	return t.next.Import(ctx, r, fileName)
}

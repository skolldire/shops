package app

import (
	"context"
	"errors"
	"strings"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

type PreviewResult struct {
	Report   ImportReport
	New      int
	Existing int
	Lines    []domain.PreviewLine
}

type PreviewView struct {
	Record    ImportRecord
	Current   PreviewResult
	Duplicate *domain.ImportStamp
}

type Previews struct {
	importer *Importer
	log      ImportLog
	targets  ImportTargets
	reader   RowReader
}

func NewPreviews(importer *Importer, log ImportLog, targets ImportTargets, reader RowReader) (*Previews, error) {
	switch {
	case importer == nil:
		return nil, errors.New("catalog: previews require an importer")
	case log == nil:
		return nil, errors.New("catalog: previews require an import log")
	case targets == nil:
		return nil, errors.New("catalog: previews require the products of an import")
	case reader == nil:
		return nil, errors.New("catalog: previews require a row reader")
	}
	return &Previews{importer: importer, log: log, targets: targets, reader: reader}, nil
}

func (p *Previews) Create(ctx context.Context, req ImportRequest) (string, error) {
	if req.Key != "" && !idPattern.MatchString(req.Key) {
		return "", ErrInvalidIdempotencyKey
	}
	result, err := p.compute(ctx, req)
	if err != nil {
		return "", err
	}
	rec, created, err := p.log.CreatePreview(ctx, ImportRecord{
		Key: strings.ToLower(req.Key), Mode: req.Mode, Status: domain.ImportPreview,
		File: req.File, Content: req.Content, Preview: result.Lines,
	})
	if err != nil {
		return "", err
	}
	if !created {
		if err := sameImport(rec, req); err != nil {
			return "", err
		}
	}
	return rec.ID, nil
}

func (p *Previews) Get(ctx context.Context, id string) (PreviewView, error) {
	rec, err := p.record(ctx, id)
	if err != nil {
		return PreviewView{}, err
	}
	view := PreviewView{Record: rec}
	if rec.Status != domain.ImportPreview {
		return view, nil
	}
	rows, err := p.reader.Rows(rec.Content)
	if err != nil {
		return PreviewView{}, err
	}
	req := ImportRequest{File: rec.File, Content: rec.Content, Rows: rows, Mode: rec.Mode, Key: rec.Key}
	if view.Current, err = p.compute(ctx, req); err != nil {
		return PreviewView{}, err
	}
	if rec.Mode == domain.ImportAdd {
		previous, found, err := p.log.LastAppliedAdd(ctx, rec.File.SHA256, rec.ID)
		if err != nil {
			return PreviewView{}, err
		}
		if found {
			view.Duplicate = &previous
		}
	}
	return view, nil
}

func (p *Previews) record(ctx context.Context, id string) (ImportRecord, error) {
	if !idPattern.MatchString(id) {
		return ImportRecord{}, ErrImportNotFound
	}
	rec, err := p.log.Get(ctx, strings.ToLower(id))
	if err != nil {
		return ImportRecord{}, err
	}
	if rec.Status == domain.ImportPreview && rec.Expired {
		if _, err := p.log.Cancel(ctx, rec.ID, domain.CancelledExpired); err != nil {
			return ImportRecord{}, err
		}
		return p.log.Get(ctx, rec.ID)
	}
	return rec, nil
}

func (p *Previews) compute(ctx context.Context, req ImportRequest) (PreviewResult, error) {
	report, valid := p.importer.prepare(req)
	skus := make([]string, len(valid))
	for i, row := range valid {
		skus[i] = row.sku
	}
	targets, err := p.targets.ImportTargets(ctx, skus)
	if err != nil {
		return PreviewResult{}, err
	}
	result := PreviewResult{Lines: []domain.PreviewLine{}}
	for _, row := range valid {
		target, found := targets[row.sku]
		after := row.product.Stock
		if req.Mode == domain.ImportAdd {
			after += target.Stock
		}
		switch {
		case !found:
			result.New++
			continue
		case target.Deleted:
			count(&report, row, OutcomeDeleted)
			continue
		case req.Mode == domain.ImportReplace && after < target.Reserved:
			count(&report, row, OutcomeBelowReserved)
		default:
			result.Existing++
		}
		result.Lines = append(result.Lines, domain.PreviewLine{SKU: row.sku, Before: target.Stock, After: after, Reserved: target.Reserved})
	}
	sortErrors(&report)
	result.Report = report
	return result, nil
}

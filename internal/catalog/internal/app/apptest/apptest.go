package apptest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

type Snapshotter interface {
	Snapshot() (restore func())
}

type Tx struct {
	Members []Snapshotter
}

type txKey struct{}

func (t Tx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if ctx.Value(txKey{}) != nil {
		return fn(ctx)
	}
	restores := make([]func(), len(t.Members))
	for i, m := range t.Members {
		restores[i] = m.Snapshot()
	}
	err := fn(context.WithValue(ctx, txKey{}, true))
	if err != nil {
		for _, restore := range slices.Backward(restores) {
			restore()
		}
	}
	return err
}

type Log struct {
	Busy     bool
	held     bool
	mu       sync.Mutex
	records  map[string]app.ImportRecord
	order    []string
	Finished []app.ImportReport
}

func NewLog() *Log {
	return &Log{records: map[string]app.ImportRecord{}}
}

func (l *Log) Create(_ context.Context, rec app.ImportRecord) (app.ImportRecord, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec.Key != "" {
		for _, id := range l.order {
			if existing := l.records[id]; existing.Key == rec.Key {
				return existing, false, nil
			}
		}
	}
	rec.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(l.order)+1)
	rec.Report = app.ImportReport{ID: rec.ID, Mode: rec.Mode, File: rec.File, Errors: []app.RowError{}, Changes: []domain.ImportChange{}}
	l.records[rec.ID] = rec
	l.order = append(l.order, rec.ID)
	return rec, true, nil
}

func (l *Log) Get(_ context.Context, id string) (app.ImportRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.records[id]
	if !ok {
		return app.ImportRecord{}, app.ErrImportNotFound
	}
	return rec, nil
}

func (l *Log) LastAppliedAdd(_ context.Context, hash, exceptID string) (domain.ImportStamp, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range slices.Backward(l.order) {
		rec := l.records[id]
		if id != exceptID && rec.Mode == domain.ImportAdd && rec.Status == domain.ImportApplied && rec.File.SHA256 == hash {
			return rec.Stamp, true, nil
		}
	}
	return domain.ImportStamp{}, false, nil
}

func (l *Log) Finish(_ context.Context, id string, from domain.ImportStatus, report app.ImportReport) (bool, error) {
	ok, err := l.move(id, from, func(rec *app.ImportRecord) {
		rec.Status, rec.Report = domain.ImportApplied, report
	})
	if ok {
		l.mu.Lock()
		l.Finished = append(l.Finished, report)
		l.mu.Unlock()
	}
	return ok, err
}

func (l *Log) LockImports(ctx context.Context) (context.Context, func(), bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Busy || l.held {
		return ctx, func() {}, false, nil
	}
	l.held = true
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.held = false
		})
	}, true, nil
}

func (l *Log) Snapshot() func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	records, order, finished := maps.Clone(l.records), slices.Clone(l.order), slices.Clone(l.Finished)
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.records, l.order, l.Finished = records, order, finished
	}
}

func (l *Log) ByKey(_ context.Context, key string) (app.ImportRecord, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range l.order {
		if rec := l.records[id]; rec.Key == key {
			return rec, true, nil
		}
	}
	return app.ImportRecord{}, false, nil
}

func (l *Log) Set(rec app.ImportRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.records[rec.ID]; !ok {
		l.order = append(l.order, rec.ID)
	}
	l.records[rec.ID] = rec
}

func (l *Log) move(id string, from domain.ImportStatus, change func(*app.ImportRecord)) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.records[id]
	if !ok || rec.Status != from {
		return false, nil
	}
	change(&rec)
	l.records[id] = rec
	return true, nil
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/logger"
)

type seeder interface {
	Seed(ctx context.Context, file io.Reader) (catalog.ImportReport, bool, error)
}

func seedDemo(ctx context.Context, log logger.Service, s seeder, path string) error {
	file, err := os.DirFS(filepath.Dir(path)).Open(filepath.Base(path))
	if err != nil {
		return fmt.Errorf("demo seed: open seed file: %w", withoutPath(err))
	}
	defer func() { _ = file.Close() }()

	report, seeded, err := s.Seed(ctx, file)
	if err != nil {
		if report.Records > 0 {
			log.Warn(ctx, "demo seed interrupted, part of the file was applied", seedSummary(report))
		}
		return fmt.Errorf("demo seed: %w", withoutPath(err))
	}
	if !seeded {
		log.Info(ctx, "demo seed skipped, the catalog already has products", nil)
		return nil
	}
	log.Info(ctx, "demo seed imported", seedSummary(report))
	return nil
}

func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

func seedSummary(report catalog.ImportReport) map[string]any {
	return map[string]any{
		"sha256":    report.File.SHA256,
		"records":   report.Records,
		"created":   report.Created,
		"updated":   report.Updated,
		"unchanged": report.Unchanged,
		"rejected":  report.Rejected,
		"skipped":   report.Skipped,
	}
}

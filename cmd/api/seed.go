package main

import (
	"context"
	"fmt"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/logger"
)

type seeder interface {
	Seed(ctx context.Context, path string) (catalog.ImportReport, bool, error)
}

func seedDemo(ctx context.Context, log logger.Service, s seeder, path string) error {
	report, seeded, err := s.Seed(ctx, path)
	if err != nil {
		if report.Records > 0 {
			log.Warn(ctx, "demo seed interrupted, part of the file was applied", seedSummary(report))
		}
		return fmt.Errorf("demo seed: %w", err)
	}
	if !seeded {
		log.Info(ctx, "demo seed skipped, the catalog already has products", nil)
		return nil
	}
	log.Info(ctx, "demo seed imported", seedSummary(report))
	return nil
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

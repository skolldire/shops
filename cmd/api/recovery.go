package main

import (
	"context"
	"fmt"

	"github.com/skolldire/shops/internal/ordering"
	"github.com/skolldire/shops/internal/platform/logger"
)

type recoverer interface {
	RecoverPending(ctx context.Context) (ordering.RecoveryReport, error)
}

func recoverPurchases(ctx context.Context, log logger.Service, r recoverer) error {
	report, err := r.RecoverPending(ctx)
	if err != nil {
		return fmt.Errorf("purchase recovery: %w", err)
	}
	log.Info(ctx, "pending purchases recovered", map[string]any{
		"confirmed": report.Confirmed, "declined": report.Declined, "still_pending": len(report.StillPending),
	})
	for _, p := range report.StillPending {
		log.Warn(ctx, "purchase still pending after recovery", map[string]any{"purchase_id": p.ID, "cause": fmt.Sprint(p.Cause)})
	}
	return nil
}

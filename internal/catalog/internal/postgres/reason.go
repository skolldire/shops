package postgres

import (
	"context"
	"fmt"

	"github.com/skolldire/shops/internal/platform/database"
)

const (
	reasonInitialStock       = "initial_stock"
	reasonManualAdjustment   = "manual_adjustment"
	reasonImport             = "import"
	reasonSale               = "sale"
	reasonReservation        = "reservation"
	reasonReservationRelease = "reservation_release"
	reasonSaleReversal       = "sale_reversal"
)

func setStockReason(ctx context.Context, q database.Querier, reason, reference string) error {
	_, err := q.Exec(ctx,
		`SELECT set_config('shop.stock_reason', $1, true), set_config('shop.stock_reference', $2, true)`,
		reason, reference)
	if err != nil {
		return fmt.Errorf("catalog: set stock reason: %w", err)
	}
	return nil
}

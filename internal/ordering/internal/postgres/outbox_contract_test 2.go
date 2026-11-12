package postgres

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

const otherProductID = "7d3e1f2a-4b5c-4d6e-8f7a-9b0c1d2e3f4a"

func referenceEvents() map[string]domain.PurchaseEvent {
	price := decimal.RequireFromString("19.99")
	return map[string]domain.PurchaseEvent{
		"purchase_confirmed_v1.json": {Type: domain.EventPurchaseConfirmed, AggregateID: purchaseID, Version: domain.PurchaseEventVersion,
			Total: decimal.RequireFromString("59.97"), Currency: "USD", PaymentRef: "fake_ref_0001",
			Items: []domain.EventItem{
				{ProductID: productID, SKU: "TENT-1", Quantity: 2, UnitPrice: price},
				{ProductID: otherProductID, SKU: "LAMP-1", Quantity: 1, UnitPrice: price},
			}},
		"purchase_failed_v1.json": {Type: domain.EventPurchaseFailed, AggregateID: purchaseID, Version: domain.PurchaseEventVersion, Reason: "card_declined",
			Items: []domain.EventItem{{ProductID: productID, Quantity: 2}, {ProductID: otherProductID, Quantity: 1}}},
	}
}

func TestThePayloadOfEverySupportedEventMatchesItsReference(t *testing.T) {
	events := referenceEvents()
	require.Len(t, events, len(domain.SupportedEvents()), "every supported type and version has a reference file")
	for file, event := range events {
		t.Run(file, func(t *testing.T) {
			reference, err := os.ReadFile(filepath.Join("testdata", file))
			require.NoError(t, err)
			raw, err := encodePayload(event)
			require.NoError(t, err)
			require.Equal(t, string(reference), string(raw)+"\n",
				"the payload of a published version never changes: a new structure needs a new version and its approval")

			decoded, err := decodePayload(event.Type, event.Version, purchaseID, reference)
			require.NoError(t, err)
			require.Equal(t, event.Type, decoded.Type)
			require.Equal(t, event.AggregateID, decoded.AggregateID)
			require.True(t, event.Total.Equal(decoded.Total))
			require.Equal(t, event.Currency, decoded.Currency)
			require.Equal(t, event.PaymentRef, decoded.PaymentRef)
			require.Equal(t, event.Reason, decoded.Reason)
			require.Len(t, decoded.Items, len(event.Items))
			for i, it := range decoded.Items {
				require.Equal(t, event.Items[i].ProductID, it.ProductID)
				require.Equal(t, event.Items[i].SKU, it.SKU)
				require.Equal(t, event.Items[i].Quantity, it.Quantity)
				require.True(t, event.Items[i].UnitPrice.Equal(it.UnitPrice))
			}
		})
	}
	covered := map[domain.EventKind]bool{}
	for _, e := range events {
		covered[domain.EventKind{Type: e.Type, Version: e.Version}] = true
	}
	for _, kind := range domain.SupportedEvents() {
		require.True(t, covered[kind], "%s v%d has a reference", kind.Type, kind.Version)
	}
}

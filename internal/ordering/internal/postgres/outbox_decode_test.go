package postgres

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

const (
	purchaseID = "6f1c9a52-3d2e-4f7b-9a10-2b8c4d5e6f70"
	productID  = "0b6f2a1e-8c4d-4e3f-9a2b-1c0d9e8f7a6b"
	secret     = "SYNTHETIC-SECRET-7731"
)

func confirmedV1() domain.PurchaseEvent {
	return domain.PurchaseEvent{Type: domain.EventPurchaseConfirmed, AggregateID: purchaseID, Version: domain.PurchaseEventVersion,
		Total: decimal.RequireFromString("20.00"), Currency: "USD", PaymentRef: "ref-1",
		Items: []domain.EventItem{{ProductID: productID, SKU: "SKU-1", Quantity: 2, UnitPrice: decimal.RequireFromString("10.00")}}}
}

func TestAnEventIsDecodedByItsTypeAndVersion(t *testing.T) {
	for _, want := range []domain.PurchaseEvent{
		confirmedV1(),
		{Type: domain.EventPurchaseFailed, AggregateID: purchaseID, Version: domain.PurchaseEventVersion, Reason: "declined",
			Items: []domain.EventItem{{ProductID: productID, Quantity: 1}}},
	} {
		raw, err := encodePayload(want)
		require.NoError(t, err)
		got, err := decodePayload(want.Type, want.Version, purchaseID, raw)
		require.NoError(t, err)
		require.Equal(t, want.Type, got.Type)
		require.Equal(t, want.Version, got.Version)
		require.Equal(t, want.Items[0].Quantity, got.Items[0].Quantity)
		require.True(t, want.Total.Equal(got.Total))
	}
}

func TestAnUnsupportedTypeOrVersionIsNeverReadAsVersionOne(t *testing.T) {
	raw, err := encodePayload(confirmedV1())
	require.NoError(t, err)
	for _, key := range []eventKey{
		{kind: domain.EventPurchaseConfirmed, version: domain.PurchaseEventVersion + 1},
		{kind: domain.EventPurchaseFailed, version: 0},
		{kind: "PurchaseRefunded", version: domain.PurchaseEventVersion},
	} {
		_, err := decodePayload(key.kind, key.version, purchaseID, raw)
		require.ErrorIs(t, err, domain.ErrUnsupportedEvent, "%s v%d", key.kind, key.version)
	}
}

func TestAnUnreadablePayloadHasASafeCause(t *testing.T) {
	for name, payload := range map[string]string{
		"malformed json":  `{"purchase_id": "` + secret,
		"wrong type":      `{"purchase_id":"` + purchaseID + `","total":"1.00","items":[{"product_id":"` + productID + `","quantity":"` + secret + `","unit_price":"1.00"}]}`,
		"total":           `{"purchase_id":"` + purchaseID + `","total":"` + secret + `","items":[{"product_id":"` + productID + `","quantity":1,"unit_price":"1.00"}]}`,
		"unit price":      `{"purchase_id":"` + purchaseID + `","total":"1.00","items":[{"product_id":"` + productID + `","quantity":1,"unit_price":"` + secret + `"}]}`,
		"no items":        `{"purchase_id":"` + purchaseID + `","total":"1.00","items":[]}`,
		"no product":      `{"purchase_id":"` + purchaseID + `","total":"1.00","items":[{"quantity":1,"unit_price":"1.00"}]}`,
		"no quantity":     `{"purchase_id":"` + purchaseID + `","total":"1.00","items":[{"product_id":"` + productID + `","unit_price":"1.00"}]}`,
		"other purchase":  `{"purchase_id":"` + productID + `","total":"1.00","items":[{"product_id":"` + productID + `","quantity":1,"unit_price":"1.00"}]}`,
		"product text":    `{"purchase_id":"` + purchaseID + `","total":"1.00","items":[{"product_id":"` + secret + `","quantity":1,"unit_price":"1.00"}]}`,
		"product braces":  `{"purchase_id":"` + purchaseID + `","total":"1.00","items":[{"product_id":"{` + productID + `}","quantity":1,"unit_price":"1.00"}]}`,
		"product urn":     `{"purchase_id":"` + purchaseID + `","total":"1.00","items":[{"product_id":"urn:uuid:` + productID + `","quantity":1,"unit_price":"1.00"}]}`,
		"purchase braces": `{"purchase_id":"{` + purchaseID + `}","total":"1.00","items":[{"product_id":"` + productID + `","quantity":1,"unit_price":"1.00"}]}`,
		"purchase urn":    `{"purchase_id":"urn:uuid:` + purchaseID + `","total":"1.00","items":[{"product_id":"` + productID + `","quantity":1,"unit_price":"1.00"}]}`,
		"empty object":    `{}`,
		"null":            `null`,
	} {
		_, err := decodePayload(domain.EventPurchaseConfirmed, domain.PurchaseEventVersion, purchaseID, []byte(payload))
		require.ErrorIs(t, err, domain.ErrUnreadablePayload, name)
		require.NotContains(t, err.Error(), secret, name)
		require.NotContains(t, err.Error(), purchaseID, name)
	}
	_, err := decodePayload(domain.EventPurchaseFailed, domain.PurchaseEventVersion, purchaseID, []byte(`{"purchase_id":"`+purchaseID+`","items":[]}`))
	require.ErrorIs(t, err, domain.ErrUnreadablePayload, "a failed purchase also lists its items")
}

func TestIdentifiersAreReadWithTheDomainGrammar(t *testing.T) {
	upper := `{"purchase_id":"` + strings.ToUpper(purchaseID) + `","total":"1.00","items":[{"product_id":"` + strings.ToUpper(productID) + `","quantity":1,"unit_price":"1.00"}]}`
	e, err := decodePayload(domain.EventPurchaseConfirmed, domain.PurchaseEventVersion, purchaseID, []byte(upper))
	require.NoError(t, err, "the same ids in upper case match")
	require.Equal(t, domain.ID(purchaseID), e.AggregateID, "and are normalised")
	require.Equal(t, domain.ID(productID), e.Items[0].ProductID)

	raw, err := encodePayload(confirmedV1())
	require.NoError(t, err)
	_, err = decodePayload(domain.EventPurchaseConfirmed, domain.PurchaseEventVersion, strings.ToUpper(purchaseID), raw)
	require.NoError(t, err, "the aggregate id is compared after normalising it too")
	for name, aggregate := range map[string]string{"other": productID, "braces": "{" + purchaseID + "}", "urn": "urn:uuid:" + purchaseID} {
		_, err := decodePayload(domain.EventPurchaseConfirmed, domain.PurchaseEventVersion, aggregate, raw)
		require.ErrorIs(t, err, domain.ErrUnreadablePayload, name)
	}
}

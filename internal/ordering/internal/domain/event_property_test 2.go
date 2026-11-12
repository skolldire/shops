package domain_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

const propertySeed uint64 = 20261017

const propertyPurchases = 500

type sequence struct {
	state uint64
}

func newSequence(seed uint64) *sequence {
	return &sequence{state: seed}
}

func (s *sequence) next() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (s *sequence) below(n uint64) uint64 {
	return s.next() % n
}

func (s *sequence) intn(n int) int {
	return int(s.below(uint64(n)))
}

type generated struct {
	purchase domain.Purchase
	user     string
	token    string
	key      string
}

func randomID(rng *sequence) string {
	return fmt.Sprintf("%08x-%04x-4%03x-a%03x-%012x", rng.below(1<<32), rng.below(1<<16), rng.below(1<<12), rng.below(1<<12), rng.below(1<<48))
}

func validPurchase(t *testing.T, rng *sequence) generated {
	t.Helper()
	g := generated{user: "user-" + randomID(rng), token: "pm_" + randomID(rng)[:8], key: randomID(rng)}
	n := 1 + rng.intn(domain.MaxItems)
	inputs := make([]domain.CheckoutItemInput, n)
	lines := make([]domain.ReservedLine, n)
	for i := range n {
		price := decimal.New(1+int64(rng.below(999_999)), -2)
		qty := 1 + rng.intn(domain.MaxQuantity)
		id := randomID(rng)
		inputs[i] = domain.CheckoutItemInput{ProductID: id, Quantity: qty, ExpectedUnitPrice: &price}
		lines[i] = domain.ReservedLine{ProductID: domain.ID(id), SKU: fmt.Sprintf("SKU-%d", i), Name: "Product", Price: price, Quantity: qty}
	}
	req, err := domain.NewCheckoutRequest(g.user, domain.CheckoutInput{IdempotencyKey: g.key, Items: inputs, PaymentToken: g.token})
	require.NoError(t, err)
	p := domain.NewPendingPurchase(req)
	p.ID = domain.ID(randomID(rng))
	p.Items, err = domain.Snapshot(req, lines)
	require.NoError(t, err)
	g.purchase = p
	return g
}

func sensitive(g generated) []string {
	return []string{g.user, g.token, g.key, g.purchase.RequestHash, g.purchase.PaymentToken}
}

func requireNoSensitiveValue(t *testing.T, e domain.PurchaseEvent, g generated) {
	t.Helper()
	fields := map[string]bool{}
	for f := range reflect.TypeOf(e).Fields() {
		fields[f.Name] = true
	}
	require.Equal(t, map[string]bool{"Type": true, "AggregateID": true, "CorrelationID": true, "Version": true, "Items": true,
		"Total": true, "Currency": true, "PaymentRef": true, "Reason": true}, fields, "the event has no field for a token, user, hash or key")
	text := fmt.Sprintf("%+v", e)
	for _, value := range sensitive(g) {
		require.NotContains(t, text, value)
	}
}

func TestAConfirmedEventKeepsTheInvariantsOfAValidPurchase(t *testing.T) {
	rng := newSequence(propertySeed)
	for i := range propertyPurchases {
		g := validPurchase(t, rng)
		p := g.purchase
		require.NoError(t, p.Apply(domain.Confirmed("ref-"+randomID(rng))))
		e, err := domain.NewPurchaseEvent(p, domain.Outcome{Status: domain.StatusConfirmed, PaymentRef: p.PaymentRef})
		require.NoError(t, err, "seed %d, purchase %d", propertySeed, i)

		sum := decimal.Zero
		for j, it := range e.Items {
			want := p.Items[j]
			require.Equal(t, domain.EventItem{ProductID: want.ProductID, SKU: want.SKU, Quantity: want.Quantity, UnitPrice: want.UnitPrice}, it)
			sum = sum.Add(it.UnitPrice.Mul(decimal.NewFromInt(int64(it.Quantity))))
		}
		require.Len(t, e.Items, len(p.Items))
		require.True(t, e.Total.Equal(sum), "seed %d, purchase %d: the total is the exact sum of its lines", propertySeed, i)
		require.Equal(t, p.Currency, e.Currency)
		require.Equal(t, domain.EventPurchaseConfirmed, e.Type)
		require.Equal(t, p.ID, e.AggregateID)
		require.Equal(t, "purchase:"+string(p.ID), e.CorrelationID)
		require.Equal(t, domain.PurchaseEventVersion, e.Version)
		require.Equal(t, p.PaymentRef, e.PaymentRef)
		require.Empty(t, e.Reason)
		requireNoSensitiveValue(t, e, g)
	}
}

func TestAFailedEventKeepsTheIdentityReasonAndItemsOfAValidPurchase(t *testing.T) {
	seed := propertySeed + 1
	rng := newSequence(seed)
	for i := range propertyPurchases {
		g := validPurchase(t, rng)
		p := g.purchase
		reason := []string{"card_declined", "insufficient_funds", "payment_not_completed"}[rng.intn(3)]
		require.NoError(t, p.Apply(domain.Failed(reason)))
		e, err := domain.NewPurchaseEvent(p, domain.Outcome{Status: domain.StatusFailed, FailureReason: reason})
		require.NoError(t, err, "seed %d, purchase %d", seed, i)

		require.Equal(t, domain.EventPurchaseFailed, e.Type)
		require.Equal(t, p.ID, e.AggregateID)
		require.Equal(t, "purchase:"+string(p.ID), e.CorrelationID)
		require.Equal(t, reason, e.Reason)
		require.Len(t, e.Items, len(p.Items))
		for j, it := range e.Items {
			require.Equal(t, p.Items[j].ProductID, it.ProductID)
			require.Equal(t, p.Items[j].Quantity, it.Quantity)
		}
		require.Empty(t, e.PaymentRef, "a failed purchase carries no payment reference")
		requireNoSensitiveValue(t, e, g)
	}
}

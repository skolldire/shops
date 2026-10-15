package app_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type state struct {
	purchases map[domain.ID]domain.Purchase
	stock     map[domain.ID]app.Product
	deleted   map[domain.ID]bool
	carts     map[string][]domain.CartItem
	reserved  map[domain.ID]int
	releases  int
	reserves  int
	commits   int
}

func (s state) clone() state {
	c := state{
		purchases: maps.Clone(s.purchases), stock: maps.Clone(s.stock), deleted: maps.Clone(s.deleted),
		carts: map[string][]domain.CartItem{}, reserved: maps.Clone(s.reserved),
		releases: s.releases, reserves: s.reserves, commits: s.commits,
	}
	for k, v := range s.purchases {
		v.Items = slices.Clone(v.Items)
		c.purchases[k] = v
	}
	for k, v := range s.carts {
		c.carts[k] = slices.Clone(v)
	}
	return c
}

type world struct {
	mu         sync.Mutex
	state      state
	nextID     int
	clock      time.Time
	inTx       bool
	txErr      error
	reserveErr error
	finErr     error
	listErr    error
}

type txKey struct{}

func newWorld() *world {
	return &world{
		state: state{
			purchases: map[domain.ID]domain.Purchase{}, stock: map[domain.ID]app.Product{},
			deleted: map[domain.ID]bool{}, carts: map[string][]domain.CartItem{}, reserved: map[domain.ID]int{},
		},
		clock: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
}

func (w *world) product(id, sku, price string, stock int) domain.ID {
	pid := domain.ID(id)
	w.state.stock[pid] = app.Product{ID: pid, SKU: sku, Name: "Product " + sku, Price: decimal.RequireFromString(price), Available: stock}
	return pid
}

func (w *world) stockOf(id domain.ID) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state.stock[id].Available
}

func (w *world) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.state.purchases)
}

func (w *world) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if ctx.Value(txKey{}) != nil {
		return fn(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	saved := w.state.clone()
	w.inTx = true
	err := fn(context.WithValue(ctx, txKey{}, true))
	w.inTx = false
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		w.state = saved
		if w.txErr != nil {
			return w.txErr
		}
		return err
	}
	return nil
}

func (w *world) locked(ctx context.Context, fn func()) {
	if ctx.Value(txKey{}) != nil {
		fn()
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	fn()
}

func (w *world) Insert(ctx context.Context, p domain.Purchase) (domain.Purchase, bool, error) {
	var (
		out      domain.Purchase
		inserted bool
	)
	w.locked(ctx, func() {
		for _, existing := range w.state.purchases {
			if existing.UserID == p.UserID && existing.IdempotencyKey == p.IdempotencyKey {
				return
			}
		}
		w.nextID++
		p.ID = domain.ID(fmt.Sprintf("00000000-0000-4000-8000-%012d", w.nextID))
		p.CreatedAt = w.clock.Add(time.Duration(w.nextID) * time.Second)
		w.state.purchases[p.ID] = p
		out, inserted = p, true
	})
	return out, inserted, nil
}

func (w *world) AddItems(ctx context.Context, id domain.ID, items []domain.PurchaseItem) error {
	w.locked(ctx, func() {
		p := w.state.purchases[id]
		p.Items = slices.Clone(items)
		w.state.purchases[id] = p
	})
	return nil
}

func (w *world) ByKey(ctx context.Context, userID string, key domain.ID) (domain.Purchase, error) {
	var (
		out   domain.Purchase
		found bool
	)
	w.locked(ctx, func() {
		for _, p := range w.state.purchases {
			if p.UserID == userID && p.IdempotencyKey == key {
				out, found = p, true
			}
		}
	})
	if !found {
		return domain.Purchase{}, app.ErrPurchaseNotFound
	}
	return out, nil
}

func (w *world) Get(ctx context.Context, userID string, id domain.ID) (domain.Purchase, error) {
	var (
		p  domain.Purchase
		ok bool
	)
	w.locked(ctx, func() { p, ok = w.state.purchases[id] })
	if !ok || p.UserID != userID {
		return domain.Purchase{}, app.ErrPurchaseNotFound
	}
	return p, nil
}

func (w *world) List(ctx context.Context, userID string, page domain.Page) ([]domain.Purchase, int, error) {
	var all []domain.Purchase
	w.locked(ctx, func() {
		for _, p := range w.state.purchases {
			if p.UserID == userID {
				all = append(all, p)
			}
		}
	})
	slices.SortFunc(all, func(a, b domain.Purchase) int { return b.CreatedAt.Compare(a.CreatedAt) })
	from := min(page.Offset(), len(all))
	to := min(from+page.PageSize, len(all))
	return all[from:to], len(all), nil
}

func (w *world) Pending(ctx context.Context) ([]domain.Purchase, error) {
	if w.listErr != nil {
		return nil, w.listErr
	}
	var out []domain.Purchase
	w.locked(ctx, func() {
		for _, p := range w.state.purchases {
			if p.Status == domain.StatusPending {
				out = append(out, p)
			}
		}
	})
	slices.SortFunc(out, func(a, b domain.Purchase) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out, nil
}

func (w *world) Finish(ctx context.Context, id domain.ID, o domain.Outcome) (bool, error) {
	if w.finErr != nil {
		return false, w.finErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	won := false
	w.locked(ctx, func() {
		p, ok := w.state.purchases[id]
		if !ok || p.Status != domain.StatusPending {
			return
		}
		p.Status, p.PaymentRef, p.FailureReason = o.Status, o.PaymentRef, o.FailureReason
		w.state.purchases[id] = p
		won = true
	})
	return won, nil
}

func (w *world) Reserve(ctx context.Context, items []domain.Quantity, _ string) ([]domain.ReservedLine, error) {
	if w.reserveErr != nil {
		return nil, w.reserveErr
	}
	var (
		lines []domain.ReservedLine
		err   error
	)
	w.locked(ctx, func() {
		for _, it := range items {
			p, ok := w.state.stock[it.ProductID]
			switch {
			case !ok || w.state.deleted[it.ProductID]:
				err = &app.UnavailableProductError{ProductID: it.ProductID}
				return
			case p.Available < it.Quantity:
				err = &app.InsufficientStockError{ProductID: it.ProductID, Available: p.Available}
				return
			}
			p.Available -= it.Quantity
			w.state.stock[it.ProductID] = p
			w.state.reserved[it.ProductID] += it.Quantity
			lines = append(lines, domain.ReservedLine{ProductID: p.ID, SKU: p.SKU, Name: p.Name, Price: p.Price, Quantity: it.Quantity})
		}
		w.state.reserves++
	})
	return lines, err
}

func (w *world) Release(ctx context.Context, items []domain.Quantity, _ string) error {
	return w.settle(ctx, items, func(id domain.ID, q int) {
		p := w.state.stock[id]
		p.Available += q
		w.state.stock[id] = p
	}, &w.state.releases)
}

func (w *world) Commit(ctx context.Context, items []domain.Quantity, _ string) error {
	return w.settle(ctx, items, func(domain.ID, int) {}, &w.state.commits)
}

func (w *world) settle(ctx context.Context, items []domain.Quantity, apply func(domain.ID, int), counter *int) error {
	var err error
	w.locked(ctx, func() {
		for _, it := range items {
			if w.state.reserved[it.ProductID] < it.Quantity {
				err = fmt.Errorf("product %s has fewer than %d reserved units", it.ProductID, it.Quantity)
				return
			}
		}
		for _, it := range items {
			w.state.reserved[it.ProductID] -= it.Quantity
			apply(it.ProductID, it.Quantity)
		}
		*counter++
	})
	return err
}

func (w *world) Product(ctx context.Context, id domain.ID) (app.Product, error) {
	var (
		p  app.Product
		ok bool
	)
	w.locked(ctx, func() { p, ok = w.state.stock[id]; ok = ok && !w.state.deleted[id] })
	if !ok {
		return app.Product{}, &app.UnavailableProductError{ProductID: id}
	}
	return p, nil
}

func (w *world) Lines(ctx context.Context, userID string) ([]domain.CartItem, error) {
	var out []domain.CartItem
	w.locked(ctx, func() { out = slices.Clone(w.state.carts[userID]) })
	return out, nil
}

func (w *world) Add(ctx context.Context, userID string, item domain.CartItem, maxLines int) error {
	var err error
	w.locked(ctx, func() {
		lines := w.state.carts[userID]
		for i, l := range lines {
			if l.ProductID == item.ProductID {
				lines[i].Quantity = min(l.Quantity+item.Quantity, domain.MaxQuantity)
				return
			}
		}
		if len(lines) >= maxLines {
			err = app.ErrCartFull
			return
		}
		w.state.carts[userID] = append(lines, item)
	})
	return err
}

func (w *world) update(ctx context.Context, userID string, id domain.ID, fn func(*domain.CartItem)) {
	w.locked(ctx, func() {
		for i := range w.state.carts[userID] {
			if w.state.carts[userID][i].ProductID == id {
				fn(&w.state.carts[userID][i])
			}
		}
	})
}

func (w *world) SetQuantity(ctx context.Context, userID string, id domain.ID, quantity int) error {
	w.update(ctx, userID, id, func(l *domain.CartItem) { l.Quantity = quantity })
	return nil
}

func (w *world) SetPriceSeen(ctx context.Context, userID string, id domain.ID, price decimal.Decimal) error {
	w.update(ctx, userID, id, func(l *domain.CartItem) { l.UnitPriceSeen = price })
	return nil
}

func (w *world) Remove(ctx context.Context, userID string, id domain.ID) error {
	w.locked(ctx, func() {
		w.state.carts[userID] = slices.DeleteFunc(w.state.carts[userID], func(l domain.CartItem) bool { return l.ProductID == id })
	})
	return nil
}

func (w *world) RemovePurchased(ctx context.Context, userID string, items []domain.PurchaseItem) error {
	w.locked(ctx, func() {
		w.state.carts[userID] = slices.DeleteFunc(w.state.carts[userID], func(l domain.CartItem) bool {
			return slices.ContainsFunc(items, func(it domain.PurchaseItem) bool {
				return it.ProductID == l.ProductID && it.Quantity == l.Quantity && it.UnitPrice.Equal(l.UnitPriceSeen)
			})
		})
	})
	return nil
}

type mode int

const (
	approve mode = iota
	decline
	unknownAfterCharge
	unknownBeforeCharge
	unavailable
	blockUntilDone
)

type provider struct {
	mu        sync.Mutex
	mode      mode
	requests  []app.Charge
	charges   map[string]app.Charge
	results   map[string]app.ChargeResult
	onCharge  func()
	seenCtxOK []bool
}

func newProvider(m mode) *provider {
	return &provider{mode: m, charges: map[string]app.Charge{}, results: map[string]app.ChargeResult{}}
}

var errContentConflict = errors.New("provider: the reference was charged with other content")

func (p *provider) Charge(ctx context.Context, c app.Charge) (app.ChargeResult, error) {
	if p.onCharge != nil {
		p.onCharge()
	}
	p.mu.Lock()
	p.requests = append(p.requests, c)
	p.seenCtxOK = append(p.seenCtxOK, ctx.Err() == nil)
	m := p.mode
	p.mu.Unlock()
	switch m {
	case unavailable:
		return app.ChargeResult{}, app.ErrPaymentUnavailable
	case unknownBeforeCharge:
		return app.ChargeResult{}, app.ErrPaymentOutcomeUnknown
	case blockUntilDone:
		<-ctx.Done()
		return app.ChargeResult{}, fmt.Errorf("%w: %w", app.ErrPaymentOutcomeUnknown, ctx.Err())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.charges[c.Reference]; ok {
		if prev.Token != c.Token || !prev.Amount.Equal(c.Amount) || prev.Currency != c.Currency {
			return app.ChargeResult{}, errContentConflict
		}
		return p.results[c.Reference], nil
	}
	res := app.ChargeResult{Approved: true, ProviderRef: "ref_" + c.Reference}
	if m == decline || c.Token == "pm_declined" {
		res = app.ChargeResult{Reason: "card_declined"}
	}
	p.charges[c.Reference], p.results[c.Reference] = c, res
	if m == unknownAfterCharge {
		return app.ChargeResult{}, app.ErrPaymentOutcomeUnknown
	}
	return res, nil
}

func (p *provider) effective() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.charges)
}

func (p *provider) received() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *provider) set(m mode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = m
}

type event struct {
	outcome  string
	purchase *domain.Purchase
	cause    error
}

type recorder struct {
	mu       sync.Mutex
	events   []event
	recovery map[string]int
}

func (r *recorder) Checkout(_ context.Context, outcome string, p *domain.Purchase, cause error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var copied *domain.Purchase
	if p != nil {
		c := *p
		copied = &c
	}
	r.events = append(r.events, event{outcome: outcome, purchase: copied, cause: cause})
}

func (r *recorder) Recovery(_ context.Context, outcome string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recovery == nil {
		r.recovery = map[string]int{}
	}
	r.recovery[outcome] += n
}

func (r *recorder) outcomes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.outcome
	}
	return out
}

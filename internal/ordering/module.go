package ordering

import (
	"context"
	"errors"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/catalogstock"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/ordering/internal/httpapi"
	"github.com/skolldire/shops/internal/ordering/internal/observe"
	"github.com/skolldire/shops/internal/ordering/internal/paymentgw"
	"github.com/skolldire/shops/internal/ordering/internal/postgres"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type Deps struct {
	Pool     *pgxpool.Pool
	Catalog  catalogstock.Catalog
	Payments paymentgw.Provider
	Log      logger.Service
	Metrics  telemetry.Metrics
	Users    []string
	Budgets  Budgets
	Recovery RecoveryConfig
	Events   EventsConfig
}

type Module struct {
	checkout *app.Checkout
	recovery *app.Recovery
	worker   *Worker
	relay    *app.Relay
	relayer  *Worker
	queries  *app.Queries
	cart     *app.Cart
	http     *httpapi.Handler
}

type ports struct {
	tx          app.Tx
	reservation app.Tx
	created     app.PurchaseCreation
	reader      app.PurchaseReader
	finisher    app.PurchaseFinisher
	pending     app.PendingPurchases
	editor      app.CartEditor
	remover     app.PurchasedCartRemover
	events      app.EventOutbox
	queue       app.EventQueue
	inbox       app.Inbox
	sales       app.SalesProjectionStore
}

func NewModule(d Deps) (*Module, error) {
	if d.Pool == nil {
		return nil, errors.New("ordering: a pool is required")
	}
	manager, err := database.NewTxManager(d.Pool)
	if err != nil {
		return nil, err
	}
	tx, err := postgres.NewTx(manager)
	if err != nil {
		return nil, err
	}
	reservation, err := postgres.NewReservationTx(manager, d.Pool, d.Budgets.LockTimeout)
	if err != nil {
		return nil, err
	}
	outbox, err := postgres.NewOutbox(d.Pool)
	if err != nil {
		return nil, err
	}
	inbox, err := postgres.NewInbox(d.Pool)
	if err != nil {
		return nil, err
	}
	sales, err := postgres.NewSales(d.Pool)
	if err != nil {
		return nil, err
	}
	purchases, err := postgres.NewPurchases(d.Pool)
	if err != nil {
		return nil, err
	}
	carts, err := postgres.NewCarts(d.Pool, manager)
	if err != nil {
		return nil, err
	}
	return newModule(ports{
		tx: tx, reservation: reservation, created: purchases, reader: purchases, finisher: purchases, pending: purchases,
		editor: carts, remover: carts, events: outbox, queue: outbox, inbox: inbox, sales: sales,
	}, d)
}

func newModule(p ports, d Deps) (*Module, error) {
	if d.Log == nil || d.Metrics == nil {
		return nil, errors.New("ordering: a logger and metrics are required")
	}
	stock, err := catalogstock.New(d.Catalog)
	if err != nil {
		return nil, err
	}
	payments, err := paymentgw.New(d.Payments)
	if err != nil {
		return nil, err
	}
	rec, err := observe.NewCheckoutRecorder(d.Metrics, d.Log)
	if err != nil {
		return nil, err
	}
	budgets := app.Budgets{Reserve: d.Budgets.Reserve, Charge: d.Budgets.Charge, Finalize: d.Budgets.Finalize}
	settlement := app.Settlement{Tx: p.tx, Finisher: p.finisher, Stock: stock, Carts: p.remover, Events: p.events}
	checkout, err := app.NewCheckout(app.CheckoutPorts{
		Settlement: settlement, Reservation: p.reservation, Reservations: stock, Purchases: p.created, Lookup: p.reader, Payments: payments, Recorder: rec,
	}, budgets)
	if err != nil {
		return nil, err
	}
	recovery, err := app.NewRecovery(app.RecoveryPorts{Settlement: settlement, Pending: p.pending, Payments: payments, Recorder: rec}, budgets, app.RecoveryPolicy{
		Owner: uuid.NewString(), MinAge: d.Recovery.MinAge, Lease: d.Recovery.Lease, MaxPendingAge: d.Recovery.MaxPendingAge,
		DBTimeout: d.Recovery.DBTimeout, Batch: d.Recovery.BatchSize, StuckAfter: d.Recovery.StuckAfterAttempts,
		Backoff: backoff(d.Recovery.BaseBackoff, d.Recovery.MaxBackoff),
	})
	if err != nil {
		return nil, err
	}
	worker, err := newTickerWorker("recovery", d.Recovery.Interval, func(ctx context.Context) error {
		_, err := recovery.RunCycle(ctx)
		return err
	}, d.Log)
	if err != nil {
		return nil, err
	}
	relay, relayer, err := newRelay(p, d)
	if err != nil {
		return nil, err
	}
	queries, err := app.NewQueries(p.reader)
	if err != nil {
		return nil, err
	}
	cart, err := app.NewCart(p.editor, stock)
	if err != nil {
		return nil, err
	}
	h, err := httpapi.New(checkout, queries, d.Users, d.Log)
	if err != nil {
		return nil, err
	}
	return &Module{checkout: checkout, recovery: recovery, worker: worker, relay: relay, relayer: relayer, queries: queries, cart: cart, http: h}, nil
}

func newRelay(p ports, d Deps) (*app.Relay, *Worker, error) {
	rec, err := observe.NewRelayRecorder(d.Metrics, d.Log)
	if err != nil {
		return nil, nil, err
	}
	projection, err := app.NewSalesProjection(p.tx, p.inbox, p.sales)
	if err != nil {
		return nil, nil, err
	}
	e := d.Events
	relay, err := app.NewRelay(p.queue, projection, rec, app.RelayPolicy{
		Owner: uuid.NewString(), Lease: e.Lease, DBTimeout: e.DBTimeout, HandlerTimeout: e.HandlerTimeout,
		Batch: e.BatchSize, StuckAfter: e.StuckAfterAttempts, Backoff: backoff(e.BaseBackoff, e.MaxBackoff),
	})
	if err != nil {
		return nil, nil, err
	}
	worker, err := newTickerWorker("relay", e.Interval, func(ctx context.Context) error {
		_, err := relay.RunCycle(ctx)
		return err
	}, d.Log)
	if err != nil {
		return nil, nil, err
	}
	return relay, worker, nil
}

func (m *Module) RelayWorker() *Worker {
	return m.relayer
}

func (m *Module) RelayOnce(ctx context.Context) (RelayReport, error) {
	report, err := m.relay.RunCycle(ctx)
	return RelayReport{Published: report.Published, Duplicates: report.Duplicates, Skipped: report.Skipped, StillPending: report.StillPending}, err
}

func EventItemBudget(dbTimeout, handlerTimeout time.Duration) time.Duration {
	return app.EventItemBudget(dbTimeout, handlerTimeout)
}

const (
	RecoveryLeaseMargin = app.LeaseMargin
	MaxCartLines        = domain.MaxCartLines
	MinLockTimeout      = app.MinLockTimeout
)

func RecoveryItemBudget(b Budgets, dbTimeout time.Duration) time.Duration {
	return app.ItemBudget(app.Budgets{Reserve: b.Reserve, Charge: b.Charge, Finalize: b.Finalize}, dbTimeout)
}

func (m *Module) RegisterRoutes(r chi.Router) {
	m.http.Routes(r)
}

func (m *Module) CheckoutCart(ctx context.Context, userID, idempotencyKey, paymentToken string) (CheckoutResult, error) {
	in, err := m.cart.CheckoutInput(ctx, userID, idempotencyKey, paymentToken)
	if err != nil {
		return CheckoutResult{}, publicError(err)
	}
	res, err := m.checkout.Checkout(ctx, userID, in)
	if err != nil {
		return CheckoutResult{}, publicError(err)
	}
	return CheckoutResult{Purchase: fromPurchase(res.Purchase), Kind: fromKind(res.Kind)}, nil
}

func fromKind(k app.ResultKind) ResultKind {
	switch k {
	case app.Replayed:
		return Replayed
	case app.InProgress:
		return InProgress
	}
	return Created
}

func (m *Module) Purchase(ctx context.Context, userID, id string) (Purchase, error) {
	p, err := m.queries.Purchase(ctx, userID, id)
	if err != nil {
		return Purchase{}, publicError(err)
	}
	return fromPurchase(p), nil
}

func (m *Module) Purchases(ctx context.Context, userID string, page, pageSize *int) (PurchasePage, error) {
	res, err := m.queries.Purchases(ctx, userID, page, pageSize)
	if err != nil {
		return PurchasePage{}, publicError(err)
	}
	out := PurchasePage{Items: make([]Purchase, len(res.Items)), Page: res.Page, PageSize: res.PageSize, Total: res.Total}
	for i, p := range res.Items {
		out.Items[i] = fromPurchase(p)
	}
	return out, nil
}

func (m *Module) Cart(ctx context.Context, userID string) (Cart, error) {
	view, err := m.cart.View(ctx, userID)
	if err != nil {
		return Cart{}, publicError(err)
	}
	out := Cart{Lines: make([]CartLine, len(view.Lines)), EstimatedTotal: view.EstimatedTotal, HasWarnings: view.HasWarnings()}
	for i, l := range view.Lines {
		out.Lines[i] = CartLine{
			ProductID: string(l.Item.ProductID), SKU: l.Product.SKU, Name: l.Product.Name, Quantity: l.Item.Quantity,
			UnitPriceSeen: l.Item.UnitPriceSeen, CurrentPrice: l.Product.Price, Available: l.Product.Available,
			Unavailable: l.Unavailable, LowStock: l.LowStock, PriceChanged: l.PriceChanged,
		}
	}
	return out, nil
}

func (m *Module) CartCount(ctx context.Context, userID string) (int, error) {
	n, err := m.cart.Count(ctx, userID)
	return n, publicError(err)
}

func (m *Module) AddToCart(ctx context.Context, userID, productID string, quantity int) error {
	return publicError(m.cart.Add(ctx, userID, productID, quantity))
}

func (m *Module) SetQuantity(ctx context.Context, userID, productID string, quantity int) error {
	return publicError(m.cart.SetQuantity(ctx, userID, productID, quantity))
}

func (m *Module) RemoveFromCart(ctx context.Context, userID, productID string) error {
	return publicError(m.cart.Remove(ctx, userID, productID))
}

func (m *Module) AcceptCurrentPrice(ctx context.Context, userID, productID string) error {
	return publicError(m.cart.AcceptCurrentPrice(ctx, userID, productID))
}

func (m *Module) RecoveryWorker() *Worker {
	return m.worker
}

func (m *Module) RecoverOnce(ctx context.Context) (RecoveryReport, error) {
	report, err := m.recovery.RunCycle(ctx)
	if err != nil {
		return RecoveryReport{}, err
	}
	out := RecoveryReport{Confirmed: report.Confirmed, Declined: report.Declined, Voided: report.Voided, Skipped: report.Skipped}
	for _, p := range report.StillPending {
		out.StillPending = append(out.StillPending, PendingPurchase{ID: string(p.PurchaseID), Cause: p.Cause})
	}
	return out, nil
}

func fromPurchase(p domain.Purchase) Purchase {
	out := Purchase{
		ID: string(p.ID), Status: Status(p.Status), Currency: p.Currency, Total: p.Total,
		PaymentRef: p.PaymentRef, FailureReason: p.FailureReason, CreatedAt: p.CreatedAt,
		Items: make([]PurchaseItem, len(p.Items)),
	}
	for i, it := range p.Items {
		out.Items[i] = PurchaseItem{
			ProductID: string(it.ProductID), SKU: it.SKU, Name: it.Name, UnitPrice: it.UnitPrice,
			Quantity: it.Quantity, LineTotal: it.LineTotal,
		}
	}
	return out
}

func publicError(err error) error {
	var rejected *app.RejectedError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rejected):
		return &RejectedError{Reason: publicReason(rejected.Reason), Errors: rejected.Errors}
	case errors.Is(err, domain.ErrInvalidIdempotencyKey):
		return ErrInvalidIdempotencyKey
	case errors.Is(err, domain.ErrInvalidID):
		return ErrInvalidID
	case errors.Is(err, app.ErrIdempotencyConflict):
		return ErrIdempotencyConflict
	case errors.Is(err, app.ErrPurchaseNotFound):
		return ErrPurchaseNotFound
	case errors.Is(err, app.ErrProductUnavailable):
		return ErrProductUnavailable
	case errors.Is(err, app.ErrCartFull):
		return ErrCartFull
	case errors.Is(err, app.ErrBusy):
		return ErrTryAgain
	}
	return err
}

func publicReason(reason error) error {
	switch {
	case errors.Is(reason, app.ErrInsufficientStock):
		return ErrInsufficientStock
	case errors.Is(reason, app.ErrPriceChanged):
		return ErrPriceChanged
	}
	return ErrProductUnavailable
}

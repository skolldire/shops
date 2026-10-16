package ordering

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/platform/logger"
)

type RecoveryWorker struct {
	recovery *app.Recovery
	interval time.Duration
	log      logger.Service
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
}

func newRecoveryWorker(recovery *app.Recovery, interval time.Duration, log logger.Service) (*RecoveryWorker, error) {
	if interval <= 0 {
		return nil, errors.New("ordering: the recovery interval must be positive")
	}
	return &RecoveryWorker{recovery: recovery, interval: interval, log: log}, nil
}

func (w *RecoveryWorker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		return
	}
	ctx, w.cancel = context.WithCancel(ctx)
	w.done = make(chan struct{})
	go w.run(ctx, w.done)
	w.log.Info(ctx, "ordering: recovery worker started", map[string]any{"component": "ordering"})
}

func (w *RecoveryWorker) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	defer w.log.Info(context.WithoutCancel(ctx), "ordering: recovery worker stopped", map[string]any{"component": "ordering"})
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if _, err := w.recovery.RunCycle(ctx); err != nil && ctx.Err() == nil {
			w.log.Error(ctx, err, map[string]any{"component": "ordering"})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *RecoveryWorker) Stop(ctx context.Context) error {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("ordering: the recovery worker did not stop in time; its purchase stays pending until its lease expires")
	}
}

func backoff(base, ceiling time.Duration) func(attempts int) time.Duration {
	return func(attempts int) time.Duration {
		wait := base
		for range min(attempts, 30) {
			if wait >= ceiling {
				break
			}
			wait *= 2
		}
		wait = min(wait, ceiling)
		half := wait / 2
		jitter, err := rand.Int(rand.Reader, big.NewInt(int64(half)+1))
		if err != nil {
			return wait
		}
		return half + time.Duration(jitter.Int64())
	}
}

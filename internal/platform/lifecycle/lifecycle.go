package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/skolldire/shops/internal/platform/logger"
)

var ErrClosed = errors.New("lifecycle: already closed")

type closer struct {
	name  string
	close func(ctx context.Context) error
}

type Lifecycle struct {
	log     logger.Service
	mu      sync.Mutex
	closers []closer
	closed  bool
}

func New(log logger.Service) (*Lifecycle, error) {
	if log == nil {
		return nil, errors.New("lifecycle: logger is required")
	}
	return &Lifecycle{log: log}, nil
}

func (l *Lifecycle) OnClose(name string, fn func(ctx context.Context) error) error {
	if fn == nil {
		return fmt.Errorf("lifecycle: closer %s is nil", name)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	l.closers = append(l.closers, closer{name: name, close: fn})
	return nil
}

func (l *Lifecycle) Close(ctx context.Context) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	closers := l.closers
	l.closers = nil
	l.mu.Unlock()

	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		c := closers[i]
		if err := c.close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("lifecycle: close %s: %w", c.name, err))
			l.log.Error(ctx, err, map[string]any{"component": c.name})
			continue
		}
		l.log.Info(ctx, "component closed", map[string]any{"component": c.name})
	}
	return errors.Join(errs...)
}

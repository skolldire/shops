package health

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/skolldire/shops/internal/platform/logger"
)

const (
	StatusUp   = "up"
	StatusDown = "down"
)

type Checker interface {
	Check(ctx context.Context) error
}

type CheckerFunc func(ctx context.Context) error

func (f CheckerFunc) Check(ctx context.Context) error { return f(ctx) }

type Config struct {
	Timeout time.Duration `mapstructure:"timeout"`
}

type Report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

type Service struct {
	log      logger.Service
	timeout  time.Duration
	mu       sync.RWMutex
	checkers map[string]Checker
}

func New(cfg Config, log logger.Service) (*Service, error) {
	if cfg.Timeout <= 0 {
		return nil, errors.New("health: timeout must be positive")
	}
	if log == nil {
		return nil, errors.New("health: logger is required")
	}
	return &Service{log: log, timeout: cfg.Timeout, checkers: map[string]Checker{}}, nil
}

func (s *Service) Register(name string, c Checker) *Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkers[name] = c
	return s
}

func (s *Service) Check(ctx context.Context) Report {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	s.mu.RLock()
	checkers := make(map[string]Checker, len(s.checkers))
	for name, c := range s.checkers {
		checkers[name] = c
	}
	s.mu.RUnlock()

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		report = Report{Status: StatusUp, Checks: make(map[string]string, len(checkers))}
	)
	for name, c := range checkers {
		wg.Go(func() {
			start := time.Now()
			err := c.Check(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				report.Checks[name] = StatusUp
				return
			}
			report.Checks[name] = StatusDown
			report.Status = StatusDown
			s.log.Warn(ctx, "health check failed", map[string]any{
				"check":      name,
				"error":      err.Error(),
				"latency_ms": time.Since(start).Milliseconds(),
			})
		})
	}
	wg.Wait()
	return report
}

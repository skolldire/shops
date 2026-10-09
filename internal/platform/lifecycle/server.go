package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

func (l *Lifecycle) Serve(ctx context.Context, shutdownTimeout time.Duration, servers ...*http.Server) error {
	listeners := make([]net.Listener, 0, len(servers))
	for _, srv := range servers {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", srv.Addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			listenErr := fmt.Errorf("lifecycle: listen %s: %w", srv.Addr, err)
			return errors.Join(listenErr, l.shutdown(ctx, shutdownTimeout, nil))
		}
		listeners = append(listeners, ln)
		l.log.Info(ctx, "server listening", map[string]any{"addr": ln.Addr().String()})
	}

	serveErr := make(chan error, len(servers))
	for i, srv := range servers {
		go func() {
			if err := srv.Serve(listeners[i]); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("lifecycle: serve %s: %w", srv.Addr, err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		l.log.Info(ctx, "shutdown requested", nil)
	case runErr = <-serveErr:
	}
	return errors.Join(runErr, l.shutdown(ctx, shutdownTimeout, servers))
}

func (l *Lifecycle) shutdown(parent context.Context, timeout time.Duration, servers []*http.Server) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()

	errs := make([]error, len(servers), len(servers)+1)
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Go(func() {
			if err := srv.Shutdown(ctx); err != nil {
				errs[i] = fmt.Errorf("lifecycle: shutdown %s: %w", srv.Addr, err)
			}
		})
	}
	wg.Wait()
	errs = append(errs, l.Close(ctx))
	err := errors.Join(errs...)
	if err == nil {
		l.log.Info(ctx, "shutdown complete", nil)
	}
	return err
}

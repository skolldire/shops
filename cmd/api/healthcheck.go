package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultHealthcheckURL = "http://127.0.0.1:8080/health/live"
	healthcheckTimeout    = 2 * time.Second
)

func runHealthcheck(args []string) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := flags.String("url", defaultHealthcheckURL, "URL probed by the healthcheck")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return probe(context.Background(), *url, healthcheckTimeout)
}

func probe(ctx context.Context, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: unexpected status %d", resp.StatusCode)
	}
	return nil
}

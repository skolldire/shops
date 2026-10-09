package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

const (
	defaultHTTPAddr    = ":8080"
	healthcheckTimeout = 2 * time.Second
)

func runHealthcheck(args []string) error {
	url, err := healthcheckURL(args, os.Getenv)
	if err != nil {
		return err
	}
	return probe(context.Background(), url, healthcheckTimeout)
}

func healthcheckURL(args []string, getenv func(string) string) (string, error) {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := flags.String("url", "", "URL probed by the healthcheck; defaults to /health/live on HTTP_ADDR")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if *url != "" {
		return *url, nil
	}

	addr := getenv("HTTP_ADDR")
	if addr == "" {
		addr = defaultHTTPAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("healthcheck: invalid HTTP_ADDR: %w", err)
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/health/live", nil
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

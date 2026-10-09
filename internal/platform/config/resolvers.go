package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrNotFound = errors.New("config: value not found")

type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

type Resolvers map[string]Resolver

type resolverFunc func(ctx context.Context, ref string) (string, error)

func (f resolverFunc) Resolve(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}

func Env(lookup func(string) (string, bool)) Resolver {
	return resolverFunc(func(_ context.Context, ref string) (string, error) {
		v, ok := lookup(ref)
		if !ok || v == "" {
			return "", ErrNotFound
		}
		return v, nil
	})
}

func File(read func(string) ([]byte, error)) Resolver {
	return resolverFunc(func(_ context.Context, ref string) (string, error) {
		b, err := read(ref)
		if err != nil {
			return "", fmt.Errorf("read file %s: %w", ref, err)
		}
		s := string(b)
		if t, ok := strings.CutSuffix(s, "\r\n"); ok {
			return t, nil
		}
		return strings.TrimSuffix(s, "\n"), nil
	})
}

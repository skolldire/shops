package config

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const envScheme = "env"

var placeholderPattern = regexp.MustCompile(`^\$\{(?:([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?|([a-z]+):([^}]+))\}$`)

func isPlaceholder(value string) bool {
	return strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}")
}

func resolve(ctx context.Context, value string, resolvers Resolvers) (string, error) {
	if !isPlaceholder(value) {
		return value, nil
	}
	m := placeholderPattern.FindStringSubmatch(value)
	if m == nil {
		return "", fmt.Errorf("invalid placeholder %s", value)
	}
	scheme, ref := envScheme, m[1]
	if m[4] != "" {
		scheme, ref = m[4], m[5]
	}
	r, ok := resolvers[scheme]
	if !ok {
		return "", fmt.Errorf("%s: unknown scheme %q", value, scheme)
	}

	v, err := r.Resolve(ctx, ref)
	switch {
	case errors.Is(err, ErrNotFound) && m[2] != "":
		return m[3], nil
	case errors.Is(err, ErrNotFound):
		return "", fmt.Errorf("%s is not set", value)
	case err != nil:
		return "", fmt.Errorf("%s: %w", value, err)
	}
	return v, nil
}

package config

import (
	"context"
	"fmt"
	"os"

	"github.com/go-viper/mapstructure/v2"
	"go.yaml.in/yaml/v3"
)

type validator interface {
	Validate() error
}

func Load[T any](ctx context.Context, path string, resolvers Resolvers) (T, error) {
	var zero, cfg T

	raw, err := os.ReadFile(path)
	if err != nil {
		return zero, fmt.Errorf("config: read %s: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return zero, fmt.Errorf("config: parse %s: %w", path, err)
	}

	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           &cfg,
		WeaklyTypedInput: true,
		ErrorUnused:      true,
		DecodeHook:       decodeHook(ctx, resolvers),
	})
	if err != nil {
		return zero, fmt.Errorf("config: build decoder: %w", err)
	}
	if err := dec.Decode(doc); err != nil {
		return zero, fmt.Errorf("config: %s: %w", path, err)
	}

	if v, ok := any(&cfg).(validator); ok {
		if err := v.Validate(); err != nil {
			return zero, fmt.Errorf("config: %s: invalid configuration: %w", path, err)
		}
	}
	return cfg, nil
}

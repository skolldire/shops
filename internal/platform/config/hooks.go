package config

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/go-viper/mapstructure/v2"

	"github.com/skolldire/shops/internal/platform/secret"
)

var (
	secretType   = reflect.TypeFor[secret.Secret]()
	durationType = reflect.TypeFor[time.Duration]()
)

func decodeHook(ctx context.Context, resolvers Resolvers) mapstructure.DecodeHookFunc {
	return mapstructure.ComposeDecodeHookFunc(
		placeholderHook(ctx, resolvers),
		rejectBareDurationHook,
		mapstructure.StringToTimeDurationHookFunc(),
	)
}

func placeholderHook(ctx context.Context, resolvers Resolvers) mapstructure.DecodeHookFuncType {
	return func(_ reflect.Type, to reflect.Type, data any) (any, error) {
		s, isString := data.(string)
		if to == secretType {
			if !isString || !isPlaceholder(s) {
				return nil, errors.New("secret values must come from a placeholder")
			}
			v, err := resolve(ctx, s, resolvers)
			if err != nil {
				return nil, err
			}
			return secret.New(v), nil
		}
		if !isString {
			return data, nil
		}
		return resolve(ctx, s, resolvers)
	}
}

func rejectBareDurationHook(from reflect.Type, to reflect.Type, data any) (any, error) {
	if to != durationType {
		return data, nil
	}
	switch from.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return nil, errors.New("a bare number is not a duration, write it with a unit such as \"10s\" or \"500ms\"")
	}
	return data, nil
}

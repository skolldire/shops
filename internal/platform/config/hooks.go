package config

import (
	"context"
	"errors"
	"reflect"

	"github.com/go-viper/mapstructure/v2"

	"github.com/skolldire/shops/internal/platform/secret"
)

var secretType = reflect.TypeFor[secret.Secret]()

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

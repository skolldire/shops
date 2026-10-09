package core

import "context"

type Repository interface {
	Create(ctx context.Context, p Product) (Product, error)
	Get(ctx context.Context, id string) (Product, error)
	Update(ctx context.Context, id string, version int, c Changes) (Product, error)
	Delete(ctx context.Context, id string, version int) error
	Search(ctx context.Context, s Search) ([]Product, int, error)
	Categories(ctx context.Context) ([]string, error)
}

package core

import (
	"context"
	"errors"
	"regexp"
)

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Service struct {
	repo Repository
}

func NewService(repo Repository) (*Service, error) {
	if repo == nil {
		return nil, errors.New("catalog: repository is required")
	}
	return &Service{repo: repo}, nil
}

func (s *Service) Create(ctx context.Context, in NewProductInput) (Product, error) {
	p, err := NewProduct(in)
	if err != nil {
		return Product{}, err
	}
	return s.repo.Create(ctx, p)
}

func (s *Service) Get(ctx context.Context, id string) (Product, error) {
	if !idPattern.MatchString(id) {
		return Product{}, ErrNotFound
	}
	return s.repo.Get(ctx, id)
}

func (s *Service) Update(ctx context.Context, id string, version int, c Changes) (Product, error) {
	if !idPattern.MatchString(id) {
		return Product{}, ErrNotFound
	}
	changes, err := ValidateChanges(c)
	if err != nil {
		return Product{}, err
	}
	if version < 1 {
		return Product{}, ErrVersionConflict
	}
	if changes.Empty() {
		current, err := s.repo.Get(ctx, id)
		if err != nil {
			return Product{}, err
		}
		if current.Version != version {
			return Product{}, ErrVersionConflict
		}
		return current, nil
	}
	return s.repo.Update(ctx, id, version, changes)
}

func (s *Service) Delete(ctx context.Context, id string, version int) error {
	if !idPattern.MatchString(id) {
		return ErrNotFound
	}
	if version < 1 {
		return ErrVersionConflict
	}
	return s.repo.Delete(ctx, id, version)
}

func (s *Service) Search(ctx context.Context, search Search) (Page, error) {
	items, total, err := s.repo.Search(ctx, search)
	if err != nil {
		return Page{}, err
	}
	return Page{Items: items, Page: search.Page, PageSize: search.PageSize, Total: total}, nil
}

func (s *Service) Categories(ctx context.Context) ([]string, error) {
	return s.repo.Categories(ctx)
}

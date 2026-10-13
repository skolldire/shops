package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

const validID = "2b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b"

type fakeRepo struct {
	calls   []string
	created domain.Product
	version int
	changes domain.Changes
	search  domain.Search
}

func (f *fakeRepo) Create(_ context.Context, p domain.Product) (domain.Product, error) {
	f.calls = append(f.calls, "create")
	f.created = p
	p.ID, p.Version = validID, 1
	return p, nil
}

func (f *fakeRepo) Get(_ context.Context, id string) (domain.Product, error) {
	f.calls = append(f.calls, "get")
	return domain.Product{ID: id, Version: 3}, nil
}

func (f *fakeRepo) Update(_ context.Context, id string, version int, c domain.Changes) (domain.Product, error) {
	f.calls = append(f.calls, "update")
	f.version, f.changes = version, c
	return domain.Product{ID: id, Version: version + 1}, nil
}

func (f *fakeRepo) Delete(context.Context, string, int) error {
	f.calls = append(f.calls, "delete")
	return nil
}

func (f *fakeRepo) Search(_ context.Context, s domain.Search) ([]domain.Product, int, error) {
	f.calls = append(f.calls, "search")
	f.search = s
	return []domain.Product{{ID: validID}}, 41, nil
}

func (f *fakeRepo) Categories(context.Context) ([]string, error) {
	f.calls = append(f.calls, "categories")
	return []string{"Audio"}, nil
}

func newService(t *testing.T) (*app.Service, *fakeRepo) {
	t.Helper()
	repo := &fakeRepo{}
	svc, err := app.NewService(repo)
	require.NoError(t, err)
	return svc, repo
}

func TestServiceCreateValidatesBeforeStoring(t *testing.T) {
	svc, repo := newService(t)

	_, err := svc.Create(t.Context(), domain.NewProductInput{})
	require.Error(t, err)
	require.Empty(t, repo.calls)

	p, err := svc.Create(t.Context(), validInput(t))
	require.NoError(t, err)
	require.Equal(t, "RS-001", repo.created.SKU)
	require.Equal(t, validID, p.ID)
}

func TestServiceRejectsMalformedIDsAsNotFound(t *testing.T) {
	svc, repo := newService(t)

	for _, id := range []string{"", "42", "not-a-uuid", validID + "x", "'; DROP TABLE products; --"} {
		_, err := svc.Get(t.Context(), id)
		require.ErrorIs(t, err, app.ErrNotFound, id)
		_, err = svc.Update(t.Context(), id, 1, domain.Changes{Name: str("x")})
		require.ErrorIs(t, err, app.ErrNotFound, id)
		require.ErrorIs(t, svc.Delete(t.Context(), id, 1), app.ErrNotFound, id)
	}
	require.Empty(t, repo.calls)
}

func TestServiceUpdate(t *testing.T) {
	svc, repo := newService(t)

	_, err := svc.Update(t.Context(), validID, 3, domain.Changes{SKU: str("X")})
	require.Equal(t, map[string]string{"sku": "immutable"}, fieldCodes(t, err))
	_, err = svc.Update(t.Context(), validID, 0, domain.Changes{Name: str("x")})
	require.ErrorIs(t, err, app.ErrVersionConflict)
	require.ErrorIs(t, svc.Delete(t.Context(), validID, 0), app.ErrVersionConflict)
	require.Empty(t, repo.calls)

	p, err := svc.Update(t.Context(), validID, 3, domain.Changes{Category: str("  Home   Audio ")})
	require.NoError(t, err)
	require.Equal(t, 3, repo.version)
	require.Equal(t, "Home Audio", *repo.changes.Category)
	require.Equal(t, 4, p.Version)
}

func TestServiceEmptyUpdateDoesNotWrite(t *testing.T) {
	svc, repo := newService(t)

	p, err := svc.Update(t.Context(), validID, 3, domain.Changes{})
	require.NoError(t, err)
	require.Equal(t, 3, p.Version)

	_, err = svc.Update(t.Context(), validID, 2, domain.Changes{})
	require.ErrorIs(t, err, app.ErrVersionConflict)
	require.Equal(t, []string{"get", "get"}, repo.calls)
}

func TestServiceSearchBuildsPage(t *testing.T) {
	svc, repo := newService(t)
	search, err := domain.NewSearch(domain.SearchInput{Page: 3, PageSize: 10})
	require.NoError(t, err)

	page, err := svc.Search(t.Context(), search)

	require.NoError(t, err)
	require.Equal(t, search, repo.search)
	require.Equal(t, app.Page{Items: []domain.Product{{ID: validID}}, Page: 3, PageSize: 10, Total: 41}, page)
}

func TestNewServiceRequiresRepository(t *testing.T) {
	_, err := app.NewService(nil)
	require.EqualError(t, err, "catalog: repository is required")
}

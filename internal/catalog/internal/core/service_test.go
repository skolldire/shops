package core_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/core"
)

const validID = "2b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b"

type fakeRepo struct {
	calls   []string
	created core.Product
	version int
	changes core.Changes
	search  core.Search
}

func (f *fakeRepo) Create(_ context.Context, p core.Product) (core.Product, error) {
	f.calls = append(f.calls, "create")
	f.created = p
	p.ID, p.Version = validID, 1
	return p, nil
}

func (f *fakeRepo) Get(_ context.Context, id string) (core.Product, error) {
	f.calls = append(f.calls, "get")
	return core.Product{ID: id, Version: 3}, nil
}

func (f *fakeRepo) Update(_ context.Context, id string, version int, c core.Changes) (core.Product, error) {
	f.calls = append(f.calls, "update")
	f.version, f.changes = version, c
	return core.Product{ID: id, Version: version + 1}, nil
}

func (f *fakeRepo) Delete(context.Context, string, int) error {
	f.calls = append(f.calls, "delete")
	return nil
}

func (f *fakeRepo) Search(_ context.Context, s core.Search) ([]core.Product, int, error) {
	f.calls = append(f.calls, "search")
	f.search = s
	return []core.Product{{ID: validID}}, 41, nil
}

func (f *fakeRepo) Categories(context.Context) ([]string, error) {
	f.calls = append(f.calls, "categories")
	return []string{"Audio"}, nil
}

func newService(t *testing.T) (*core.Service, *fakeRepo) {
	t.Helper()
	repo := &fakeRepo{}
	svc, err := core.NewService(repo)
	require.NoError(t, err)
	return svc, repo
}

func TestServiceCreateValidatesBeforeStoring(t *testing.T) {
	svc, repo := newService(t)

	_, err := svc.Create(t.Context(), core.NewProductInput{})
	require.Error(t, err)
	require.Empty(t, repo.calls)

	p, err := svc.Create(t.Context(), validInput())
	require.NoError(t, err)
	require.Equal(t, "RS-001", repo.created.SKU)
	require.Equal(t, validID, p.ID)
}

func TestServiceRejectsMalformedIDsAsNotFound(t *testing.T) {
	svc, repo := newService(t)

	for _, id := range []string{"", "42", "not-a-uuid", validID + "x", "'; DROP TABLE products; --"} {
		_, err := svc.Get(t.Context(), id)
		require.ErrorIs(t, err, core.ErrNotFound, id)
		_, err = svc.Update(t.Context(), id, 1, core.Changes{Name: str("x")})
		require.ErrorIs(t, err, core.ErrNotFound, id)
		require.ErrorIs(t, svc.Delete(t.Context(), id, 1), core.ErrNotFound, id)
	}
	require.Empty(t, repo.calls)
}

func TestServiceUpdate(t *testing.T) {
	svc, repo := newService(t)

	_, err := svc.Update(t.Context(), validID, 3, core.Changes{SKU: str("X")})
	require.Equal(t, map[string]string{"sku": "immutable"}, fieldCodes(t, err))
	_, err = svc.Update(t.Context(), validID, 0, core.Changes{Name: str("x")})
	require.ErrorIs(t, err, core.ErrVersionConflict)
	require.ErrorIs(t, svc.Delete(t.Context(), validID, 0), core.ErrVersionConflict)
	require.Empty(t, repo.calls)

	p, err := svc.Update(t.Context(), validID, 3, core.Changes{Category: str("  Home   Audio ")})
	require.NoError(t, err)
	require.Equal(t, 3, repo.version)
	require.Equal(t, "Home Audio", *repo.changes.Category)
	require.Equal(t, 4, p.Version)
}

func TestServiceEmptyUpdateDoesNotWrite(t *testing.T) {
	svc, repo := newService(t)

	p, err := svc.Update(t.Context(), validID, 3, core.Changes{})
	require.NoError(t, err)
	require.Equal(t, 3, p.Version)

	_, err = svc.Update(t.Context(), validID, 2, core.Changes{})
	require.ErrorIs(t, err, core.ErrVersionConflict)
	require.Equal(t, []string{"get", "get"}, repo.calls)
}

func TestServiceSearchBuildsPage(t *testing.T) {
	svc, repo := newService(t)
	search, err := core.NewSearch(core.SearchInput{Page: 3, PageSize: 10})
	require.NoError(t, err)

	page, err := svc.Search(t.Context(), search)

	require.NoError(t, err)
	require.Equal(t, search, repo.search)
	require.Equal(t, core.Page{Items: []core.Product{{ID: validID}}, Page: 3, PageSize: 10, Total: 41}, page)
}

func TestNewServiceRequiresRepository(t *testing.T) {
	_, err := core.NewService(nil)
	require.EqualError(t, err, "catalog: repository is required")
}

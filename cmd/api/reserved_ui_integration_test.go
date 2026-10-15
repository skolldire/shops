//go:build integration

package main

import (
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var formVersion = regexp.MustCompile(`name="version" value="(\d+)"`)

func (b *browser) openEdit(id string) string {
	b.t.Helper()
	resp, body := b.get("/admin/products/" + id + "/edit")
	require.Equal(b.t, http.StatusOK, resp.StatusCode, body)
	m := formVersion.FindStringSubmatch(body)
	require.Len(b.t, m, 2)
	return m[1]
}

func editForm(version, stock string) url.Values {
	return url.Values{
		"version": {version}, "name": {"Edited"}, "description": {""}, "category": {"Tests"},
		"price": {"10.00"}, "stock": {stock}, "weight_kg": {"1"},
	}
}

func TestAnEditFormOpenDuringACheckout(t *testing.T) {
	c := startApp(t)
	id := c.product("EDIT-1", "10.00", 5)
	admin := newBrowser(c)

	version := admin.openEdit(id)
	resp, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_declined", item(id, 2, "10.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	resp, body = admin.post("/admin/products/"+id, editForm(version, "6"))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/admin/products", resp.Request.URL.Path, "a reservation and its release do not change the version: "+body)
	require.Equal(t, 6, c.stockOf(id))

	version = admin.openEdit(id)
	resp, body = c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 2, "10.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	resp, body = admin.post("/admin/products/"+id, editForm(version, "9"))
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	require.Contains(t, body, "changed by someone else", "a sale changes the physical stock, so the form must be reloaded")
	require.Equal(t, 4, c.stockOf(id))
}

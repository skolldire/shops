package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const userCookie = "demo_user"

func (ui *UI) currentUser(r *http.Request) string {
	c, err := r.Cookie(userCookie)
	if err == nil {
		if user, err := url.QueryUnescape(c.Value); err == nil && slices.Contains(ui.users, user) {
			return user
		}
	}
	return ui.users[0]
}

func (ui *UI) selectUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		ui.badForm(w, r, err)
		return
	}
	user := r.PostForm.Get("user")
	if !slices.Contains(ui.users, user) {
		ui.errorPage(w, r, http.StatusBadRequest, errorPage{
			Title: "Unknown demo user", Status: http.StatusBadRequest,
			Message: "Choose one of the demo users listed in the header.",
		})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: userCookie, Value: url.QueryEscape(user), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil,
	})
	http.Redirect(w, r, localPath(r.PostForm.Get("next")), http.StatusSeeOther)
}

func localPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") ||
		strings.HasPrefix(raw, "//") || strings.Contains(raw, `\`) {
		return "/"
	}
	return u.RequestURI()
}

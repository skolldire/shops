package httpapi

import (
	"context"
	"net/http"

	"github.com/skolldire/shops/internal/platform/httpx"
)

const DemoUserHeader = "X-Demo-User-ID"

type userKey struct{}

func (h *Handler) demoUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get(DemoUserHeader)
		if !h.users[user] {
			httpx.WriteError(w, r, httpx.ErrorResponse{
				Status: http.StatusBadRequest, Code: "invalid_demo_user",
				Detail: "The " + DemoUserHeader + " header must name one of the demo users.",
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	})
}

func userFrom(ctx context.Context) string {
	user, _ := ctx.Value(userKey{}).(string)
	return user
}

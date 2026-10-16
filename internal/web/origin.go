package web

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

type origin struct {
	scheme string
	host   string
	port   string
}

func (ui *UI) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !sameOrigin(r, ui.publicOrigins...) {
			ui.errorPage(w, r, http.StatusForbidden, errorPage{
				Title: "Request blocked", Status: http.StatusForbidden,
				Message: "This form was not sent from this site, so it was not processed. Go back to the page and try again.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func ValidPublicOrigin(raw string) bool {
	_, ok := parseOrigin(raw)
	return ok
}

func sameOrigin(r *http.Request, public ...origin) bool {
	self, ok := requestOrigin(r)
	if !ok {
		return false
	}
	accepted := func(got origin) bool { return got == self || slices.Contains(public, got) }
	if values, sent := r.Header["Origin"]; sent {
		if len(values) != 1 {
			return false
		}
		got, ok := parseOrigin(values[0])
		return ok && accepted(got)
	}
	if values, sent := r.Header["Referer"]; sent {
		if len(values) != 1 {
			return false
		}
		got, ok := parseReferer(values[0])
		return ok && accepted(got)
	}
	return false
}

func requestOrigin(r *http.Request) (origin, bool) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return authority(scheme, r.Host)
}

func parseOrigin(raw string) (origin, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return origin{}, false
	}
	return authority(u.Scheme, u.Host)
}

func parseReferer(raw string) (origin, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil {
		return origin{}, false
	}
	return authority(u.Scheme, u.Host)
}

func authority(scheme, hostport string) (origin, bool) {
	scheme = strings.ToLower(scheme)
	var defaultPort string
	switch scheme {
	case "http":
		defaultPort = "80"
	case "https":
		defaultPort = "443"
	default:
		return origin{}, false
	}
	if hostport == "" {
		return origin{}, false
	}
	host, port := hostport, defaultPort
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	} else if strings.Contains(hostport, ":") && !strings.HasPrefix(hostport, "[") {
		return origin{}, false
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if host == "" || port == "" {
		return origin{}, false
	}
	return origin{scheme: scheme, host: host, port: port}, true
}

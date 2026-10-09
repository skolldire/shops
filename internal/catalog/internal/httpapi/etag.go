package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

var (
	errPreconditionRequired = errors.New("If-Match header with the product version is required")
	errInvalidPrecondition  = errors.New(`If-Match must be a single entity tag such as "3"`)
)

func etag(version int) string {
	return `"` + strconv.Itoa(version) + `"`
}

func ifMatchVersion(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" || raw == "*" {
		return 0, errPreconditionRequired
	}
	tag := strings.TrimPrefix(raw, "W/")
	if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return 0, errInvalidPrecondition
	}
	version, err := strconv.Atoi(tag[1 : len(tag)-1])
	if err != nil || version < 1 {
		return 0, errInvalidPrecondition
	}
	return version, nil
}

package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	contentTypeJSON    = "application/json"
	contentTypeProblem = "application/problem+json"
)

type ErrorResponse struct {
	Status int
	Code   string
	Title  string
	Detail string
	Errors []validation.FieldError
}

type problem struct {
	Type      string                  `json:"type"`
	Title     string                  `json:"title"`
	Status    int                     `json:"status"`
	Detail    string                  `json:"detail,omitempty"`
	Code      string                  `json:"code,omitempty"`
	Instance  string                  `json:"instance"`
	RequestID string                  `json:"request_id,omitempty"`
	Errors    []validation.FieldError `json:"errors,omitempty"`
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	writeBody(w, status, contentTypeJSON, v)
}

func WriteError(w http.ResponseWriter, r *http.Request, e ErrorResponse) {
	status := e.Status
	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
	}
	title := e.Title
	if title == "" {
		title = http.StatusText(status)
	}
	writeBody(w, status, contentTypeProblem, problem{
		Type:      "about:blank",
		Title:     title,
		Status:    status,
		Detail:    e.Detail,
		Code:      e.Code,
		Instance:  r.URL.Path,
		RequestID: RequestIDFrom(r.Context()),
		Errors:    e.Errors,
	})
}

func writeBody(w http.ResponseWriter, status int, contentType string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		contentType = contentTypeProblem
		body = []byte(`{"type":"about:blank","title":"Internal Server Error","status":500,"code":"internal_error"}`)
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

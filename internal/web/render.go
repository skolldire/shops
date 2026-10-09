package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/shopspring/decimal"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type renderer struct {
	pages map[string]*template.Template
}

func newRenderer() (*renderer, error) {
	funcs := template.FuncMap{
		"money":  func(d decimal.Decimal) string { return d.StringFixed(2) },
		"weight": func(d decimal.Decimal) string { return d.StringFixed(3) },
		"itoa":   strconv.Itoa,
	}
	partials, err := fs.Glob(templateFS, "templates/_*.html")
	if err != nil {
		return nil, fmt.Errorf("web: list partials: %w", err)
	}
	pages, err := fs.Glob(templateFS, "templates/[a-z]*.html")
	if err != nil {
		return nil, fmt.Errorf("web: list pages: %w", err)
	}
	r := &renderer{pages: make(map[string]*template.Template, len(pages))}
	for _, page := range pages {
		files := append(append([]string{}, partials...), page)
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("web: parse %s: %w", page, err)
		}
		r.pages[page[len("templates/"):len(page)-len(".html")]] = t
	}
	return r, nil
}

func (r *renderer) render(w http.ResponseWriter, status int, page, block string, data any) error {
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("web: unknown page %q", page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, data); err != nil {
		return fmt.Errorf("web: render %s: %w", page, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := buf.WriteTo(w)
	return err
}

func staticFiles() (http.Handler, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("web: static files: %w", err)
	}
	return http.StripPrefix("/static/", http.FileServerFS(sub)), nil
}

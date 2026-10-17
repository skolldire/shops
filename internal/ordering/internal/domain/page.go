package domain

import "github.com/skolldire/shops/internal/platform/validation"

const (
	defaultPageSize = 20
	maxPageSize     = 100
	maxPage         = 100000
)

type Page struct {
	Page     int
	PageSize int
}

func (p Page) Offset() int {
	return (p.Page - 1) * p.PageSize
}

func NewPage(page, pageSize *int) (Page, error) {
	var errs validation.Errors
	p := Page{Page: 1, PageSize: defaultPageSize}
	if page != nil {
		p.Page = *page
		if p.Page < 1 || p.Page > maxPage {
			errs.Add("page", validation.CodeOutOfRange, "must be between 1 and 100000")
		}
	}
	if pageSize != nil {
		p.PageSize = *pageSize
		if p.PageSize < 1 || p.PageSize > maxPageSize {
			errs.Add("page_size", validation.CodeOutOfRange, "must be between 1 and 100")
		}
	}
	if err := errs.Err(); err != nil {
		return Page{}, err
	}
	return p, nil
}

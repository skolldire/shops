package domain

import "github.com/skolldire/shops/internal/platform/validation"

type Paging struct {
	Page     int
	PageSize int
}

func (p Paging) Offset() int {
	return (p.Page - 1) * p.PageSize
}

func NewPaging(page, pageSize int) (Paging, error) {
	var errs validation.Errors
	p := checkPaging(&errs, page, pageSize)
	if err := errs.Err(); err != nil {
		return Paging{}, err
	}
	return p, nil
}

func checkPaging(errs *validation.Errors, page, pageSize int) Paging {
	p := Paging{Page: page, PageSize: pageSize}
	switch {
	case p.Page == 0:
		p.Page = 1
	case p.Page < 0 || p.Page > maxPage:
		errs.Add("page", validation.CodeOutOfRange, "must be between 1 and 100000")
	}
	switch {
	case p.PageSize == 0:
		p.PageSize = defaultPageSize
	case p.PageSize < 0 || p.PageSize > maxPageSize:
		errs.Add("page_size", validation.CodeOutOfRange, "must be between 1 and 100")
	}
	return p
}

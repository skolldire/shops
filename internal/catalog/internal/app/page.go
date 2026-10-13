package app

import "github.com/skolldire/shops/internal/catalog/internal/domain"

type Page struct {
	Items    []domain.Product
	Page     int
	PageSize int
	Total    int
}

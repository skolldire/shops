package domain

import (
	"errors"
	"time"
)

type ImportMode string

const (
	ImportAdd     ImportMode = "add"
	ImportReplace ImportMode = "replace"
)

var ErrInvalidImportMode = errors.New("catalog: the import mode must be add or replace")

func ParseImportMode(raw string) (ImportMode, error) {
	switch mode := ImportMode(raw); mode {
	case ImportAdd, ImportReplace:
		return mode, nil
	case "":
		return ImportReplace, nil
	}
	return "", ErrInvalidImportMode
}

type ImportStatus string

const (
	ImportPreview   ImportStatus = "PREVIEW"
	ImportApplying  ImportStatus = "APPLYING"
	ImportApplied   ImportStatus = "APPLIED"
	ImportCancelled ImportStatus = "CANCELLED"
)

const (
	CancelledByUser  = "user"
	CancelledExpired = "expired"
)

type ImportStamp struct {
	CreatedAt time.Time
	AppliedAt time.Time
}

type PreviewLine struct {
	SKU      string
	Before   int
	After    int
	Reserved int
}

type ImportTarget struct {
	Stock    int
	Reserved int
	Deleted  bool
}

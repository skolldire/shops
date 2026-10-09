package validation

import "strings"

const (
	CodeRequired        = "required"
	CodeTooLong         = "too_long"
	CodeNotPositive     = "not_positive"
	CodeTooManyDecimals = "too_many_decimals"
	CodeOutOfRange      = "out_of_range"
	CodeInvalidFormat   = "invalid_format"
	CodeImmutable       = "immutable"
)

type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Errors []FieldError

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i, fe := range e {
		parts[i] = fe.Field + ": " + fe.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

func (e *Errors) Add(field, code, message string) {
	*e = append(*e, FieldError{Field: field, Code: code, Message: message})
}

func (e Errors) Err() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

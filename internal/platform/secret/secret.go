package secret

import (
	"fmt"
	"io"
)

const redacted = "[REDACTED]"

type Secret struct {
	value string
}

func New(v string) Secret {
	return Secret{value: v}
}

func (s Secret) Reveal() string {
	return s.value
}

func (s Secret) IsZero() bool {
	return s.value == ""
}

func (s Secret) String() string {
	return redacted
}

func (s Secret) GoString() string {
	return redacted
}

func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redacted)
}

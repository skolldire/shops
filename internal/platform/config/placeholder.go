package config

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	namePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	schemePattern = regexp.MustCompile(`^[a-z]+$`)
)

type placeholder struct {
	raw        string
	scheme     string
	ref        string
	def        string
	hasDefault bool
}

func parseBody(raw string) (*placeholder, error) {
	body := raw[2 : len(raw)-1]
	if name, def, ok := strings.Cut(body, ":-"); ok && namePattern.MatchString(name) {
		return &placeholder{raw: raw, ref: name, def: def, hasDefault: true}, nil
	}
	if namePattern.MatchString(body) {
		return &placeholder{raw: raw, ref: body}, nil
	}
	if scheme, ref, ok := strings.Cut(body, ":"); ok && schemePattern.MatchString(scheme) {
		if ref == "" {
			return nil, fmt.Errorf("placeholder %s has an empty reference", raw)
		}
		return &placeholder{raw: raw, scheme: scheme, ref: ref}, nil
	}
	return nil, fmt.Errorf("invalid placeholder %s", raw)
}

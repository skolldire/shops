package logger

import (
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var (
	sensitiveKeys = map[string]bool{
		"password": true, "passwd": true, "pwd": true, "secret": true, "token": true,
		"api_key": true, "apikey": true, "access_key": true, "secret_key": true,
		"authorization": true, "auth": true, "cookie": true, "set-cookie": true,
		"card_number": true, "cvv": true, "cvc": true,
	}
	sensitivePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api_?key)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&]+)`),
		regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)([A-Za-z0-9._~+/=-]+)`),
		regexp.MustCompile(`()()\b(?:\d[ -]?){13,19}\b`),
	}
)

func sanitizeValue(key string, v any) any {
	if sensitiveKeys[strings.ToLower(key)] {
		return redacted
	}
	if s, ok := v.(string); ok {
		return Sanitize(s)
	}
	return v
}

func Sanitize(s string) string {
	for _, p := range sensitivePatterns {
		s = p.ReplaceAllString(s, "${1}${2}"+redacted)
	}
	return s
}

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHealthcheckURL(t *testing.T) {
	tests := map[string]struct {
		args    []string
		addr    string
		want    string
		wantErr string
	}{
		"HTTP_ADDR unset":     {nil, "", "http://127.0.0.1:8080/health/live", ""},
		"port only":           {nil, ":9090", "http://127.0.0.1:9090/health/live", ""},
		"all IPv4 interfaces": {nil, "0.0.0.0:9090", "http://127.0.0.1:9090/health/live", ""},
		"loopback":            {nil, "127.0.0.1:9090", "http://127.0.0.1:9090/health/live", ""},
		"all IPv6 interfaces": {nil, "[::]:9090", "http://127.0.0.1:9090/health/live", ""},
		"specific IPv6":       {nil, "[::1]:9090", "http://[::1]:9090/health/live", ""},
		"flag wins over env":  {[]string{"-url", "http://10.0.0.1:7000/health/live"}, ":9090", "http://10.0.0.1:7000/health/live", ""},
		"invalid HTTP_ADDR":   {nil, "9090", "", "healthcheck: invalid HTTP_ADDR"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := healthcheckURL(tt.args, func(key string) string {
				if key == "HTTP_ADDR" {
					return tt.addr
				}
				return ""
			})

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

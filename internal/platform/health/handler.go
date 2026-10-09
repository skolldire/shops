package health

import (
	"net/http"

	"github.com/skolldire/shops/internal/platform/httpx"
)

func (s *Service) Live(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, Report{Status: StatusUp})
}

func (s *Service) Ready(w http.ResponseWriter, r *http.Request) {
	report := s.Check(r.Context())
	status := http.StatusOK
	if report.Status != StatusUp {
		status = http.StatusServiceUnavailable
	}
	httpx.WriteJSON(w, status, report)
}

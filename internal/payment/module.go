package payment

import "context"

const (
	tokenApproved = "pm_approved"
	tokenDeclined = "pm_declined"
	refPrefix     = "fake_"
)

type Module struct{}

func NewModule() *Module {
	return &Module{}
}

func (m *Module) Charge(_ context.Context, r ChargeRequest) (ChargeResult, error) {
	switch r.Token {
	case tokenApproved:
		return ChargeResult{Status: StatusApproved, ProviderRef: refPrefix + r.Reference}, nil
	case tokenDeclined:
		return ChargeResult{Status: StatusDeclined, Reason: ReasonCardDeclined}, nil
	}
	return ChargeResult{Status: StatusDeclined, Reason: ReasonUnknownPaymentToken}, nil
}

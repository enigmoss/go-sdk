package enigmoss

import "context"

// PayoutMethod is a currency and network available for payouts, with limits and the base
// fee.
type PayoutMethod struct {
	CurrencyCode string `json:"currency_code"`
	NetworkCode  string `json:"network_code"`
	// ContractAddress is the token contract; empty for the native coin of the network.
	ContractAddress string `json:"contract_address"`
	MinAmount       string `json:"min_amount"`
	MaxAmount       string `json:"max_amount"`
	Fee             struct {
		// Amount is the fixed part of the fee.
		Amount string `json:"amount"`
		// Percent is the percent part of the fee.
		Percent string `json:"percent"`
	} `json:"fee"`
}

// PayoutMethodDetails are the limits, decimals, memo support and per-priority fees of one
// payout currency and network.
type PayoutMethodDetails struct {
	CurrencyCode string `json:"currency_code"`
	NetworkCode  string `json:"network_code"`
	NetworkName  string `json:"network_name"`
	// TokenStandard is such as "TRC-20"; empty for a native coin.
	TokenStandard   string `json:"token_standard"`
	MinAmount       string `json:"min_amount"`
	MaxAmount       string `json:"max_amount"`
	Decimals        int    `json:"decimals"`
	IsMemoAvailable bool   `json:"is_memo_available"`
	Priorities      []struct {
		Priority PayoutPriority `json:"priority"`
		Fee      string         `json:"fee"`
	} `json:"priorities"`
}

var (
	epPayoutMethodList = get("/payout-methods", scopeAny, "payout methods")
	epPayoutMethodGet  = get("/payout-methods/%s/%s", scopeAny, "payout methods")
)

// PayoutMethodService lists the currencies and networks available for payouts. Either key.
type PayoutMethodService struct{ c *core }

// List returns the payout methods. GET /api/v4/payout-methods.
func (s *PayoutMethodService) List(ctx context.Context) ([]*PayoutMethod, error) {
	return many[PayoutMethod](ctx, s.c, call{ep: epPayoutMethodList})
}

// Get returns the details of one payout currency and network.
// GET /api/v4/payout-methods/{currency}/{network}.
func (s *PayoutMethodService) Get(ctx context.Context, currencyCode, networkCode string) (*PayoutMethodDetails, error) {
	return one[PayoutMethodDetails](ctx, s.c, call{ep: epPayoutMethodGet, params: []string{currencyCode, networkCode}})
}

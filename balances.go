package enigmoss

import "context"

// Balance is the business balance in one currency, shared by all projects of the account
// owner.
type Balance struct {
	CurrencyCode string `json:"currency_code"`
	Balance      string `json:"balance"`
	// BalanceUSD is the USD equivalent at the current rate, 2 decimals; empty when no rate
	// is available.
	BalanceUSD string `json:"balance_usd"`
}

var epBalanceList = get("/balances", scopePayment, "balances")

// BalanceService lists the business balances. It needs the payment key.
type BalanceService struct{ c *core }

// List returns the business balances, one per currency. GET /api/v4/balances, permission
// info_balance.
func (s *BalanceService) List(ctx context.Context) ([]*Balance, error) {
	return many[Balance](ctx, s.c, call{ep: epBalanceList})
}

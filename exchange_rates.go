package enigmoss

import "context"

// ExchangeRate is the current rate from one currency to another.
type ExchangeRate struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Rate is a decimal string with 18 fractional digits.
	Rate string `json:"rate"`
}

var epExchangeRateList = get("/exchange-rates", scopeAny, "exchange rates")

// ExchangeRateService lists exchange rates. Either key.
type ExchangeRateService struct{ c *core }

// List returns the current exchange rates; from limits them to one source currency ("" for
// all). GET /api/v4/exchange-rates.
func (s *ExchangeRateService) List(ctx context.Context, from string) ([]*ExchangeRate, error) {
	return many[ExchangeRate](ctx, s.c, call{ep: epExchangeRateList, query: []queryParam{{"from", from}}})
}

package enigmoss

import "context"

// PaymentMethod is a currency and network the customer can pay with, and your discount
// for it.
type PaymentMethod struct {
	CurrencyCode string `json:"currency_code"`
	NetworkCode  string `json:"network_code"`
	IsEnabled    bool   `json:"is_enabled"`
	// DiscountPercent is positive for a discount, negative for a surcharge, 0 when unset.
	DiscountPercent int `json:"discount_percent"`
}

var (
	epPaymentMethodList   = get("/payment-methods", scopeAny, "payment methods")
	epPaymentMethodUpdate = patch("/payment-methods/%s/%s", scopePayment, "payment method settings")
)

// PaymentMethodService lists the payment methods and sets their discounts.
type PaymentMethodService struct{ c *core }

// List returns the payment methods of your tariff. GET /api/v4/payment-methods, either key.
func (s *PaymentMethodService) List(ctx context.Context) ([]*PaymentMethod, error) {
	return many[PaymentMethod](ctx, s.c, call{ep: epPaymentMethodList})
}

// UpdateDiscount sets the discount (positive) or surcharge (negative) of a payment method,
// -99..90 percent. PATCH /api/v4/payment-methods/{currency}/{network}, payment key,
// permission payment_settings.
func (s *PaymentMethodService) UpdateDiscount(ctx context.Context, currencyCode, networkCode string, discountPercent int) (*PaymentMethod, error) {
	if err := checkRange(discountPercent, "discountPercent", -99, 90); err != nil {
		return nil, err
	}
	body := struct {
		DiscountPercent int `json:"discount_percent"`
	}{discountPercent}
	return one[PaymentMethod](ctx, s.c, call{ep: epPaymentMethodUpdate, params: []string{currencyCode, networkCode}, body: body})
}

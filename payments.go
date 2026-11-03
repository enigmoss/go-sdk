package enigmoss

import (
	"context"
	"errors"
	"iter"
	"time"
)

// PaymentStatus is the status of an invoice. The server may add statuses: keep a default
// branch in a switch, and decide with Payment.IsFinal and Payment.IsSuccessful.
type PaymentStatus string

// Payment statuses.
const (
	PaymentPending          PaymentStatus = "pending"
	PaymentProcessing       PaymentStatus = "processing"
	PaymentConfirming       PaymentStatus = "confirming"
	PaymentPaid             PaymentStatus = "paid"
	PaymentOverpaid         PaymentStatus = "overpaid"
	PaymentUnderpaid        PaymentStatus = "underpaid"
	PaymentUnderpaidWaiting PaymentStatus = "underpaid_waiting"
	PaymentExpired          PaymentStatus = "expired"
	PaymentCancelled        PaymentStatus = "cancelled"
	PaymentFailed           PaymentStatus = "failed"
	PaymentLocked           PaymentStatus = "locked"
	PaymentRefunding        PaymentStatus = "refunding"
	PaymentRefunded         PaymentStatus = "refunded"
	PaymentRefundFailed     PaymentStatus = "refund_failed"
)

// Payment is an invoice, or a deposit to a static wallet (then WalletID is set). The same
// object is the data of payment.* webhooks. Amounts are decimal strings, never floats.
type Payment struct {
	ID       string        `json:"id"`
	OrderID  string        `json:"order_id"`
	WalletID string        `json:"wallet_id"`
	Status   PaymentStatus `json:"status"`
	// IsFinal reports whether the status will not change any more.
	IsFinal     bool   `json:"is_final"`
	FailureCode string `json:"failure_code"`
	// Amount is the invoice amount in CurrencyCode.
	Amount       string             `json:"amount"`
	CurrencyCode string             `json:"currency_code"`
	Payer        PaymentPayer       `json:"payer"`
	Paid         PaidAmount         `json:"paid"`
	Settlement   PaymentSettlement  `json:"settlement"`
	Conversion   *PaymentConversion `json:"conversion"`
	Discount     PaymentDiscount    `json:"discount"`
	TxHash       string             `json:"tx_hash"`
	// PaymentURL is the hosted payment page to redirect the customer to.
	PaymentURL  string    `json:"payment_url"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	PaidAt      time.Time `json:"paid_at"`
	Description string    `json:"description"`
	// Metadata is your metadata, returned unchanged.
	Metadata map[string]any `json:"metadata"`
}

// IsSuccessful is the only fulfilment rule: the customer paid at least the invoice amount
// and the status will not change any more. A paid payment can be non-final while an
// automatic conversion settles: fulfil when IsSuccessful returns true.
func (p *Payment) IsSuccessful() bool {
	return (p.Status == PaymentPaid || p.Status == PaymentOverpaid) && p.IsFinal
}

// IsDeposit reports whether the payment is a deposit to a static wallet. A deposit has a
// WalletID and no OrderID: branch on it first.
func (p *Payment) IsDeposit() bool { return p.WalletID != "" }

// PaymentPayer is what the customer pays. Empty until the customer picks a currency.
type PaymentPayer struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
	NetworkCode  string `json:"network_code"`
	// Address is the deposit address the customer pays to.
	Address string `json:"address"`
	// SenderAddress is the address the customer paid from, when known.
	SenderAddress string `json:"sender_address"`
	ExchangeRate  string `json:"exchange_rate"`
}

// PaidAmount is the amount received.
type PaidAmount struct {
	// Amount is in the payer currency.
	Amount string `json:"amount"`
	// AmountUSD is the same amount in USD, 2 decimals.
	AmountUSD string `json:"amount_usd"`
}

// PaymentSettlement is what is credited to your balance and the fee charged.
type PaymentSettlement struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
	Fee          string `json:"fee"`
}

// PaymentConversion is the automatic conversion of the received funds.
type PaymentConversion struct {
	From struct {
		Amount       string `json:"amount"`
		CurrencyCode string `json:"currency_code"`
		NetworkCode  string `json:"network_code"`
	} `json:"from"`
	To struct {
		CurrencyCode string `json:"currency_code"`
	} `json:"to"`
	Rate string `json:"rate"`
}

// PaymentDiscount is the discount (positive percent) or surcharge (negative) applied.
type PaymentDiscount struct {
	Percent *int   `json:"percent"`
	Amount  string `json:"amount"`
}

// PaymentParams are the parameters of a new invoice. Amount, CurrencyCode and OrderID are
// required; an empty field is not sent.
type PaymentParams struct {
	// Amount is a decimal string in CurrencyCode, such as "10.50".
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
	// OrderID is your unique order ID (1-128 letters, digits, "-", "_"). One order_id is
	// one invoice, forever.
	OrderID string `json:"order_id"`
	// SettlementCurrencyCode is the currency the customer pays in, to fix it.
	SettlementCurrencyCode string `json:"settlement_currency_code,omitempty"`
	// NetworkCode is the network the customer pays on, such as "tron".
	NetworkCode string `json:"network_code,omitempty"`
	// ReturnURL is the "back to shop" link of the payment page.
	ReturnURL string `json:"return_url,omitempty"`
	// SuccessURL is where the customer goes after paying.
	SuccessURL string `json:"success_url,omitempty"`
	// CallbackURL is the public URL for the payment.* webhooks of this invoice.
	CallbackURL string `json:"callback_url,omitempty"`
	// ExpiresIn is the lifetime in seconds, 300-43200 (default 3600).
	ExpiresIn int `json:"expires_in,omitempty"`
	// AllowMultiplePayments accepts several payments to the invoice.
	AllowMultiplePayments *bool `json:"allow_multiple_payments,omitempty"`
	// CustomerFeePercent is the share of the fee paid by the customer, 0-100.
	CustomerFeePercent *int `json:"customer_fee_percent,omitempty"`
	// UnderpaymentTolerancePercent is the underpayment still treated as paid, "0"-"5.00".
	UnderpaymentTolerancePercent string `json:"underpayment_tolerance_percent,omitempty"`
	// DiscountPercent is -99..90: positive is a discount, negative a surcharge.
	DiscountPercent *int `json:"discount_percent,omitempty"`
	// AllowedMethods offers only these currencies and networks.
	AllowedMethods []PaymentMethodRef `json:"allowed_methods,omitempty"`
	// ExcludedMethods does not offer these currencies and networks.
	ExcludedMethods []PaymentMethodRef `json:"excluded_methods,omitempty"`
	CustomerEmail   string             `json:"customer_email,omitempty"`
	// Metadata is any JSON object, returned unchanged in the payment and its webhooks.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Description is shown to the customer, up to 150 characters.
	Description string `json:"description,omitempty"`
	// Theme is the payment page theme: "light", "dark" or "system".
	Theme        string `json:"theme,omitempty"`
	ReferralCode string `json:"referral_code,omitempty"`
}

// PaymentMethodRef is a currency, optionally on one network, for AllowedMethods and
// ExcludedMethods. Without a network it applies to every network of the currency.
type PaymentMethodRef struct {
	CurrencyCode string `json:"currency_code"`
	NetworkCode  string `json:"network_code,omitempty"`
}

func (p *PaymentParams) check() error {
	if p == nil {
		return errNilParams
	}
	errs := []error{checkAmount(p.Amount, "Amount"), checkOrderID(p.OrderID, "OrderID"), checkLength(p.Description, "Description", 0, 150)}
	if p.ExpiresIn != 0 {
		errs = append(errs, checkRange(p.ExpiresIn, "ExpiresIn", 300, 43200))
	}
	if p.CustomerFeePercent != nil {
		errs = append(errs, checkRange(*p.CustomerFeePercent, "CustomerFeePercent", 0, 100))
	}
	if p.DiscountPercent != nil {
		errs = append(errs, checkRange(*p.DiscountPercent, "DiscountPercent", -99, 90))
	}
	if p.UnderpaymentTolerancePercent != "" && !percentPattern.MatchString(p.UnderpaymentTolerancePercent) {
		errs = append(errs, errors.New(`enigmoss: UnderpaymentTolerancePercent must be a decimal string with up to 2 decimals, e.g. "1.5"`))
	}
	return firstError(errs...)
}

// PaymentQuery filters the invoice list. Every field is optional.
type PaymentQuery struct {
	OrderID     string
	WalletID    string
	Status      PaymentStatus
	CreatedFrom time.Time
	CreatedTo   time.Time
	// Limit is the page size, 1-50 (default 20).
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

func (q *PaymentQuery) params() ([]queryParam, error) {
	if q == nil {
		q = &PaymentQuery{}
	}
	walletID := q.WalletID
	errs := []error{checkLimit(q.Limit, 50)}
	if q.OrderID != "" {
		errs = append(errs, checkOrderID(q.OrderID, "OrderID"))
	}
	if walletID != "" {
		var err error
		walletID, err = checkUUID(walletID, "WalletID")
		errs = append(errs, err)
	}
	return []queryParam{
		{"order_id", q.OrderID},
		{"wallet_id", walletID},
		{"status", string(q.Status)},
		{"created_from", queryTime(q.CreatedFrom)},
		{"created_to", queryTime(q.CreatedTo)},
		{"limit", queryInt(q.Limit)},
		{"cursor", q.Cursor},
	}, firstError(errs...)
}

// AMLVerificationLink is a link where the customer of a locked payment passes the AML
// verification.
type AMLVerificationLink struct {
	URL string `json:"url"`
	// Status is "init", "pending" or "completed".
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

var (
	epPaymentCreate  = create("/payments", scopePayment, attemptTimeout, "payments")
	epPaymentGet     = get("/payments/%s", scopePayment, "payments")
	epPaymentList    = get("/payments", scopePayment, "payments")
	epPaymentCancel  = action("/payments/%s/cancel", scopePayment, "payments")
	epPaymentMark    = action("/payments/%s/mark-paid", scopePayment, "payments")
	epPaymentAMLLink = action("/payments/%s/aml-verification-links", scopePayment, "payments")
)

// PaymentService creates and reads invoices. It needs the payment key.
type PaymentService struct{ c *core }

// Create creates an invoice and returns it with the payment page URL (PaymentURL).
//
// Safe to repeat: the Idempotency-Key is derived from the order_id, so a repeated call
// returns the same invoice (within 24 hours). POST /api/v4/payments, permission
// payment_create_invoice.
func (s *PaymentService) Create(ctx context.Context, params *PaymentParams) (*Payment, error) {
	if err := params.check(); err != nil {
		return nil, err
	}
	return one[Payment](ctx, s.c, call{ep: epPaymentCreate, body: params, orderID: params.OrderID})
}

// Get returns an invoice by its ID. GET /api/v4/payments/{id}, permission info_payments.
func (s *PaymentService) Get(ctx context.Context, id string) (*Payment, error) {
	return s.byID(ctx, epPaymentGet, id)
}

// FindByOrderID returns nil, nil when no invoice has this order_id. Use it to reconcile
// after an error with OutcomeUnknown. Static wallet deposits have no order_id: list them
// by WalletID.
func (s *PaymentService) FindByOrderID(ctx context.Context, orderID string) (*Payment, error) {
	if err := checkOrderID(orderID, "orderID"); err != nil {
		return nil, err
	}
	return first(s.List(ctx, &PaymentQuery{OrderID: orderID, Limit: 1}))
}

// List returns one page of invoices, newest first. GET /api/v4/payments, permission
// info_payments.
func (s *PaymentService) List(ctx context.Context, query *PaymentQuery) (*Page[Payment], error) {
	return s.list(ctx, query, 0)
}

// All returns every invoice matching the query, fetching pages lazily.
func (s *PaymentService) All(ctx context.Context, query *PaymentQuery) iter.Seq2[*Payment, error] {
	q := PaymentQuery{}
	if query != nil {
		q = *query
	}
	return all(ctx, q.Cursor, func(ctx context.Context, cursor string) (*Page[Payment], error) {
		q.Cursor = cursor
		return s.list(ctx, &q, iterateRetryDelay)
	})
}

func (s *PaymentService) list(ctx context.Context, query *PaymentQuery, retryDelay time.Duration) (*Page[Payment], error) {
	qp, err := query.params()
	if err != nil {
		return nil, err
	}
	return page[Payment](ctx, s.c, call{ep: epPaymentList, query: qp, maxRetryDelay: retryDelay})
}

// Cancel cancels an unpaid invoice (pending or processing, nothing received).
// POST /api/v4/payments/{id}/cancel, permission payment_cancel.
func (s *PaymentService) Cancel(ctx context.Context, id string) (*Payment, error) {
	return s.byID(ctx, epPaymentCancel, id)
}

// MarkPaid accepts an underpaid invoice as paid. For underpaid_waiting it asks the gateway
// to close the invoice, and the response may still be underpaid_waiting: wait for the
// payment.* webhook. POST /api/v4/payments/{id}/mark-paid, permission payment_mark_paid.
func (s *PaymentService) MarkPaid(ctx context.Context, id string) (*Payment, error) {
	return s.byID(ctx, epPaymentMark, id)
}

// CreateAMLVerificationLinks returns the AML verification links of a locked payment,
// creating one when none is active. POST /api/v4/payments/{id}/aml-verification-links,
// permission info_payments.
func (s *PaymentService) CreateAMLVerificationLinks(ctx context.Context, id string) ([]*AMLVerificationLink, error) {
	id, err := checkUUID(id, "id")
	if err != nil {
		return nil, err
	}
	return many[AMLVerificationLink](ctx, s.c, call{ep: epPaymentAMLLink, params: []string{id}})
}

func (s *PaymentService) byID(ctx context.Context, ep *endpoint, id string) (*Payment, error) {
	id, err := checkUUID(id, "id")
	if err != nil {
		return nil, err
	}
	return one[Payment](ctx, s.c, call{ep: ep, params: []string{id}})
}

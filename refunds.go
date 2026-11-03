package enigmoss

import (
	"context"
	"iter"
	"time"
)

// RefundStatus is the status of a refund. The server may add statuses: keep a default
// branch in a switch, and decide with Refund.IsFinal.
type RefundStatus string

// Refund statuses.
const (
	RefundPending    RefundStatus = "pending"
	RefundProcessing RefundStatus = "processing"
	RefundCompleted  RefundStatus = "completed"
	RefundFailed     RefundStatus = "failed"
)

// Refund is a refund of a payment or of a blocked static wallet. The same object is the
// data of refund.* webhooks.
type Refund struct {
	ID string `json:"id"`
	// OrderID is empty only for automatic refunds of a blocked wallet.
	OrderID string `json:"order_id"`
	// Source is "payment", "aml" or "blocked_wallet".
	Source       string       `json:"source"`
	PaymentID    string       `json:"payment_id"`
	WalletID     string       `json:"wallet_id"`
	Status       RefundStatus `json:"status"`
	IsFinal      bool         `json:"is_final"`
	FailureCode  string       `json:"failure_code"`
	Amount       string       `json:"amount"`
	CurrencyCode string       `json:"currency_code"`
	NetworkCode  string       `json:"network_code"`
	Address      string       `json:"address"`
	TxHash       string       `json:"tx_hash"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
	CompletedAt  time.Time    `json:"completed_at"`
}

// PaymentRefundParams are the parameters of a refund of a paid invoice.
type PaymentRefundParams struct {
	// OrderID is your unique refund ID. One order_id is one refund, forever, even a failed one.
	OrderID string `json:"order_id"`
	// Address is the customer address to refund to (the currency and network of the payment).
	Address string `json:"address"`
	// Amount is a decimal string; the whole paid amount when empty.
	Amount string `json:"amount,omitempty"`
	// FeeDeductedFromAmount: true (default) takes the fee out of the amount, false adds it on top.
	FeeDeductedFromAmount *bool `json:"fee_deducted_from_amount,omitempty"`
	// CallbackURL is the URL for refund.* webhooks; the payment's callback URL when empty.
	CallbackURL string `json:"callback_url,omitempty"`
}

// WalletRefundParams are the parameters of the refund of a blocked static wallet.
type WalletRefundParams struct {
	// OrderID is your unique refund ID.
	OrderID string `json:"order_id"`
	// Address receives everything paid to the blocked wallet.
	Address string `json:"address"`
	// CallbackURL is the URL for refund.* webhooks; the wallet's callback URL when empty.
	CallbackURL string `json:"callback_url,omitempty"`
}

// RefundQuery filters the refund list. Every field is optional.
type RefundQuery struct {
	OrderID string
	// Source is "payment", "aml" or "blocked_wallet".
	Source      string
	Status      RefundStatus
	PaymentID   string
	WalletID    string
	CreatedFrom time.Time
	CreatedTo   time.Time
	// Limit is the page size, 1-100 (default 20).
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

func (q *RefundQuery) params() ([]queryParam, error) {
	if q == nil {
		q = &RefundQuery{}
	}
	paymentID, walletID := q.PaymentID, q.WalletID
	errs := []error{checkLimit(q.Limit, 100)}
	if q.OrderID != "" {
		errs = append(errs, checkOrderID(q.OrderID, "OrderID"))
	}
	var err error
	if paymentID != "" {
		paymentID, err = checkUUID(paymentID, "PaymentID")
		errs = append(errs, err)
	}
	if walletID != "" {
		walletID, err = checkUUID(walletID, "WalletID")
		errs = append(errs, err)
	}
	return []queryParam{
		{"order_id", q.OrderID},
		{"source", q.Source},
		{"status", string(q.Status)},
		{"payment_id", paymentID},
		{"wallet_id", walletID},
		{"created_from", queryTime(q.CreatedFrom)},
		{"created_to", queryTime(q.CreatedTo)},
		{"limit", queryInt(q.Limit)},
		{"cursor", q.Cursor},
	}, firstError(errs...)
}

var (
	epRefundPayment = create("/payments/%s/refunds", scopePayout, payoutTimeout, "refunds")
	epRefundWallet  = create("/wallets/%s/refunds", scopePayout, payoutTimeout, "refunds")
	epRefundGet     = get("/refunds/%s", scopePayout, "refunds")
	epRefundList    = get("/refunds", scopePayout, "refunds")
)

// RefundService refunds payments and blocked wallets. It needs the payout key, and the
// server IP must be allowed for payout-key requests. Creating refunds is limited to 1
// request per minute per project by default.
type RefundService struct{ c *core }

// CreateForPayment refunds a paid invoice, fully or partly, and returns the refund in
// status pending; follow it with refund.* webhooks.
//
// A refund is paid out like a payout: the call may take up to 180 seconds, so give it a
// context of at least 10 minutes in a background worker. Money-safe to repeat: with the
// same order_id it never refunds twice. On an error with OutcomeUnknown, reconcile with
// FindByOrderID; never use a new order_id. POST /api/v4/payments/{id}/refunds, permission
// payment_refund.
func (s *RefundService) CreateForPayment(ctx context.Context, paymentID string, params *PaymentRefundParams) (*Refund, error) {
	if params == nil {
		return nil, errNilParams
	}
	paymentID, err := checkUUID(paymentID, "paymentID")
	errs := []error{err, checkOrderID(params.OrderID, "OrderID"), checkLength(params.Address, "Address", 10, 128)}
	if params.Amount != "" {
		errs = append(errs, checkAmount(params.Amount, "Amount"))
	}
	if err := firstError(errs...); err != nil {
		return nil, err
	}
	return one[Refund](ctx, s.c, call{ep: epRefundPayment, params: []string{paymentID}, body: params, orderID: params.OrderID})
}

// CreateForWallet refunds everything paid to a blocked static wallet in one payout (once
// per wallet). The same rules as CreateForPayment apply. POST /api/v4/wallets/{id}/refunds,
// permission payment_refund.
func (s *RefundService) CreateForWallet(ctx context.Context, walletID string, params *WalletRefundParams) (*Refund, error) {
	if params == nil {
		return nil, errNilParams
	}
	walletID, err := checkUUID(walletID, "walletID")
	if err := firstError(err, checkOrderID(params.OrderID, "OrderID"), checkLength(params.Address, "Address", 10, 128)); err != nil {
		return nil, err
	}
	return one[Refund](ctx, s.c, call{ep: epRefundWallet, params: []string{walletID}, body: params, orderID: params.OrderID})
}

// Get returns a refund by its ID. GET /api/v4/refunds/{id}, permission payment_refund.
func (s *RefundService) Get(ctx context.Context, id string) (*Refund, error) {
	id, err := checkUUID(id, "id")
	if err != nil {
		return nil, err
	}
	return one[Refund](ctx, s.c, call{ep: epRefundGet, params: []string{id}})
}

// FindByOrderID returns nil, nil when no refund has this order_id: the reconciliation
// step after an error with OutcomeUnknown.
func (s *RefundService) FindByOrderID(ctx context.Context, orderID string) (*Refund, error) {
	if err := checkOrderID(orderID, "orderID"); err != nil {
		return nil, err
	}
	return first(s.List(ctx, &RefundQuery{OrderID: orderID, Limit: 1}))
}

// List returns one page of refunds, newest first. GET /api/v4/refunds, permission
// payment_refund.
func (s *RefundService) List(ctx context.Context, query *RefundQuery) (*Page[Refund], error) {
	return s.list(ctx, query, 0)
}

// All returns every refund matching the query, fetching pages lazily.
func (s *RefundService) All(ctx context.Context, query *RefundQuery) iter.Seq2[*Refund, error] {
	q := RefundQuery{}
	if query != nil {
		q = *query
	}
	return all(ctx, q.Cursor, func(ctx context.Context, cursor string) (*Page[Refund], error) {
		q.Cursor = cursor
		return s.list(ctx, &q, iterateRetryDelay)
	})
}

func (s *RefundService) list(ctx context.Context, query *RefundQuery, retryDelay time.Duration) (*Page[Refund], error) {
	qp, err := query.params()
	if err != nil {
		return nil, err
	}
	return page[Refund](ctx, s.c, call{ep: epRefundList, query: qp, maxRetryDelay: retryDelay})
}

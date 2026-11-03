package enigmoss

import (
	"context"
	"iter"
	"time"
)

// WalletStatus is the status of a static wallet. The server may add statuses: keep a
// default branch in a switch.
type WalletStatus string

// Wallet statuses.
const (
	WalletActive WalletStatus = "active"
	// WalletBlocked: new deposits are not credited; refundable once with
	// Refunds.CreateForWallet.
	WalletBlocked  WalletStatus = "blocked"
	WalletInactive WalletStatus = "inactive"
)

// Wallet is a static wallet: a permanent deposit address bound to your order_id. Every
// deposit to it is a Payment with WalletID set.
type Wallet struct {
	ID           string       `json:"id"`
	OrderID      string       `json:"order_id"`
	CurrencyCode string       `json:"currency_code"`
	NetworkCode  string       `json:"network_code"`
	Address      string       `json:"address"`
	Status       WalletStatus `json:"status"`
	// RefundIncoming reports whether deposits arriving after blocking are refunded.
	RefundIncoming bool   `json:"refund_incoming"`
	CallbackURL    string `json:"callback_url"`
	// PaymentURL is the hosted page that shows the address and QR code to the customer.
	PaymentURL string    `json:"payment_url"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// WalletParams are the parameters of a new static wallet. CurrencyCode, NetworkCode and
// OrderID are required.
type WalletParams struct {
	CurrencyCode string `json:"currency_code"`
	NetworkCode  string `json:"network_code"`
	// OrderID is your ID of the wallet owner (a customer, an account). One order_id is one
	// wallet, forever.
	OrderID string `json:"order_id"`
	// CallbackURL is the public URL for the payment.* webhooks of the wallet deposits.
	CallbackURL  string `json:"callback_url,omitempty"`
	ReferralCode string `json:"referral_code,omitempty"`
}

// WalletQuery filters the wallet list. Every field is optional.
type WalletQuery struct {
	OrderID      string
	Status       WalletStatus
	CurrencyCode string
	NetworkCode  string
	CreatedFrom  time.Time
	CreatedTo    time.Time
	// Limit is the page size, 1-50 (default 20).
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

func (q *WalletQuery) params() ([]queryParam, error) {
	if q == nil {
		q = &WalletQuery{}
	}
	errs := []error{checkLimit(q.Limit, 50)}
	if q.OrderID != "" {
		errs = append(errs, checkOrderID(q.OrderID, "OrderID"))
	}
	return []queryParam{
		{"order_id", q.OrderID},
		{"status", string(q.Status)},
		{"currency_code", q.CurrencyCode},
		{"network_code", q.NetworkCode},
		{"created_from", queryTime(q.CreatedFrom)},
		{"created_to", queryTime(q.CreatedTo)},
		{"limit", queryInt(q.Limit)},
		{"cursor", q.Cursor},
	}, firstError(errs...)
}

var (
	epWalletCreate = create("/wallets", scopePayment, attemptTimeout, "wallets")
	epWalletGet    = get("/wallets/%s", scopePayment, "wallets")
	epWalletList   = get("/wallets", scopePayment, "wallets")
	epWalletBlock  = action("/wallets/%s/block", scopePayment, "wallets")
)

// WalletService creates, reads and blocks static wallets. It needs the payment key.
type WalletService struct{ c *core }

// Create creates a static wallet. Safe to repeat with the same order_id.
// POST /api/v4/wallets, permission payment_create_static_wallet.
func (s *WalletService) Create(ctx context.Context, params *WalletParams) (*Wallet, error) {
	if params == nil {
		return nil, errNilParams
	}
	if err := checkOrderID(params.OrderID, "OrderID"); err != nil {
		return nil, err
	}
	return one[Wallet](ctx, s.c, call{ep: epWalletCreate, body: params, orderID: params.OrderID})
}

// Get returns a static wallet by its ID. GET /api/v4/wallets/{id}, permission info_payments.
func (s *WalletService) Get(ctx context.Context, id string) (*Wallet, error) {
	id, err := checkUUID(id, "id")
	if err != nil {
		return nil, err
	}
	return one[Wallet](ctx, s.c, call{ep: epWalletGet, params: []string{id}})
}

// FindByOrderID returns nil, nil when no wallet has this order_id.
func (s *WalletService) FindByOrderID(ctx context.Context, orderID string) (*Wallet, error) {
	if err := checkOrderID(orderID, "orderID"); err != nil {
		return nil, err
	}
	return first(s.List(ctx, &WalletQuery{OrderID: orderID, Limit: 1}))
}

// List returns one page of static wallets, newest first. GET /api/v4/wallets, permission
// info_payments.
func (s *WalletService) List(ctx context.Context, query *WalletQuery) (*Page[Wallet], error) {
	return s.list(ctx, query, 0)
}

// All returns every static wallet matching the query, fetching pages lazily.
func (s *WalletService) All(ctx context.Context, query *WalletQuery) iter.Seq2[*Wallet, error] {
	q := WalletQuery{}
	if query != nil {
		q = *query
	}
	return all(ctx, q.Cursor, func(ctx context.Context, cursor string) (*Page[Wallet], error) {
		q.Cursor = cursor
		return s.list(ctx, &q, iterateRetryDelay)
	})
}

func (s *WalletService) list(ctx context.Context, query *WalletQuery, retryDelay time.Duration) (*Page[Wallet], error) {
	qp, err := query.params()
	if err != nil {
		return nil, err
	}
	return page[Wallet](ctx, s.c, call{ep: epWalletList, query: qp, maxRetryDelay: retryDelay})
}

// Block blocks a static wallet. refundIncoming (whether later deposits are refunded) is
// fixed at the first block and cannot be undone; repeating the call with the same value
// returns the wallet unchanged. POST /api/v4/wallets/{id}/block, permission wallet_block.
func (s *WalletService) Block(ctx context.Context, id string, refundIncoming bool) (*Wallet, error) {
	id, err := checkUUID(id, "id")
	if err != nil {
		return nil, err
	}
	body := struct {
		RefundIncoming bool `json:"refund_incoming"`
	}{refundIncoming}
	return one[Wallet](ctx, s.c, call{ep: epWalletBlock, params: []string{id}, body: body})
}

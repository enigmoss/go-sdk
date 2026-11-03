package enigmoss

import (
	"context"
	"iter"
	"time"
)

// PayoutStatus is the status of a payout. The server may add statuses: keep a default
// branch in a switch, and decide with Payout.IsFinal.
type PayoutStatus string

// Payout statuses.
const (
	PayoutPending    PayoutStatus = "pending"
	PayoutProcessing PayoutStatus = "processing"
	// PayoutAwaitingApproval waits for manual approval (a large amount).
	PayoutAwaitingApproval PayoutStatus = "awaiting_approval"
	PayoutCompleted        PayoutStatus = "completed"
	// PayoutFailed: not paid out; the order_id stays used.
	PayoutFailed PayoutStatus = "failed"
	// PayoutCancelled: not paid out; the order_id stays used.
	PayoutCancelled PayoutStatus = "cancelled"
)

// PayoutPriority is the network fee priority of a payout: a higher priority confirms faster
// and costs more.
type PayoutPriority string

// Payout priorities.
const (
	PriorityRecommended PayoutPriority = "recommended"
	PriorityEconomy     PayoutPriority = "economy"
	PriorityHigh        PayoutPriority = "high"
	PriorityHighest     PayoutPriority = "highest"
)

// Payout is a payout to an external address. The same object is the data of payout.*
// webhooks.
type Payout struct {
	ID      string       `json:"id"`
	OrderID string       `json:"order_id"`
	Status  PayoutStatus `json:"status"`
	IsFinal bool         `json:"is_final"`
	// FailureCode tells why the payout failed.
	FailureCode  string         `json:"failure_code"`
	Amount       string         `json:"amount"`
	CurrencyCode string         `json:"currency_code"`
	NetworkCode  string         `json:"network_code"`
	Address      string         `json:"address"`
	Memo         string         `json:"memo"`
	Priority     PayoutPriority `json:"priority"`
	// Recipient is what the recipient address receives.
	Recipient PayoutAmount `json:"recipient"`
	// Charged is what is debited from your balance, fee included.
	Charged    PayoutCharge      `json:"charged"`
	Conversion *PayoutConversion `json:"conversion"`
	TxHash     string            `json:"tx_hash"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

// PayoutQuote is the calculation of a payout without creating it. It is not binding:
// check Payout.Charged after the payout is created.
type PayoutQuote struct {
	Amount       string            `json:"amount"`
	CurrencyCode string            `json:"currency_code"`
	NetworkCode  string            `json:"network_code"`
	Priority     PayoutPriority    `json:"priority"`
	Recipient    PayoutAmount      `json:"recipient"`
	Charged      PayoutCharge      `json:"charged"`
	Conversion   *PayoutConversion `json:"conversion"`
}

// PayoutAmount is an amount in a currency.
type PayoutAmount struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
}

// PayoutCharge is what is debited from your balance and the fee in it.
type PayoutCharge struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
	Fee          string `json:"fee"`
}

// PayoutConversion is the conversion from another balance that funds the payout.
type PayoutConversion struct {
	SourceCurrencyCode string `json:"source_currency_code"`
	SourceAmount       string `json:"source_amount"`
	Fee                string `json:"fee"`
	Rate               string `json:"rate"`
}

// PayoutParams are the parameters of a payout. OrderID, Amount, CurrencyCode and Address
// are required.
type PayoutParams struct {
	// OrderID is your unique payout ID. One order_id is one payout, forever, even a failed one.
	OrderID string `json:"order_id"`
	// Amount is a decimal string, such as "100.00".
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
	// NetworkCode is required when the currency has several networks.
	NetworkCode string `json:"network_code,omitempty"`
	Address     string `json:"address"`
	// Memo is the memo or tag for networks that need it, up to 64 characters.
	Memo string `json:"memo,omitempty"`
	// RecipientCurrencyCode is the currency the recipient gets, when it differs.
	RecipientCurrencyCode string `json:"recipient_currency_code,omitempty"`
	// SourceCurrencyCode is the balance that funds the payout, when it differs.
	SourceCurrencyCode string `json:"source_currency_code,omitempty"`
	// FeeDeductedFromAmount: true (default) takes the fee out of the amount.
	FeeDeductedFromAmount *bool `json:"fee_deducted_from_amount,omitempty"`
	// RateSource is the exchange rate source of a conversion: "Binance", "BinanceP2p",
	// "Exmo", "Garantexio" or "Kucoin". The project setting when empty.
	RateSource string         `json:"rate_source,omitempty"`
	Priority   PayoutPriority `json:"priority,omitempty"`
	// CallbackURL is the public URL for payout.* webhooks, up to 150 characters.
	CallbackURL string `json:"callback_url,omitempty"`
}

// PayoutQuoteParams are the parameters of a payout calculation: the fields of
// PayoutParams without OrderID, Memo and CallbackURL.
type PayoutQuoteParams struct {
	Amount                string         `json:"amount"`
	CurrencyCode          string         `json:"currency_code"`
	NetworkCode           string         `json:"network_code,omitempty"`
	Address               string         `json:"address"`
	RecipientCurrencyCode string         `json:"recipient_currency_code,omitempty"`
	SourceCurrencyCode    string         `json:"source_currency_code,omitempty"`
	FeeDeductedFromAmount *bool          `json:"fee_deducted_from_amount,omitempty"`
	RateSource            string         `json:"rate_source,omitempty"`
	Priority              PayoutPriority `json:"priority,omitempty"`
}

func (p *PayoutParams) check() error {
	if p == nil {
		return errNilParams
	}
	errs := []error{
		checkOrderID(p.OrderID, "OrderID"),
		checkAmount(p.Amount, "Amount"),
		checkLength(p.Address, "Address", 10, 128),
		checkLength(p.Memo, "Memo", 0, 64),
	}
	if p.CallbackURL != "" {
		errs = append(errs, checkLength(p.CallbackURL, "CallbackURL", 6, 150))
	}
	return firstError(errs...)
}

// PayoutQuery filters the payout list. Every field is optional.
type PayoutQuery struct {
	OrderID     string
	Status      PayoutStatus
	CreatedFrom time.Time
	CreatedTo   time.Time
	// Limit is the page size, 1-100 (default 20).
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

func (q *PayoutQuery) params() ([]queryParam, error) {
	if q == nil {
		q = &PayoutQuery{}
	}
	errs := []error{checkLimit(q.Limit, 100)}
	if q.OrderID != "" {
		errs = append(errs, checkOrderID(q.OrderID, "OrderID"))
	}
	return []queryParam{
		{"order_id", q.OrderID},
		{"status", string(q.Status)},
		{"created_from", queryTime(q.CreatedFrom)},
		{"created_to", queryTime(q.CreatedTo)},
		{"limit", queryInt(q.Limit)},
		{"cursor", q.Cursor},
	}, firstError(errs...)
}

var (
	epPayoutCreate = create("/payouts", scopePayout, payoutTimeout, "payouts")
	epPayoutQuote  = calculation("/payouts/quote", scopePayout, "payouts")
	epPayoutGet    = get("/payouts/%s", scopePayout, "payouts")
	epPayoutList   = get("/payouts", scopePayout, "payouts")
)

// PayoutService sends money to external addresses. It needs the payout key, and the
// server IP must be allowed for payout-key requests.
type PayoutService struct{ c *core }

// Create creates a payout; the amount with the fee is debited at once.
//
// Run it in a background worker with a context of at least 10 minutes, not in an HTTP
// handler: the gateway is called synchronously and one attempt may take up to 180
// seconds. A short context makes the outcome unknown, not the payout safer.
//
// Money-safe to repeat: with the same order_id it never pays twice. On an error with
// OutcomeUnknown keep the withdrawal "pending" and reconcile with FindByOrderID; never
// create a new order_id. POST /api/v4/payouts, permission payout_create, 10 requests per
// minute by default.
func (s *PayoutService) Create(ctx context.Context, params *PayoutParams) (*Payout, error) {
	if err := params.check(); err != nil {
		return nil, err
	}
	return one[Payout](ctx, s.c, call{ep: epPayoutCreate, body: params, orderID: params.OrderID})
}

// Quote calculates the fee and the amounts of a payout without creating it.
// POST /api/v4/payouts/quote, permission payout_create.
func (s *PayoutService) Quote(ctx context.Context, params *PayoutQuoteParams) (*PayoutQuote, error) {
	if params == nil {
		return nil, errNilParams
	}
	if err := firstError(checkAmount(params.Amount, "Amount"), checkLength(params.Address, "Address", 10, 128)); err != nil {
		return nil, err
	}
	return one[PayoutQuote](ctx, s.c, call{ep: epPayoutQuote, body: params})
}

// Get returns a payout by its ID. GET /api/v4/payouts/{id}, permission info_payout.
func (s *PayoutService) Get(ctx context.Context, id string) (*Payout, error) {
	id, err := checkUUID(id, "id")
	if err != nil {
		return nil, err
	}
	return one[Payout](ctx, s.c, call{ep: epPayoutGet, params: []string{id}})
}

// FindByOrderID returns nil, nil when no payout has this order_id: the reconciliation
// step after an error with OutcomeUnknown.
func (s *PayoutService) FindByOrderID(ctx context.Context, orderID string) (*Payout, error) {
	if err := checkOrderID(orderID, "orderID"); err != nil {
		return nil, err
	}
	return first(s.List(ctx, &PayoutQuery{OrderID: orderID, Limit: 1}))
}

// List returns one page of payouts, newest first. GET /api/v4/payouts, permission
// info_payouts_list.
func (s *PayoutService) List(ctx context.Context, query *PayoutQuery) (*Page[Payout], error) {
	return s.list(ctx, query, 0)
}

// All returns every payout matching the query, fetching pages lazily.
func (s *PayoutService) All(ctx context.Context, query *PayoutQuery) iter.Seq2[*Payout, error] {
	q := PayoutQuery{}
	if query != nil {
		q = *query
	}
	return all(ctx, q.Cursor, func(ctx context.Context, cursor string) (*Page[Payout], error) {
		q.Cursor = cursor
		return s.list(ctx, &q, iterateRetryDelay)
	})
}

func (s *PayoutService) list(ctx context.Context, query *PayoutQuery, retryDelay time.Duration) (*Page[Payout], error) {
	qp, err := query.params()
	if err != nil {
		return nil, err
	}
	return page[Payout](ctx, s.c, call{ep: epPayoutList, query: qp, maxRetryDelay: retryDelay})
}

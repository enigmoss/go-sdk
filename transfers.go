package enigmoss

import (
	"context"
	"errors"
	"iter"
	"time"
)

// Transfer is a transfer from the business balance to the owner's personal account. It is
// synchronous: a returned transfer means the money has moved.
type Transfer struct {
	ID           string `json:"id"`
	OrderID      string `json:"order_id"`
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
	// TransactionID is the debiting movement in Transactions.
	TransactionID string    `json:"transaction_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// TransferParams are the parameters of a transfer. Every field is required.
type TransferParams struct {
	// OrderID is your unique transfer ID.
	OrderID string `json:"order_id"`
	// Amount is a decimal string with at most 8 decimals, such as "25.00".
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
}

// TransferQuery filters the transfer list. Every field is optional.
type TransferQuery struct {
	OrderID string
	// Limit is the page size, 1-50 (default 20).
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

func (q *TransferQuery) params() ([]queryParam, error) {
	if q == nil {
		q = &TransferQuery{}
	}
	errs := []error{checkLimit(q.Limit, 50)}
	if q.OrderID != "" {
		errs = append(errs, checkOrderID(q.OrderID, "OrderID"))
	}
	return []queryParam{{"order_id", q.OrderID}, {"limit", queryInt(q.Limit)}, {"cursor", q.Cursor}}, firstError(errs...)
}

var (
	epTransferCreate = create("/transfers", scopePayout, attemptTimeout, "transfers")
	epTransferGet    = get("/transfers/%s", scopePayout, "transfers")
	epTransferList   = get("/transfers", scopePayout, "transfers")
)

// TransferService moves money to the personal account. It needs the payout key and the
// permission transfer_to_personal.
type TransferService struct{ c *core }

// Create moves money from the business balance to the personal account, without a fee.
// Money-safe to repeat with the same order_id; on an error with OutcomeUnknown reconcile
// with FindByOrderID. POST /api/v4/transfers.
func (s *TransferService) Create(ctx context.Context, params *TransferParams) (*Transfer, error) {
	if params == nil {
		return nil, errNilParams
	}
	if err := checkOrderID(params.OrderID, "OrderID"); err != nil {
		return nil, err
	}
	if !transferPattern.MatchString(params.Amount) {
		return nil, errors.New("enigmoss: Amount must be a decimal string with at most 11 integer digits and 8 decimals")
	}
	return one[Transfer](ctx, s.c, call{ep: epTransferCreate, body: params, orderID: params.OrderID})
}

// Get returns a transfer by its ID. GET /api/v4/transfers/{id}.
func (s *TransferService) Get(ctx context.Context, id string) (*Transfer, error) {
	id, err := checkUUID(id, "id")
	if err != nil {
		return nil, err
	}
	return one[Transfer](ctx, s.c, call{ep: epTransferGet, params: []string{id}})
}

// FindByOrderID returns nil, nil when no transfer has this order_id.
func (s *TransferService) FindByOrderID(ctx context.Context, orderID string) (*Transfer, error) {
	if err := checkOrderID(orderID, "orderID"); err != nil {
		return nil, err
	}
	return first(s.List(ctx, &TransferQuery{OrderID: orderID, Limit: 1}))
}

// List returns one page of transfers, newest first. GET /api/v4/transfers.
func (s *TransferService) List(ctx context.Context, query *TransferQuery) (*Page[Transfer], error) {
	return s.list(ctx, query, 0)
}

// All returns every transfer matching the query, fetching pages lazily.
func (s *TransferService) All(ctx context.Context, query *TransferQuery) iter.Seq2[*Transfer, error] {
	q := TransferQuery{}
	if query != nil {
		q = *query
	}
	return all(ctx, q.Cursor, func(ctx context.Context, cursor string) (*Page[Transfer], error) {
		q.Cursor = cursor
		return s.list(ctx, &q, iterateRetryDelay)
	})
}

func (s *TransferService) list(ctx context.Context, query *TransferQuery, retryDelay time.Duration) (*Page[Transfer], error) {
	qp, err := query.params()
	if err != nil {
		return nil, err
	}
	return page[Transfer](ctx, s.c, call{ep: epTransferList, query: qp, maxRetryDelay: retryDelay})
}

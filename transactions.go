package enigmoss

import (
	"context"
	"errors"
	"iter"
	"time"
)

// TransactionType is the kind of a balance movement. The server may add types: keep a
// default branch in a switch.
type TransactionType string

// Transaction types.
const (
	TransactionPayment       TransactionType = "payment"
	TransactionPaymentHold   TransactionType = "payment_hold"
	TransactionPayout        TransactionType = "payout"
	TransactionRefund        TransactionType = "refund"
	TransactionTransfer      TransactionType = "transfer"
	TransactionConversionOut TransactionType = "conversion_out"
	TransactionConversionIn  TransactionType = "conversion_in"
	// TransactionOther is a legacy movement: it can be read but not used as a filter.
	TransactionOther TransactionType = "other"
)

// Transaction is one balance movement of the project. Amount is negative for debits. The
// *ID links are set for objects of API v4 only.
type Transaction struct {
	ID           string          `json:"id"`
	Type         TransactionType `json:"type"`
	Amount       string          `json:"amount"`
	Fee          string          `json:"fee"`
	CurrencyCode string          `json:"currency_code"`
	NetworkCode  string          `json:"network_code"`
	PaymentID    string          `json:"payment_id"`
	PayoutID     string          `json:"payout_id"`
	RefundID     string          `json:"refund_id"`
	TransferID   string          `json:"transfer_id"`
	CreatedAt    time.Time       `json:"created_at"`
}

// TransactionQuery filters the transaction list. Every field is optional.
type TransactionQuery struct {
	Type        TransactionType
	CreatedFrom time.Time
	CreatedTo   time.Time
	// Limit is the page size, 1-50 (default 20).
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

func (q *TransactionQuery) params() ([]queryParam, error) {
	if q == nil {
		q = &TransactionQuery{}
	}
	errs := []error{checkLimit(q.Limit, 50)}
	if q.Type == TransactionOther {
		errs = append(errs, errors.New(`enigmoss: Type "other" cannot be used as a filter`))
	}
	return []queryParam{
		{"type", string(q.Type)},
		{"created_from", queryTime(q.CreatedFrom)},
		{"created_to", queryTime(q.CreatedTo)},
		{"limit", queryInt(q.Limit)},
		{"cursor", q.Cursor},
	}, firstError(errs...)
}

var epTransactionList = get("/transactions", scopePayment, "transactions")

// TransactionService lists the balance movements. It needs the payment key.
type TransactionService struct{ c *core }

// List returns one page of balance movements, newest first. GET /api/v4/transactions,
// permission info_transactions_history.
func (s *TransactionService) List(ctx context.Context, query *TransactionQuery) (*Page[Transaction], error) {
	return s.list(ctx, query, 0)
}

// All returns every balance movement matching the query, fetching pages lazily.
func (s *TransactionService) All(ctx context.Context, query *TransactionQuery) iter.Seq2[*Transaction, error] {
	q := TransactionQuery{}
	if query != nil {
		q = *query
	}
	return all(ctx, q.Cursor, func(ctx context.Context, cursor string) (*Page[Transaction], error) {
		q.Cursor = cursor
		return s.list(ctx, &q, iterateRetryDelay)
	})
}

func (s *TransactionService) list(ctx context.Context, query *TransactionQuery, retryDelay time.Duration) (*Page[Transaction], error) {
	qp, err := query.params()
	if err != nil {
		return nil, err
	}
	return page[Transaction](ctx, s.c, call{ep: epTransactionList, query: qp, maxRetryDelay: retryDelay})
}

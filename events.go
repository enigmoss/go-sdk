package enigmoss

import (
	"context"
	"errors"
	"iter"
	"time"
)

// DeliveryStatus is the delivery state of a webhook event. The server may add values: keep
// a default branch in a switch.
type DeliveryStatus string

// Delivery statuses.
const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliveryDelivered DeliveryStatus = "delivered"
	DeliveryFailed    DeliveryStatus = "failed"
	// DeliverySuperseded: a newer event of the same object made this one obsolete.
	DeliverySuperseded DeliveryStatus = "superseded"
	// DeliveryNoDestination: no callback URL was configured.
	DeliveryNoDestination DeliveryStatus = "no_destination"
)

// EventRecord is a webhook event from the events journal (kept for 60 days), with its
// delivery state. Payload is the same Event your endpoint receives, so a missed webhook can
// be replayed through the same handler.
type EventRecord struct {
	ID string `json:"id"`
	// Type is such as "payment.paid".
	Type string `json:"event"`
	// ObjectType is "payment", "wallet", "payout" or "refund".
	ObjectType string `json:"object_type"`
	ObjectID   string `json:"object_id"`
	// DeliveryStatus is the status of the latest delivery attempt.
	DeliveryStatus DeliveryStatus `json:"delivery_status"`
	CreatedAt      time.Time      `json:"created_at"`
	Payload        Event          `json:"payload"`
	// Attempts are filled by Get, Redeliver and SendTest; empty in lists.
	Attempts []DeliveryAttempt `json:"attempts"`
}

// DeliveryAttempt is one delivery attempt of a webhook event, with your endpoint's response.
type DeliveryAttempt struct {
	Number int `json:"number"`
	// Trigger is "auto", "retry", "proxy_retry", "manual" or "bulk".
	Trigger            string         `json:"trigger"`
	Status             DeliveryStatus `json:"status"`
	URL                string         `json:"url"`
	ResponseStatusCode int            `json:"response_status_code"`
	// ResponseBody is the first 64000 bytes of your endpoint's response.
	ResponseBody string    `json:"response_body"`
	ScheduledAt  time.Time `json:"scheduled_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// EventQuery filters the events journal. Every field is optional.
type EventQuery struct {
	// Event is such as "payment.paid".
	Event string
	// ObjectType is "payment", "wallet", "payout" or "refund".
	ObjectType     string
	ObjectID       string
	DeliveryStatus DeliveryStatus
	CreatedFrom    time.Time
	CreatedTo      time.Time
	// Limit is the page size, 1-100 (default 20).
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

func (q *EventQuery) params() ([]queryParam, error) {
	if q == nil {
		q = &EventQuery{}
	}
	objectID := q.ObjectID
	errs := []error{checkLimit(q.Limit, 100), checkEvent(q.Event, "Event")}
	if objectID != "" {
		var err error
		objectID, err = checkUUID(objectID, "ObjectID")
		errs = append(errs, err)
	}
	return []queryParam{
		{"event", q.Event},
		{"object_type", q.ObjectType},
		{"object_id", objectID},
		{"delivery_status", string(q.DeliveryStatus)},
		{"created_from", queryTime(q.CreatedFrom)},
		{"created_to", queryTime(q.CreatedTo)},
		{"limit", queryInt(q.Limit)},
		{"cursor", q.Cursor},
	}, firstError(errs...)
}

// BulkRedeliverParams select the events to redeliver: the latest event of each matching
// object, at most 1000. CreatedFrom is required and not older than 60 days.
type BulkRedeliverParams struct {
	CreatedFrom time.Time
	CreatedTo   time.Time
	// Event is such as "payment.paid".
	Event string
	// ObjectType is "payment", "wallet", "payout" or "refund".
	ObjectType     string
	ObjectID       string
	DeliveryStatus DeliveryStatus
}

// BulkRedeliveryResult counts the events queued by RedeliverBulk.
type BulkRedeliveryResult struct {
	Queued         int `json:"queued"`
	AlreadyPending int `json:"already_pending"`
	// SkippedOutdated counts older events of objects that have a newer one.
	SkippedOutdated int `json:"skipped_outdated"`
}

var (
	epEventList       = get("/events", scopePayment, "webhook events")
	epEventGet        = get("/events/%s", scopePayment, "webhook events")
	epEventRedeliver  = unsafePost("/events/%s/redeliver", scopePayment, "webhook events")
	epEventBulkResend = unsafePost("/events/redeliver", scopePayment, "webhook events")
)

// EventService reads and redelivers webhook events. It needs the payment key and the
// permission webhooks.
type EventService struct{ c *core }

// List returns one page of events, newest first. GET /api/v4/events.
func (s *EventService) List(ctx context.Context, query *EventQuery) (*Page[EventRecord], error) {
	return s.list(ctx, query, 0)
}

// All returns every event matching the query, fetching pages lazily. Replay missed
// webhooks with it:
//
//	for rec, err := range c.Events.All(ctx, &enigmoss.EventQuery{DeliveryStatus: enigmoss.DeliveryFailed}) { ... handle(rec.Payload) }
func (s *EventService) All(ctx context.Context, query *EventQuery) iter.Seq2[*EventRecord, error] {
	q := EventQuery{}
	if query != nil {
		q = *query
	}
	return all(ctx, q.Cursor, func(ctx context.Context, cursor string) (*Page[EventRecord], error) {
		q.Cursor = cursor
		return s.list(ctx, &q, iterateRetryDelay)
	})
}

func (s *EventService) list(ctx context.Context, query *EventQuery, retryDelay time.Duration) (*Page[EventRecord], error) {
	qp, err := query.params()
	if err != nil {
		return nil, err
	}
	return page[EventRecord](ctx, s.c, call{ep: epEventList, query: qp, maxRetryDelay: retryDelay})
}

// Get returns an event with all its delivery attempts. GET /api/v4/events/{id}.
func (s *EventService) Get(ctx context.Context, id string) (*EventRecord, error) {
	id, err := checkUUID(id, "id")
	if err != nil {
		return nil, err
	}
	return one[EventRecord](ctx, s.c, call{ep: epEventGet, params: []string{id}})
}

// Redeliver sends an event again, once, to the current callback URL. Only the latest event
// of an object can be redelivered, at most 10 times. POST /api/v4/events/{id}/redeliver.
func (s *EventService) Redeliver(ctx context.Context, id string) (*EventRecord, error) {
	id, err := checkUUID(id, "id")
	if err != nil {
		return nil, err
	}
	return one[EventRecord](ctx, s.c, call{ep: epEventRedeliver, params: []string{id}})
}

// RedeliverBulk queues the latest event of every matching object for redelivery, once per
// minute. POST /api/v4/events/redeliver.
func (s *EventService) RedeliverBulk(ctx context.Context, params *BulkRedeliverParams) (*BulkRedeliveryResult, error) {
	if params == nil {
		return nil, errNilParams
	}
	if params.CreatedFrom.IsZero() {
		return nil, errors.New("enigmoss: CreatedFrom is required")
	}
	objectID := params.ObjectID
	errs := []error{checkEvent(params.Event, "Event")}
	if objectID != "" {
		var err error
		objectID, err = checkUUID(objectID, "ObjectID")
		errs = append(errs, err)
	}
	if err := firstError(errs...); err != nil {
		return nil, err
	}
	body := struct {
		CreatedFrom    string `json:"created_from"`
		CreatedTo      string `json:"created_to,omitempty"`
		Event          string `json:"event,omitempty"`
		ObjectType     string `json:"object_type,omitempty"`
		ObjectID       string `json:"object_id,omitempty"`
		DeliveryStatus string `json:"delivery_status,omitempty"`
	}{queryTime(params.CreatedFrom), queryTime(params.CreatedTo), params.Event, params.ObjectType, objectID, string(params.DeliveryStatus)}
	return one[BulkRedeliveryResult](ctx, s.c, call{ep: epEventBulkResend, body: body})
}

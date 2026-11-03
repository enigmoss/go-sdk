package enigmoss

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	webhookTolerance = 300 // seconds
	maxWebhookBytes  = 1 << 20
)

var timestampPattern = regexp.MustCompile(`^\d{1,10}$`)

// Event is a webhook event: {"event", "event_id", "created_at", "data"}. The same Event is
// the Payload of an EventRecord in the events journal.
//
// Deduplicate by ID: retries and redeliveries carry the same ID. Events may arrive out of
// order; compare CreatedAt or get the object when the order matters.
type Event struct {
	// ID is stable across retries and redeliveries.
	ID string `json:"event_id"`
	// Type is such as "payment.paid", "payout.completed", "refund.failed" or "webhook.test".
	// New types may appear: acknowledge unknown ones with 200.
	Type      string    `json:"event"`
	CreatedAt time.Time `json:"created_at"`
	// Data is the object of the event exactly as received; null for webhook.test.
	Data json.RawMessage `json:"data"`
}

// Payment returns the payment of a payment.* event.
func (e *Event) Payment() (*Payment, error) {
	p := new(Payment)
	return p, e.object("payment", p)
}

// Payout returns the payout of a payout.* event.
func (e *Event) Payout() (*Payout, error) {
	p := new(Payout)
	return p, e.object("payout", p)
}

// Refund returns the refund of a refund.* event.
func (e *Event) Refund() (*Refund, error) {
	r := new(Refund)
	return r, e.object("refund", r)
}

func (e *Event) object(kind string, out any) error {
	if !strings.HasPrefix(e.Type, kind+".") {
		return fmt.Errorf("enigmoss: event %q does not carry a %s", e.Type, kind)
	}
	data := bytes.TrimSpace(e.Data)
	if len(data) == 0 || string(data) == "null" {
		return fmt.Errorf("enigmoss: event %q has no data", e.Type)
	}
	if err := decodeJSON(data, out); err != nil {
		return fmt.Errorf("enigmoss: the %s of event %q does not match the API: %w", kind, e.Type, err)
	}
	return nil
}

// ParseWebhook verifies an incoming webhook and returns its event. Pass the payment key:
// every webhook is signed with it, payout.* and refund.* included. During a key rotation
// pass the new and the old key.
//
// It reads the raw body (at most 1 MiB) and checks, in order, the X-Project-Uuid,
// X-Timestamp and X-Signature headers, the project, a timestamp within 300 seconds and
// the HMAC-SHA256 signature. On an error answer 400 and do not process the request.
//
// After a valid signature an unknown event type is not an error: acknowledge it with 200.
func ParseWebhook(r *http.Request, projectID string, keys ...string) (*Event, error) {
	if r == nil || r.Body == nil {
		return nil, errors.New("enigmoss: the webhook request has no body")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes+1))
	if err != nil {
		return nil, fmt.Errorf("enigmoss: cannot read the webhook body: %w", err)
	}
	if len(body) > maxWebhookBytes {
		return nil, errors.New("enigmoss: the webhook body is larger than 1 MiB")
	}
	if err := verifyWebhook(body, r.Header, projectID, keys, time.Now()); err != nil {
		return nil, err
	}
	return parseEvent(body)
}

// SignWebhook returns the headers of a webhook body signed now with key, as the API sends
// them. Use it in tests of your webhook endpoint, or to POST a signed body to your local
// endpoint without a tunnel:
//
//	req := httptest.NewRequest("POST", "/webhooks", bytes.NewReader(body))
//	req.Header = enigmoss.SignWebhook(body, projectID, paymentKey)
func SignWebhook(body []byte, projectID, key string) http.Header {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Project-Uuid", projectID)
	h.Set("X-Timestamp", timestamp)
	h.Set("X-Signature", signWebhook(key, timestamp, projectID, body))
	return h
}

// signWebhook is hex(HMAC-SHA256(payment key, "{ts}\n{project}\n{raw body}")).
func signWebhook(key, timestamp, projectID string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + "\n" + projectID + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyWebhook(body []byte, header http.Header, projectID string, keys []string, now time.Time) error {
	project := strings.TrimSpace(header.Get("X-Project-Uuid"))
	timestamp := strings.TrimSpace(header.Get("X-Timestamp"))
	var signatures []string
	for _, s := range header.Values("X-Signature") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			signatures = append(signatures, s)
		}
	}

	var missing []string
	if project == "" {
		missing = append(missing, "X-Project-Uuid")
	}
	if timestamp == "" {
		missing = append(missing, "X-Timestamp")
	}
	if len(signatures) == 0 {
		missing = append(missing, "X-Signature")
	}
	if len(missing) > 0 {
		return fmt.Errorf("enigmoss: the webhook has no %s header: make sure the web server or proxy passes custom headers through", strings.Join(missing, ", "))
	}
	if !strings.EqualFold(project, strings.TrimSpace(projectID)) {
		return errors.New("enigmoss: the webhook is addressed to another project")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if !timestampPattern.MatchString(timestamp) || err != nil || abs(now.Unix()-ts) > webhookTolerance {
		return errors.New("enigmoss: the webhook timestamp is outside the accepted window of 300 seconds")
	}

	var usable bool
	for _, key := range keys {
		if key == "" {
			continue
		}
		usable = true
		// The signature covers the project exactly as sent.
		expected := []byte(signWebhook(key, timestamp, project, body))
		for _, s := range signatures {
			if hmac.Equal(expected, []byte(s)) {
				return nil
			}
		}
	}
	if !usable {
		return errors.New("enigmoss: ParseWebhook needs the payment key")
	}
	return errors.New("enigmoss: the webhook signature does not match: verify the raw request body with the payment key of this project (payout.* and refund.* webhooks are signed with the payment key too)")
}

// parseEvent reads the envelope leniently: after a valid signature the content of the event
// never makes the webhook fail, except a body that is not a JSON object.
func parseEvent(body []byte) (*Event, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return nil, errors.New("enigmoss: the webhook body is not a JSON object")
	}
	ev := &Event{Data: envelope["data"]}
	_ = json.Unmarshal(envelope["event_id"], &ev.ID)
	_ = json.Unmarshal(envelope["event"], &ev.Type)
	var created string
	if json.Unmarshal(envelope["created_at"], &created) == nil {
		if t, err := time.Parse(time.RFC3339, created); err == nil {
			ev.CreatedAt = t.UTC()
		}
	}
	return ev, nil
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

var epWebhookTest = unsafePost("/webhooks/test", scopePayment, "test webhooks")

// WebhookService sends test webhooks. It needs the payment key and the permission webhooks.
type WebhookService struct{ c *core }

// SendTest queues a webhook.test event to callbackURL or, when it is "", to the default URL
// of the project. It is sent once, without retries. The returned event usually has its first
// attempt still pending: read the delivery status and your endpoint's response later with
// Events.Get. POST /api/v4/webhooks/test, 10 per minute.
func (s *WebhookService) SendTest(ctx context.Context, callbackURL string) (*EventRecord, error) {
	var body any
	if callbackURL != "" {
		body = struct {
			CallbackURL string `json:"callback_url"`
		}{callbackURL}
	}
	return one[EventRecord](ctx, s.c, call{ep: epWebhookTest, body: body})
}

package enigmoss

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const (
	testProject    = "3f2b8c1e-5d4a-4e7b-9c6f-1a2b3c4d5e6f"
	testPaymentKey = "test-payment-secret"
	testPayoutKey  = "test-payout-secret"
)

// recorded is one request seen by the fake API.
type recorded struct {
	Method     string
	RequestURI string
	Path       string
	Header     http.Header
	Body       []byte
	Key        string // "payment", "payout" or "" when the signature matched neither key
	Proto      int
}

// fakeAPI is an HTTPS server that verifies signatures and answers with a handler.
type fakeAPI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
	handler  func(w http.ResponseWriter, r *recorded)
}

func newFakeAPI(t *testing.T, handler func(w http.ResponseWriter, r *recorded)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{handler: handler}
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.EnableHTTP2 = true
	f.StartTLS()
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec := recorded{Method: r.Method, RequestURI: r.RequestURI, Path: r.URL.Path, Header: r.Header.Clone(), Body: body, Proto: r.ProtoMajor}
	ts, project, sig := r.Header.Get("X-Timestamp"), r.Header.Get("X-Project-Uuid"), r.Header.Get("X-Signature")
	switch sig {
	case signRequest(testPaymentKey, ts, project, r.Method, r.RequestURI, body):
		rec.Key = "payment"
	case signRequest(testPayoutKey, ts, project, r.Method, r.RequestURI, body):
		rec.Key = "payout"
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	f.mu.Unlock()
	f.handler(w, &rec)
}

func (f *fakeAPI) recorded() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

// newTestClient returns a client of the fake API with both keys, instant retries and a
// fixed jitter.
func newTestClient(t *testing.T, f *fakeAPI, opts ...Option) (*Client, *[]time.Duration) {
	t.Helper()
	if len(opts) == 0 {
		opts = []Option{WithPaymentKey(testPaymentKey), WithPayoutKey(testPayoutKey)}
	}
	opts = append(opts, WithHTTPClient(f.Client()))
	c, err := NewClient(f.URL, testProject, opts...)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	delays := &[]time.Duration{}
	c.c.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*delays = append(*delays, d)
		mu.Unlock()
		return ctx.Err()
	}
	c.c.random = func() float64 { return 0.5 }
	return c, delays
}

// reply writes an API success envelope.
func reply(w http.ResponseWriter, status int, data string, meta string) {
	if meta == "" {
		meta = `{"request_id":"req-1"}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"data":`+data+`,"meta":`+meta+`}`)
}

// replyError writes an API error envelope.
func replyError(w http.ResponseWriter, status int, code string, retryable bool, details string) {
	if details == "" {
		details = "{}"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	r, _ := json.Marshal(retryable)
	_, _ = io.WriteString(w, `{"error":{"type":"x","code":"`+code+`","message":"Server says `+code+`","retryable":`+string(r)+`,"details":`+details+`},"meta":{"request_id":"req-err"}}`)
}

// Realistic API payloads in the wire format.
const (
	fxProject = `{"id":"3f2b8c1e-5d4a-4e7b-9c6f-1a2b3c4d5e6f","name":"Example Shop","key_type":"payment","permissions":["payment_create_invoice","info_payments","webhooks"],"server_time":1767225600}`
	fxPayment = `{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","order_id":"order-42","wallet_id":null,"status":"pending","is_final":false,"failure_code":null,"amount":"10.50","currency_code":"USDT",` +
		`"payer":{"amount":null,"currency_code":null,"network_code":null,"address":null,"sender_address":null,"exchange_rate":null},"paid":{"amount":null,"amount_usd":null},` +
		`"settlement":{"amount":null,"currency_code":null,"fee":null},"conversion":null,"discount":{"percent":null,"amount":null},"tx_hash":null,` +
		`"payment_url":"https://pay.example.com/invoice/7c9e6679-7425-40de-944b-e07fc1f90ae7?theme=system","expires_at":"2026-01-01T01:00:00Z","created_at":"2026-01-01T00:00:00Z",` +
		`"updated_at":"2026-01-01T00:00:00Z","paid_at":null,"description":null,"metadata":{}}`
	fxPaidPayment = `{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","order_id":"order-42","wallet_id":null,"status":"paid","is_final":true,"failure_code":null,"amount":"10.50","currency_code":"USDT",` +
		`"payer":{"amount":"10.50","currency_code":"USDT","network_code":"tron","address":"TXYZabcdefghijklmnopqrstuvwxyz1234","sender_address":"TSenderabcdefghijklmnopqrstuvwxyz12","exchange_rate":"1"},` +
		`"paid":{"amount":"10.50","amount_usd":"10.49"},"settlement":{"amount":"10.29","currency_code":"USDT","fee":"0.21"},` +
		`"conversion":{"from":{"amount":"10.50","currency_code":"USDT","network_code":"tron"},"to":{"currency_code":"USDC"},"rate":"0.999"},"discount":{"percent":5,"amount":"0.50"},"tx_hash":"0x7f3a9c",` +
		`"payment_url":"https://pay.example.com/invoice/7c9e6679-7425-40de-944b-e07fc1f90ae7?theme=system","expires_at":"2026-01-01T01:00:00Z","created_at":"2026-01-01T03:00:00+03:00",` +
		`"updated_at":"2026-01-01T00:05:00Z","paid_at":"2026-01-01T00:05:00Z","description":"Order 42","metadata":{"cart":"42"}}`
	fxWallet = `{"id":"0f8fad5b-d9cb-469f-a165-70867728950e","order_id":"customer-7","currency_code":"USDT","network_code":"tron","address":"TXYZabcdefghijklmnopqrstuvwxyz1234","status":"active",` +
		`"refund_incoming":false,"callback_url":null,"payment_url":"https://pay.example.com/static/0f8fad5b-d9cb-469f-a165-70867728950e","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`
	fxRefund = `{"id":"01970000-0000-7000-8000-000000000001","order_id":"refund-42","source":"payment","payment_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","wallet_id":null,"status":"pending",` +
		`"is_final":false,"failure_code":null,"amount":"10.50","currency_code":"USDT","network_code":"tron","address":"TCustomerabcdefghijklmnopqrstuvwxy","tx_hash":null,` +
		`"created_at":"2026-01-02T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","completed_at":null}`
	fxPayout = `{"id":"9b2d4f6a-1c3e-4a5b-8d7f-0e1a2b3c4d5e","order_id":"withdrawal-42","status":"pending","is_final":false,"failure_code":null,"amount":"100.00","currency_code":"USDT",` +
		`"network_code":"tron","address":"TRecipientabcdefghijklmnopqrstuvwx","memo":null,"priority":"recommended","recipient":{"amount":"99.00","currency_code":"USDT"},` +
		`"charged":{"amount":"100.00","currency_code":"USDT","fee":"1.00"},"conversion":null,"tx_hash":null,"created_at":"2026-01-03T00:00:00Z","updated_at":"2026-01-03T00:00:00Z"}`
	fxPayoutQuote = `{"amount":"100.00","currency_code":"USDT","network_code":"tron","priority":"recommended","recipient":{"amount":"99.00","currency_code":"USDT"},` +
		`"charged":{"amount":"100.00","currency_code":"USDT","fee":"1.00"},"conversion":{"source_currency_code":"USDC","source_amount":"100.10","fee":"0.10","rate":"1.001"}}`
	fxTransfer    = `{"id":"01970000-0000-7000-8000-000000000002","order_id":"transfer-1","amount":"25.00","currency_code":"USDT","transaction_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","created_at":"2026-01-04T00:00:00Z"}`
	fxTransaction = `{"id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","type":"payout","amount":"-100.00","fee":"1.00","currency_code":"USDT","network_code":"tron","payment_id":null,` +
		`"payout_id":"9b2d4f6a-1c3e-4a5b-8d7f-0e1a2b3c4d5e","refund_id":null,"transfer_id":null,"created_at":"2026-01-03T00:00:00Z"}`
	fxBalance             = `{"currency_code":"USDT","balance":"490.50","balance_usd":"490.12"}`
	fxPaymentMethod       = `{"currency_code":"USDT","network_code":"tron","is_enabled":true,"discount_percent":5}`
	fxPayoutMethod        = `{"currency_code":"USDT","network_code":"tron","contract_address":"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t","min_amount":"1.00","max_amount":"10000.00","fee":{"amount":"1.00","percent":"0.5"}}`
	fxPayoutMethodDetails = `{"currency_code":"USDT","network_code":"tron","network_name":"Tron","token_standard":"TRC-20","min_amount":"1.00","max_amount":"10000.00","decimals":6,"is_memo_available":false,` +
		`"priorities":[{"priority":"economy","fee":"0.80"},{"priority":"recommended","fee":"1.00"}]}`
	fxExchangeRate = `{"from":"USDT","to":"USD","rate":"0.999800000000000000"}`
	fxAMLLink      = `{"url":"https://aml.example.com/verify/1","status":"init","expires_at":"2026-01-05T00:00:00Z"}`
	fxEventRecord  = `{"id":"019b7710-0000-7000-8000-000000000001","event":"payment.paid","object_type":"payment","object_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","delivery_status":"delivered",` +
		`"created_at":"2026-01-01T00:05:00Z","payload":{"event":"payment.paid","event_id":"019b7710-0000-7000-8000-000000000001","created_at":"2026-01-01T00:05:00Z","data":` + fxPaidPayment + `},` +
		`"attempts":[{"number":1,"trigger":"auto","status":"delivered","url":"https://shop.example/webhooks","response_status_code":200,"response_body":"ok","scheduled_at":null,"created_at":"2026-01-01T00:05:00Z","updated_at":null}]}`
	fxBulkResult = `{"queued":3,"already_pending":1,"skipped_outdated":2}`
)

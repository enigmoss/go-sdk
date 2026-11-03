package enigmoss

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	paymentID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	walletID  = "0f8fad5b-d9cb-469f-a165-70867728950e"
)

// operation is one API operation as the SDK calls it.
type operation struct {
	name     string // "METHOD /path" with {id} placeholders, as in openapi-v4.json
	call     func(ctx context.Context, c *Client) (any, error)
	uri      string // the request URI after /api/v4
	key      string // the key that must sign it
	body     string // the exact body, "" for none
	idem     string // "derived", "random" or ""
	timeout  time.Duration
	data     string // the response data
	response any    // the model type of one item, for the field check
}

func list(item string) string { return "[" + item + "]" }

var cursorMeta = `{"request_id":"req-1","cursor":{"next":null,"prev":null,"per_page":20,"has_more":false}}`

func operations() []operation {
	return []operation{
		{name: "GET /project", uri: "/project", key: "payment", data: fxProject, response: Project{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.Project.Get(ctx) }},

		{name: "POST /payments", uri: "/payments", key: "payment", idem: "derived", timeout: 30 * time.Second, data: fxPayment, response: Payment{},
			body: `{"amount":"10.50","currency_code":"USDT","order_id":"order-42","callback_url":"https://shop.example/webhooks?a=1&b=<2>","customer_fee_percent":0,"discount_percent":0,"allowed_methods":[{"currency_code":"USDT","network_code":"tron"},{"currency_code":"BTC"}],"metadata":{"cart":"№42"},"description":"Café — 42"}`,
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Payments.Create(ctx, &PaymentParams{
					Amount: "10.50", CurrencyCode: "USDT", OrderID: "order-42", CallbackURL: "https://shop.example/webhooks?a=1&b=<2>",
					CustomerFeePercent: Int(0), DiscountPercent: Int(0), Metadata: map[string]any{"cart": "№42"}, Description: "Café — 42",
					AllowedMethods: []PaymentMethodRef{{CurrencyCode: "USDT", NetworkCode: "tron"}, {CurrencyCode: "BTC"}},
				})
			}},
		{name: "GET /payments/{id}", uri: "/payments/" + paymentID, key: "payment", data: fxPayment, response: Payment{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Payments.Get(ctx, strings.ToUpper(paymentID))
			}},
		{name: "GET /payments", uri: "/payments?wallet_id=" + walletID + "&status=paid&created_from=2026-01-01T00%3A00%3A00Z&limit=50&cursor=abc%2B%2F%3D", key: "payment", data: list(fxPayment), response: Payment{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Payments.List(ctx, &PaymentQuery{WalletID: walletID, Status: PaymentPaid, CreatedFrom: time.Date(2026, 1, 1, 3, 0, 0, 0, time.FixedZone("", 3*3600)), Limit: 50, Cursor: "abc+/="})
			}},
		{name: "POST /payments/{id}/cancel", uri: "/payments/" + paymentID + "/cancel", key: "payment", body: "{}", idem: "random", data: fxPayment, response: Payment{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.Payments.Cancel(ctx, paymentID) }},
		{name: "POST /payments/{id}/mark-paid", uri: "/payments/" + paymentID + "/mark-paid", key: "payment", body: "{}", idem: "random", data: fxPaidPayment, response: Payment{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.Payments.MarkPaid(ctx, paymentID) }},
		{name: "POST /payments/{id}/aml-verification-links", uri: "/payments/" + paymentID + "/aml-verification-links", key: "payment", body: "{}", idem: "random", data: list(fxAMLLink), response: AMLVerificationLink{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Payments.CreateAMLVerificationLinks(ctx, paymentID)
			}},

		{name: "POST /wallets", uri: "/wallets", key: "payment", idem: "derived", data: fxWallet, response: Wallet{},
			body: `{"currency_code":"USDT","network_code":"tron","order_id":"customer-7"}`,
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Wallets.Create(ctx, &WalletParams{CurrencyCode: "USDT", NetworkCode: "tron", OrderID: "customer-7"})
			}},
		{name: "GET /wallets/{id}", uri: "/wallets/" + walletID, key: "payment", data: fxWallet, response: Wallet{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.Wallets.Get(ctx, walletID) }},
		{name: "GET /wallets", uri: "/wallets?order_id=customer-7&limit=1", key: "payment", data: list(fxWallet), response: Wallet{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.Wallets.FindByOrderID(ctx, "customer-7") }},
		{name: "POST /wallets/{id}/block", uri: "/wallets/" + walletID + "/block", key: "payment", body: `{"refund_incoming":false}`, idem: "random", data: fxWallet, response: Wallet{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.Wallets.Block(ctx, walletID, false) }},

		{name: "POST /payments/{id}/refunds", uri: "/payments/" + paymentID + "/refunds", key: "payout", idem: "derived", timeout: 180 * time.Second, data: fxRefund, response: Refund{},
			body: `{"order_id":"refund-42","address":"TCustomerabcdefghijklmnopqrstuvwxy","amount":"5.00","fee_deducted_from_amount":false}`,
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Refunds.CreateForPayment(ctx, paymentID, &PaymentRefundParams{OrderID: "refund-42", Address: "TCustomerabcdefghijklmnopqrstuvwxy", Amount: "5.00", FeeDeductedFromAmount: Bool(false)})
			}},
		{name: "POST /wallets/{id}/refunds", uri: "/wallets/" + walletID + "/refunds", key: "payout", idem: "derived", timeout: 180 * time.Second, data: fxRefund, response: Refund{},
			body: `{"order_id":"refund-wallet-7","address":"TCustomerabcdefghijklmnopqrstuvwxy"}`,
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Refunds.CreateForWallet(ctx, walletID, &WalletRefundParams{OrderID: "refund-wallet-7", Address: "TCustomerabcdefghijklmnopqrstuvwxy"})
			}},
		{name: "GET /refunds/{id}", uri: "/refunds/01970000-0000-7000-8000-000000000001", key: "payout", data: fxRefund, response: Refund{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Refunds.Get(ctx, "01970000-0000-7000-8000-000000000001")
			}},
		{name: "GET /refunds", uri: "/refunds?source=payment&payment_id=" + paymentID + "&limit=100", key: "payout", data: list(fxRefund), response: Refund{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Refunds.List(ctx, &RefundQuery{Source: "payment", PaymentID: paymentID, Limit: 100})
			}},

		{name: "POST /payouts/quote", uri: "/payouts/quote", key: "payout", data: fxPayoutQuote, response: PayoutQuote{},
			body: `{"amount":"100.00","currency_code":"USDT","network_code":"tron","address":"TRecipientabcdefghijklmnopqrstuvwx","priority":"economy"}`,
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Payouts.Quote(ctx, &PayoutQuoteParams{Amount: "100.00", CurrencyCode: "USDT", NetworkCode: "tron", Address: "TRecipientabcdefghijklmnopqrstuvwx", Priority: PriorityEconomy})
			}},
		{name: "POST /payouts", uri: "/payouts", key: "payout", idem: "derived", timeout: 180 * time.Second, data: fxPayout, response: Payout{},
			body: `{"order_id":"withdrawal-42","amount":"100.00","currency_code":"USDT","network_code":"tron","address":"TRecipientabcdefghijklmnopqrstuvwx","fee_deducted_from_amount":true,"rate_source":"Binance","priority":"high","callback_url":"https://shop.example/hooks"}`,
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Payouts.Create(ctx, &PayoutParams{OrderID: "withdrawal-42", Amount: "100.00", CurrencyCode: "USDT", NetworkCode: "tron", Address: "TRecipientabcdefghijklmnopqrstuvwx",
					FeeDeductedFromAmount: Bool(true), RateSource: "Binance", Priority: PriorityHigh, CallbackURL: "https://shop.example/hooks"})
			}},
		{name: "GET /payouts/{id}", uri: "/payouts/9b2d4f6a-1c3e-4a5b-8d7f-0e1a2b3c4d5e", key: "payout", data: fxPayout, response: Payout{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Payouts.Get(ctx, "9b2d4f6a-1c3e-4a5b-8d7f-0e1a2b3c4d5e")
			}},
		{name: "GET /payouts", uri: "/payouts?status=failed&limit=10", key: "payout", data: list(fxPayout), response: Payout{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Payouts.List(ctx, &PayoutQuery{Status: PayoutFailed, Limit: 10})
			}},

		{name: "POST /transfers", uri: "/transfers", key: "payout", idem: "derived", timeout: 30 * time.Second, data: fxTransfer, response: Transfer{},
			body: `{"order_id":"transfer-1","amount":"25.00","currency_code":"USDT"}`,
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Transfers.Create(ctx, &TransferParams{OrderID: "transfer-1", Amount: "25.00", CurrencyCode: "USDT"})
			}},
		{name: "GET /transfers/{id}", uri: "/transfers/01970000-0000-7000-8000-000000000002", key: "payout", data: fxTransfer, response: Transfer{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Transfers.Get(ctx, "01970000-0000-7000-8000-000000000002")
			}},
		{name: "GET /transfers", uri: "/transfers?order_id=transfer-1&limit=1", key: "payout", data: list(fxTransfer), response: Transfer{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.Transfers.FindByOrderID(ctx, "transfer-1") }},

		{name: "GET /balances", uri: "/balances", key: "payment", data: list(fxBalance), response: Balance{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.Balances.List(ctx) }},
		{name: "GET /transactions", uri: "/transactions?type=payout&created_to=2026-02-01T00%3A00%3A00Z", key: "payment", data: list(fxTransaction), response: Transaction{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Transactions.List(ctx, &TransactionQuery{Type: TransactionPayout, CreatedTo: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)})
			}},
		{name: "GET /payment-methods", uri: "/payment-methods", key: "payment", data: list(fxPaymentMethod), response: PaymentMethod{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.PaymentMethods.List(ctx) }},
		{name: "PATCH /payment-methods/{id}/{id}", uri: "/payment-methods/USDT/tron%20main", key: "payment", body: `{"discount_percent":-5}`, data: fxPaymentMethod, response: PaymentMethod{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.PaymentMethods.UpdateDiscount(ctx, "USDT", "tron main", -5)
			}},
		{name: "GET /payout-methods", uri: "/payout-methods", key: "payment", data: list(fxPayoutMethod), response: PayoutMethod{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.PayoutMethods.List(ctx) }},
		{name: "GET /payout-methods/{id}/{id}", uri: "/payout-methods/USDT/tron", key: "payment", data: fxPayoutMethodDetails, response: PayoutMethodDetails{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.PayoutMethods.Get(ctx, "USDT", "tron") }},
		{name: "GET /exchange-rates", uri: "/exchange-rates?from=USDT", key: "payment", data: list(fxExchangeRate), response: ExchangeRate{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.ExchangeRates.List(ctx, "USDT") }},

		{name: "GET /events", uri: "/events?event=payment.paid&delivery_status=failed&limit=100", key: "payment", data: list(fxEventRecord), response: EventRecord{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Events.List(ctx, &EventQuery{Event: "payment.paid", DeliveryStatus: DeliveryFailed, Limit: 100})
			}},
		{name: "GET /events/{id}", uri: "/events/019b7710-0000-7000-8000-000000000001", key: "payment", data: fxEventRecord, response: EventRecord{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Events.Get(ctx, "019b7710-0000-7000-8000-000000000001")
			}},
		{name: "POST /events/{id}/redeliver", uri: "/events/019b7710-0000-7000-8000-000000000001/redeliver", key: "payment", body: "{}", data: fxEventRecord, response: EventRecord{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Events.Redeliver(ctx, "019b7710-0000-7000-8000-000000000001")
			}},
		{name: "POST /events/redeliver", uri: "/events/redeliver", key: "payment", data: fxBulkResult, response: BulkRedeliveryResult{},
			body: `{"created_from":"2026-01-01T00:00:00Z","object_type":"payout","delivery_status":"failed"}`,
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Events.RedeliverBulk(ctx, &BulkRedeliverParams{CreatedFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ObjectType: "payout", DeliveryStatus: DeliveryFailed})
			}},
		{name: "POST /webhooks/test", uri: "/webhooks/test", key: "payment", body: `{"callback_url":"https://shop.example/webhooks"}`, data: fxEventRecord, response: EventRecord{},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Webhooks.SendTest(ctx, "https://shop.example/webhooks")
			}},
	}
}

var uuid4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestOperations calls every operation against a fake API and checks the method, the
// request URI, the key, the body, the Idempotency-Key, the timeout and the decoded response.
func TestOperations(t *testing.T) {
	for _, op := range operations() {
		t.Run(op.name, func(t *testing.T) {
			meta := ""
			if strings.HasPrefix(op.data, "[") && op.name != "GET /balances" && !strings.Contains(op.name, "methods") &&
				op.name != "GET /exchange-rates" && !strings.Contains(op.name, "aml") {
				meta = cursorMeta
			}
			var deadline time.Duration
			f := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) { reply(w, 200, op.data, meta) })
			c, _ := newTestClient(t, f)
			inner := c.c.http.Transport
			if inner == nil {
				inner = http.DefaultTransport
			}
			c.c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if d, ok := r.Context().Deadline(); ok {
					deadline = time.Until(d)
				}
				return inner.RoundTrip(r)
			})

			out, err := op.call(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if reflect.ValueOf(out).IsNil() {
				t.Fatal("nil result")
			}
			reqs := f.recorded()
			if len(reqs) != 1 {
				t.Fatalf("%d requests", len(reqs))
			}
			r := reqs[0]
			method, _, _ := strings.Cut(op.name, " ")
			if r.Method != method || r.RequestURI != "/api/v4"+op.uri {
				t.Errorf("sent %s %s, want %s /api/v4%s", r.Method, r.RequestURI, method, op.uri)
			}
			if r.Key != op.key {
				t.Errorf("signed with %q key, want %q", r.Key, op.key)
			}
			if string(r.Body) != op.body {
				t.Errorf("body\n got %s\nwant %s", r.Body, op.body)
			}
			if got := r.Header.Get("Content-Type"); (op.body != "") != (got == "application/json") {
				t.Errorf("Content-Type %q with body %q", got, op.body)
			}
			key := r.Header.Get("Idempotency-Key")
			switch op.idem {
			case "derived":
				if want := derivedIdempotencyKey(r.RequestURI, orderIDOf(t, r.Body)); key != want {
					t.Errorf("Idempotency-Key %q, want %q", key, want)
				}
			case "random":
				if !uuid4Pattern.MatchString(key) {
					t.Errorf("Idempotency-Key %q is not a UUIDv4", key)
				}
			default:
				if key != "" {
					t.Errorf("unexpected Idempotency-Key %q", key)
				}
			}
			if op.timeout != 0 && (deadline > op.timeout || deadline < op.timeout-5*time.Second) {
				t.Errorf("attempt timeout %v, want %v", deadline, op.timeout)
			}
			if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "enigmoss-go-sdk/"+Version+" go/") {
				t.Errorf("User-Agent %q", ua)
			}
		})
	}
}

func orderIDOf(t *testing.T, body []byte) string {
	t.Helper()
	var v struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v.OrderID
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestOperationsMatchContract fails when an operation of openapi-v4.json is not implemented,
// or when a response model misses a field of the API.
func TestOperationsMatchContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/contract/openapi-v4.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Operations map[string][]string `json:"operations"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	implemented := map[string]operation{}
	for _, op := range operations() {
		implemented[op.name] = op
	}
	var names []string
	for name := range contract.Operations {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) != 34 {
		t.Errorf("the contract has %d operations, want 34", len(names))
	}
	for _, name := range names {
		op, ok := implemented[name]
		if !ok {
			t.Errorf("operation %s is not implemented", name)
			continue
		}
		fields := jsonFields(reflect.TypeOf(op.response))
		for _, f := range contract.Operations[name] {
			if !fields[f] {
				t.Errorf("%s: %T has no field %q", name, op.response, f)
			}
		}
	}
	if len(implemented) != len(contract.Operations) {
		t.Errorf("%d operations tested, %d in the contract", len(implemented), len(contract.Operations))
	}
}

func jsonFields(t reflect.Type) map[string]bool {
	fields := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		fields[name] = true
	}
	return fields
}

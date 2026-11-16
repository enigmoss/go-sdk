package enigmoss_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/enigmoss/go-sdk"
)

const projectID = "3f2b8c1e-5d4a-4e7b-9c6f-1a2b3c4d5e6f"

// fakeAPI is a stand-in for the API host in tests: an HTTPS server from the standard
// library that answers with fixtures. Real code passes the API host from the dashboard.
func fakeAPI() *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Idempotency-Key") == "" {
			http.Error(w, "a create without Idempotency-Key", http.StatusBadRequest)
			return
		}
		data := map[string]string{
			"GET /api/v4/project":   `{"id":"` + projectID + `","name":"Example Shop","key_type":"payment","permissions":["payment_create_invoice"],"server_time":1767225600}`,
			"POST /api/v4/payments": `{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","order_id":"order-42","status":"pending","is_final":false,"amount":"10.50","currency_code":"USDT","payment_url":"https://pay.example.com/invoice/7c9e6679"}`,
			"GET /api/v4/payments":  `[{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","status":"paid","is_final":true,"amount":"10.50"},{"id":"0f8fad5b-d9cb-469f-a165-70867728950e","status":"overpaid","is_final":true,"amount":"7.00"}]`,
			"GET /api/v4/payouts":   `[]`,
			"POST /api/v4/payouts":  `{"id":"9b2d4f6a-1c3e-4a5b-8d7f-0e1a2b3c4d5e","order_id":"withdrawal-42","status":"pending","is_final":false,"amount":"100.00","charged":{"amount":"101.00","fee":"1.00"}}`,
		}[r.Method+" "+r.URL.Path]
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":%s,"meta":{"cursor":{"next":null,"has_more":false}}}`, data)
	}))
}

func ExampleNewClient() {
	srv := fakeAPI() // in production: "https://api.example.com", the API host from your dashboard
	defer srv.Close()

	c, err := enigmoss.NewClient(srv.URL, projectID,
		enigmoss.WithPaymentKey("payment-key"), // os.Getenv("ENIGMOSS_PAYMENT_KEY")
		enigmoss.WithHTTPClient(srv.Client()),  // only for the test server
	)
	if err != nil {
		log.Fatal(err)
	}

	// The first safe call: it changes nothing and checks the key, the permissions and the clock.
	p, err := c.Project.Get(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(p.Name, p.KeyType)
	// Output: Example Shop payment
}

func ExamplePaymentService_Create() {
	srv := fakeAPI()
	defer srv.Close()
	c, _ := enigmoss.NewClient(srv.URL, projectID, enigmoss.WithPaymentKey("payment-key"), enigmoss.WithHTTPClient(srv.Client()))

	p, err := c.Payments.Create(context.Background(), &enigmoss.PaymentParams{
		Amount:       "10.50", // money is always a decimal string
		CurrencyCode: "USDT",
		OrderID:      "order-42", // one order_id is one invoice, forever
		CallbackURL:  "https://shop.example/webhooks/enigmoss",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(p.Status, p.PaymentURL) // redirect the customer to PaymentURL
	// Output: pending https://pay.example.com/invoice/7c9e6679
}

func ExamplePayoutService_Create() {
	srv := fakeAPI()
	defer srv.Close()
	c, _ := enigmoss.NewClient(srv.URL, projectID, enigmoss.WithPayoutKey("payout-key"), enigmoss.WithHTTPClient(srv.Client()))

	// Run payouts in a background worker with a long context, not in an HTTP handler.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	params := &enigmoss.PayoutParams{
		OrderID:      "withdrawal-42",
		Amount:       "100.00",
		CurrencyCode: "USDT",
		NetworkCode:  "tron",
		Address:      "TRecipientabcdefghijklmnopqrstuvwx",
	}
	p, err := c.Payouts.Create(ctx, params)
	switch {
	case err == nil:
		// created: save p.ID, wait for the payout.* webhook
		fmt.Println("created", p.Status, "charged", p.Charged.Amount)
	case enigmoss.OutcomeUnknown(err):
		// maybe created: keep your record "pending", reconcile later with FindByOrderID; never a new order_id
		fmt.Println("pending reconcile:", err)
	case enigmoss.IsOrderIDTaken(err):
		p, err = c.Payouts.FindByOrderID(ctx, params.OrderID)
		fmt.Println("already exists", p, err)
	default:
		// definitely not created: fix the cause and retry with the SAME order_id
		fmt.Println("not created:", err)
	}
	// Output: created pending charged 101.00
}

func ExamplePaymentService_All() {
	srv := fakeAPI()
	defer srv.Close()
	c, _ := enigmoss.NewClient(srv.URL, projectID, enigmoss.WithPaymentKey("payment-key"), enigmoss.WithHTTPClient(srv.Client()))

	// Pages are fetched lazily; break stops the requests.
	for p, err := range c.Payments.All(context.Background(), &enigmoss.PaymentQuery{CreatedFrom: time.Now().AddDate(0, 0, -7)}) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(p.ID, p.Status, p.Amount, p.IsSuccessful())
	}
	// Output:
	// 7c9e6679-7425-40de-944b-e07fc1f90ae7 paid 10.50 true
	// 0f8fad5b-d9cb-469f-a165-70867728950e overpaid 7.00 true
}

func ExampleParseWebhook() {
	paymentKey := "payment-key" // os.Getenv("ENIGMOSS_PAYMENT_KEY"): payout.* and refund.* are signed with it too

	handler := func(w http.ResponseWriter, r *http.Request) {
		ev, err := enigmoss.ParseWebhook(r, projectID, paymentKey)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Dedupe by ev.ID in your database, enqueue the work, answer 200 within 15 s.
		if ev.Type == "payment.paid" {
			p, err := ev.Payment()
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if p.IsSuccessful() {
				fmt.Println("fulfil order", p.OrderID)
			}
		}
		w.WriteHeader(http.StatusOK)
	}

	// A signed webhook, as the API sends it.
	body, _ := json.Marshal(map[string]any{
		"event": "payment.paid", "event_id": "019b7710-0000-7000-8000-000000000001", "created_at": "2026-01-01T00:05:00Z",
		"data": map[string]any{"id": "7c9e6679-7425-40de-944b-e07fc1f90ae7", "order_id": "order-42", "status": "paid", "is_final": true, "amount": "10.50"},
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/enigmoss", bytes.NewReader(body))
	req.Header = enigmoss.SignWebhook(body, projectID, paymentKey)
	rec := httptest.NewRecorder()
	handler(rec, req)
	fmt.Println(rec.Code)
	// Output:
	// fulfil order order-42
	// 200
}

func Example_reconcile() {
	srv := fakeAPI()
	defer srv.Close()
	c, _ := enigmoss.NewClient(srv.URL, projectID, enigmoss.WithPayoutKey("payout-key"), enigmoss.WithHTTPClient(srv.Client()))

	// A cron job for payouts left "pending" after OutcomeUnknown.
	p, err := c.Payouts.FindByOrderID(context.Background(), "withdrawal-42")
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "try again later:", err)
	case p == nil:
		fmt.Println("not created: repeat Create with the SAME order_id")
	default:
		fmt.Println("created:", p.Status)
	}
	// Output: not created: repeat Create with the SAME order_id
}

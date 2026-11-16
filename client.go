package enigmoss

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Version is the version of this SDK, sent in the User-Agent header.
const Version = "0.1.0"

const apiPrefix = "/api/v4"

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Client calls the Project API v4. It is safe for concurrent use: create one per process
// and share it.
//
// Every resource of the API is a field of the client:
//
//	p, err := c.Payments.Create(ctx, &enigmoss.PaymentParams{Amount: "10.50", CurrencyCode: "USDT", OrderID: "order-42"})
type Client struct {
	// Project reads the project the key belongs to. Either key.
	Project *ProjectService
	// Payments creates and reads invoices. Payment key.
	Payments *PaymentService
	// Wallets creates and blocks static wallets. Payment key.
	Wallets *WalletService
	// Refunds refunds payments and blocked wallets. Payout key.
	Refunds *RefundService
	// Payouts sends money to external addresses. Payout key.
	Payouts *PayoutService
	// Transfers moves money to the personal account. Payout key.
	Transfers *TransferService
	// Balances lists the business balances. Payment key.
	Balances *BalanceService
	// Transactions lists the balance movements. Payment key.
	Transactions *TransactionService
	// PaymentMethods lists payment methods and sets their discounts.
	PaymentMethods *PaymentMethodService
	// PayoutMethods lists payout methods. Either key.
	PayoutMethods *PayoutMethodService
	// ExchangeRates lists exchange rates. Either key.
	ExchangeRates *ExchangeRateService
	// Events reads and redelivers webhook events. Payment key.
	Events *EventService
	// Webhooks sends test webhooks. Payment key. Incoming webhooks are verified by ParseWebhook.
	Webhooks *WebhookService

	c *core
}

// Option configures a Client in NewClient.
type Option func(*options)

type options struct {
	paymentKey, payoutKey secret
	httpClient            *http.Client
	fallbackOrigin        string
	onOriginChange        func(OriginChange)
	errs                  []error
}

// WithPaymentKey sets the payment API key. It signs payments, static wallets, balances,
// transactions, payment method settings, the events journal and test webhooks, and it is
// the key incoming webhooks are signed with.
func WithPaymentKey(key string) Option {
	return func(o *options) {
		if key == "" {
			o.errs = append(o.errs, errors.New("enigmoss: WithPaymentKey: the key is empty"))
		}
		o.paymentKey = newSecret(key)
	}
}

// WithPayoutKey sets the payout API key. It signs payouts, payout quotes, refunds and
// transfers. Pass it only when you use them: requests with this key are accepted only
// from the IP addresses allowed for the project.
func WithPayoutKey(key string) Option {
	return func(o *options) {
		if key == "" {
			o.errs = append(o.errs, errors.New("enigmoss: WithPayoutKey: the key is empty"))
		}
		o.payoutKey = newSecret(key)
	}
}

// WithHTTPClient sets the HTTP client, for a proxy, mTLS or tests
// (httptest.NewTLSServer(...).Client()). The SDK uses a copy that never follows redirects.
//
// Do not set http.Client.Timeout below 180 seconds: creating a payout or a refund may take
// that long. The SDK applies its own per-attempt timeouts and honours the context.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *options) {
		if hc == nil {
			o.errs = append(o.errs, errors.New("enigmoss: WithHTTPClient: the client is nil"))
			return
		}
		o.httpClient = hc
	}
}

// WithFallbackBaseURL sets the fallback API host: the host the SDK asks where the API is
// now when the API host itself cannot be reached. It has the same form as baseURL. Without
// this option, or with an empty fallbackURL (an unset environment variable), the fallback
// is off.
//
// The fallback is the last step of a call, after the usual retries: when the last attempt
// again could not connect to the API host (DNS, connect or TLS handshake; never a response
// timeout, a connection lost after sending, a cancelled context or a proxy failure), the
// SDK sends one signed request to the fallback host. A new origin from a response signed
// with your key is remembered for the time the response allows (1 hour to 7 days), and
// the call is repeated there with the same body and Idempotency-Key. Any other answer of
// the fallback host is returned as the error of the call, with the network error as its
// cause, and the fallback host is not asked again for 5 minutes.
func WithFallbackBaseURL(fallbackURL string) Option {
	return func(o *options) {
		if strings.TrimSpace(fallbackURL) == "" {
			o.fallbackOrigin = ""
			return
		}
		origin, err := normalizeBaseURL(fallbackURL)
		if err != nil {
			o.errs = append(o.errs, fmt.Errorf("enigmoss: WithFallbackBaseURL: %s", strings.TrimPrefix(err.Error(), "enigmoss: ")))
			return
		}
		o.fallbackOrigin = origin
	}
}

// WithOnOriginChange sets a function that is called every time the client moves to
// another API origin named by the fallback host (see WithFallbackBaseURL): log it, alert,
// update your configuration.
//
// It is called once per move, from a goroutine of the SDK, before the calls that waited
// for the new origin continue: keep it fast. A panic of the function is dropped.
func WithOnOriginChange(fn func(OriginChange)) Option {
	return func(o *options) {
		if fn == nil {
			o.errs = append(o.errs, errors.New("enigmoss: WithOnOriginChange: the function is nil"))
			return
		}
		o.onOriginChange = fn
	}
}

// NewClient returns a client of the API host baseURL (the host shown in your dashboard,
// such as "https://api.example.com") for the project projectID. Pass at least one key.
//
// The SDK appends "/api/v4" itself; a baseURL ending with "/api/v4" is accepted too. Any
// other path is rejected, because the server signs the exact request path.
func NewClient(baseURL, projectID string, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if err := errors.Join(o.errs...); err != nil {
		return nil, err
	}

	origin, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	project := strings.ToLower(strings.TrimSpace(projectID))
	if !uuidPattern.MatchString(project) {
		return nil, errors.New(`enigmoss: projectID must be the project UUID, e.g. "3f2b8c1e-5d4a-4e7b-9c6f-1a2b3c4d5e6f"`)
	}
	if o.paymentKey == nil && o.payoutKey == nil {
		return nil, errors.New("enigmoss: pass at least one API key: enigmoss.WithPaymentKey and/or enigmoss.WithPayoutKey")
	}

	hc := &http.Client{}
	if o.httpClient != nil {
		copied := *o.httpClient
		hc = &copied
	}
	// A redirected POST could send a payout to another host: never follow redirects.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	cr := &core{
		origin:     origin,
		projectID:  project,
		paymentKey: o.paymentKey,
		payoutKey:  o.payoutKey,
		http:       hc,
		userAgent:  userAgent(),
		now:        time.Now,
		sleep:      sleepContext,
		random:     rand.Float64,
	}
	cr.fb.origin = o.fallbackOrigin
	cr.fb.onChange = o.onOriginChange
	return &Client{
		Project:        &ProjectService{cr},
		Payments:       &PaymentService{cr},
		Wallets:        &WalletService{cr},
		Refunds:        &RefundService{cr},
		Payouts:        &PayoutService{cr},
		Transfers:      &TransferService{cr},
		Balances:       &BalanceService{cr},
		Transactions:   &TransactionService{cr},
		PaymentMethods: &PaymentMethodService{cr},
		PayoutMethods:  &PayoutMethodService{cr},
		ExchangeRates:  &ExchangeRateService{cr},
		Events:         &EventService{cr},
		Webhooks:       &WebhookService{cr},
		c:              cr,
	}, nil
}

// String describes the client without its keys. The origin is the one requests go to
// now: the base URL, or the origin the fallback host named (see WithFallbackBaseURL).
func (c *Client) String() string {
	if c == nil || c.c == nil {
		return "enigmoss.Client(nil)"
	}
	return fmt.Sprintf("enigmoss.Client{%s, project %s, keys [redacted]}", c.c.activeOrigin(), c.c.projectID)
}

// Format prints the client without its keys for every verb, %+v and %#v included.
func (c *Client) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprint(f, c.String())
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || u.Hostname() == "" {
		return "", errors.New(`enigmoss: baseURL must be an absolute URL such as "https://api.example.com"`)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", errors.New("enigmoss: baseURL must use https")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", errors.New("enigmoss: baseURL must not contain credentials, a query string or a fragment")
	}
	if path := strings.TrimRight(u.EscapedPath(), "/"); path != "" && path != apiPrefix {
		return "", fmt.Errorf(`enigmoss: baseURL must point at the API host ("https://api.example.com"), optionally followed by %q; got path %q`, apiPrefix, path)
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// secret keeps an API key out of logs: fmt prints a func only as an address, whatever the
// verb and however deep it is nested.
type secret func() string

func newSecret(key string) secret {
	if key == "" {
		return nil
	}
	return func() string { return key }
}

// Bool returns a pointer to b, for optional flags where false is meaningful:
//
//	FeeDeductedFromAmount: enigmoss.Bool(false)
func Bool(b bool) *bool { return &b }

// Int returns a pointer to n, for optional numbers where 0 is meaningful:
//
//	DiscountPercent: enigmoss.Int(0)
func Int(n int) *int { return &n }

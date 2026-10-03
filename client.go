// Package stripepay charges with Stripe Checkout and, above all, decides whether a payment
// really is yours.
//
// It talks to the Stripe API over plain HTTP with form bodies, without the official
// library. Three reasons, none of them cosmetic:
//
//   - `stripe-go` keeps the API key in a package-level GLOBAL (`stripe.Key`). A process
//     that charges for more than one account — a multi-tenant service with one key per
//     tenant, or one key per mode — cannot use that without contortions. Here the
//     [Client] is explicit and there is no global state at all.
//   - `stripe-go` REJECTS every event the day the API version pinned on the webhook
//     endpoint is not exactly the one it expects. That forces you to pin an `api_version`
//     when you register the endpoint and to keep it in step with the library. Reading the
//     JSON ourselves, Stripe's forward compatibility is enough and nothing is pinned.
//   - The surface most applications use is five calls. The official library's dependency
//     tree, for five calls, does not pay for itself.
//
// What this package actually gives you is not the API wrapper: it is [Client.Check], the
// verification policy. A Stripe account can be shared by several projects, and Stripe
// delivers EVERY event to EVERY endpoint on the account. A session being paid does not
// mean it is yours, nor that it carries the money you expected, nor that the money has
// arrived yet.
package stripepay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultBaseURL is the Stripe API. Tests point elsewhere with [Client.SetBaseURL].
const defaultBaseURL = "https://api.stripe.com"

// maxBody is the most that is read from a Stripe response or from a webhook body.
const maxBody = 1 << 20

// ErrNoSuchObject is what Stripe returns when the object asked for does not exist
// (`resource_missing`): an invented identifier, or one from another account or another mode.
//
// It is told apart from every other error on purpose: "it does not exist" and "Stripe is
// not answering" lead to different decisions, and conflating them is how a return page
// handed an invented identifier ends up claiming a payment is being confirmed.
var ErrNoSuchObject = errors.New("stripe: no such object")

// Client talks to one Stripe account, with one key, on behalf of one project.
//
// There is no global state: a process may hold as many clients as it likes, each with its
// own key. That is what lets a multi-tenant service charge into each tenant's own account.
type Client struct {
	secret        string
	webhookSecret string
	service       string
	http          *http.Client
	baseURL       string
}

// Options are the parameters of [New].
type Options struct {
	// Secret is the account's secret or restricted key (`sk_…` or `rk_…`). Empty is
	// legitimate: it means this client cannot charge, and [Client.Enabled] reports false.
	// A service that also offers free things can run like that.
	Secret string

	// WebhookSecret is the endpoint's signing secret (`whsec_…`). Without it
	// [Client.VerifyWebhook] always fails: an event that cannot be checked cannot be
	// accepted.
	WebhookSecret string

	// Service is who you are inside the account. It goes into `metadata[service]` of every
	// session you create and it is the first thing [Client.Check] looks at: a session
	// carrying a different mark belongs to another project and is dropped in silence.
	//
	// It is required, because without it there is no way to tell your own payments from a
	// neighbour's on a shared account.
	Service string

	// HTTPClient, when given, replaces the default one (a 20 second timeout).
	HTTPClient *http.Client
}

// New creates a client. It fails if it is not told which project you are.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Service) == "" {
		return nil, fmt.Errorf("stripepay: Options.Service is required (who you are inside the account)")
	}
	h := o.HTTPClient
	if h == nil {
		h = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		secret:        o.Secret,
		webhookSecret: o.WebhookSecret,
		service:       o.Service,
		http:          h,
		baseURL:       defaultBaseURL,
	}, nil
}

// Enabled reports whether this client can charge. Without a key, it cannot.
func (c *Client) Enabled() bool { return c.secret != "" }

// Service is this project's mark inside the account.
func (c *Client) Service() string { return c.service }

// Mode reports whether the key is a test or a live one, from the prefix Stripe puts on it.
func (c *Client) Mode() (Mode, error) { return ModeOf(c.secret) }

// SetBaseURL changes the API address. **For tests only**: it points the client at a fake
// server so the request you would really send can be asserted without leaving the machine.
func (c *Client) SetBaseURL(u string) { c.baseURL = u }

func (c *Client) do(ctx context.Context, method, path string, form url.Values, idem string, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		switch {
		// It does not exist. Callers must be able to tell this from "Stripe is not
		// answering": a return page handed an invented identifier must not claim there is
		// a payment being confirmed.
		case e.Error.Code == "resource_missing":
			return fmt.Errorf("%w: %s", ErrNoSuchObject, e.Error.Message)
		// The usual case with a restricted key: it is missing a permission. Better to say
		// so than to let someone hunt for the problem on their side.
		case strings.Contains(string(raw), "does not have the required permissions"):
			return fmt.Errorf("stripe %s: the key lacks a permission for %s: %s",
				resp.Status, path, string(raw))
		}
		return fmt.Errorf("stripe %s: %s", resp.Status, string(raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, "", out)
}

func (c *Client) post(ctx context.Context, path string, form url.Values, idem string, out any) error {
	return c.do(ctx, http.MethodPost, path, form, idem, out)
}

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
//     JSON ourselves, Stripe's forward compatibility is enough: no endpoint pins one (the
//     only version sent is on the request that sets [Options.DisplayName]).
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

// brandingAPIVersion is the first Stripe API version that has `branding_settings` on a
// Checkout Session. The request that carries it says this version, so that it does not
// depend on the default version of the account the key belongs to. No other request pins one.
const brandingAPIVersion = "2025-09-30.clover"

// displayNameField is the form field of [Options.DisplayName].
const displayNameField = "branding_settings[display_name]"

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
	account       string
	displayName   string // empty only with Options.KeepAccountName
	http          *http.Client
	baseURL       string
}

// Options are the parameters of [New].
type Options struct {
	// Secret is the account's secret or restricted key (`sk_…` or `rk_…`). Empty is
	// legitimate: it means this client cannot charge, and [Client.Enabled] reports false.
	// A service that also offers free things can run like that.
	Secret string

	// Account (`acct_…`) makes every call act on a CONNECTED account (Stripe Connect, direct
	// charges): the key is the platform's and each request carries the `Stripe-Account`
	// header, so the sessions, the payments and the refunds live in the connected account,
	// which is the merchant. It also sets what the [Handler] accepts: only events of that
	// account. Empty means the key's own account.
	Account string

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

	// DisplayName is the name at the top of the payment page of every session this client
	// creates (`branding_settings[display_name]`): the brand the buyer knows. A Stripe
	// account shared by several projects has one public name, and it is not theirs. It only
	// changes the top of the page: the account's name still appears in the terms and on the
	// receipts.
	//
	// Exactly one of DisplayName and KeepAccountName is required.
	DisplayName string

	// KeepAccountName says that the payment page must show the name of the account being
	// charged. It is what a multi-tenant service sets when the account is the tenant's own
	// (its own key, or [Options.Account]): the tenant is the merchant and the name is theirs.
	KeepAccountName bool

	// HTTPClient, when given, replaces the default one (a 20 second timeout).
	HTTPClient *http.Client
}

// New creates a client. It fails if it is not told which project you are, or which name the
// payment page has to show.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Service) == "" {
		return nil, fmt.Errorf("stripepay: Options.Service is required (who you are inside the account)")
	}
	name := strings.TrimSpace(o.DisplayName)
	switch {
	case name == "" && !o.KeepAccountName:
		return nil, fmt.Errorf("stripepay: Options.DisplayName is required (the name at the top of the payment page); " +
			"set KeepAccountName instead only to charge into an account whose own name must show")
	case name != "" && o.KeepAccountName:
		return nil, fmt.Errorf("stripepay: Options.DisplayName and Options.KeepAccountName contradict each other: set one")
	}
	h := o.HTTPClient
	if h == nil {
		h = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		secret:        o.Secret,
		webhookSecret: o.WebhookSecret,
		service:       o.Service,
		account:       strings.TrimSpace(o.Account),
		displayName:   name,
		http:          h,
		baseURL:       defaultBaseURL,
	}, nil
}

// Enabled reports whether this client can charge. Without a key, it cannot.
func (c *Client) Enabled() bool { return c.secret != "" }

// Account is the connected account every call acts on, or "" when it is the key's own.
func (c *Client) Account() string { return c.account }

// Service is this project's mark inside the account.
func (c *Client) Service() string { return c.service }

// DisplayName is the name the payment page shows, or "" when it is the account's own.
func (c *Client) DisplayName() string { return c.displayName }

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
	if c.account != "" {
		req.Header.Set("Stripe-Account", c.account)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if form.Has(displayNameField) {
		req.Header.Set("Stripe-Version", brandingAPIVersion)
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

func (c *Client) delete(ctx context.Context, path string, form url.Values, out any) error {
	return c.do(ctx, http.MethodDelete, path, form, "", out)
}

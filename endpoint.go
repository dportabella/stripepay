package stripepay

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Endpoint is a webhook endpoint on the Stripe account.
type Endpoint struct {
	ID            string   `json:"id"`
	URL           string   `json:"url"`
	Status        string   `json:"status"` // enabled | disabled
	Description   string   `json:"description"`
	EnabledEvents []string `json:"enabled_events"`
	// APIVersion is the API version pinned on the endpoint, if any.
	//
	// This library never pins one, and that is deliberate: an endpoint with a pinned
	// version receives events in that version's shape forever, and whoever reads them has
	// to know which one it was. Reading the JSON ourselves, only the fields we ask for
	// matter, and Stripe's forward compatibility is enough. (The official `stripe-go`, by
	// contrast, rejects every event if the version is not exactly its own, which is where
	// the habit of pinning comes from.)
	APIVersion string `json:"api_version"`
	// Secret is only populated WHEN IT IS CREATED: "Returns the webhook endpoint object
	// with the secret field populated". After that, Stripe never shows it again.
	Secret string `json:"secret"`
}

// HasEvents reports whether the endpoint is subscribed to exactly these events, no more and
// no fewer.
func (e *Endpoint) HasEvents(events []string) bool {
	if len(e.EnabledEvents) != len(events) {
		return false
	}
	a := append([]string(nil), e.EnabledEvents...)
	b := append([]string(nil), events...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ListEndpoints returns every webhook endpoint on the account, in the key's mode.
func (c *Client) ListEndpoints(ctx context.Context) ([]Endpoint, error) {
	var out struct {
		Data []Endpoint `json:"data"`
	}
	if err := c.get(ctx, "/v1/webhook_endpoints?limit=100", &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// FindEndpoint looks for the endpoint of one particular URL, or returns nil if there is none.
func (c *Client) FindEndpoint(ctx context.Context, endpointURL string) (*Endpoint, error) {
	list, err := c.ListEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].URL == endpointURL {
			return &list[i], nil
		}
	}
	return nil, nil
}

// CreateEndpoint registers a webhook endpoint and returns the signing secret, which is the
// only moment it can be obtained.
func (c *Client) CreateEndpoint(ctx context.Context, endpointURL, description string, events []string) (*Endpoint, error) {
	if endpointURL == "" {
		return nil, fmt.Errorf("stripepay: the endpoint URL is required")
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("stripepay: it has to be told which events to subscribe to")
	}
	f := url.Values{}
	f.Set("url", endpointURL)
	if description != "" {
		f.Set("description", description)
	}
	for _, e := range events {
		f.Add("enabled_events[]", e)
	}
	var ep Endpoint
	if err := c.post(ctx, "/v1/webhook_endpoints", f, "", &ep); err != nil {
		return nil, err
	}
	if ep.Secret == "" {
		return nil, fmt.Errorf("stripepay: Stripe created endpoint %s but did not return its "+
			"secret, and it never shows it again: delete it and try once more", ep.ID)
	}
	return &ep, nil
}

// UpdateEndpointEvents changes the event list of an endpoint that already exists. It does
// not return the secret: Stripe only shows that when the endpoint is created.
func (c *Client) UpdateEndpointEvents(ctx context.Context, id string, events []string) (*Endpoint, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("stripepay: it has to be told which events to subscribe to")
	}
	f := url.Values{}
	for _, e := range events {
		f.Add("enabled_events[]", e)
	}
	var ep Endpoint
	if err := c.post(ctx, "/v1/webhook_endpoints/"+url.PathEscape(id), f, "", &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// SetEndpointEnabled turns a webhook endpoint on or off without deleting it.
//
// This is what you want when a service moves between modes. Webhook endpoints are
// per mode, and the one belonging to the mode you just left does not go away on its own: on
// a SHARED account it keeps receiving the other projects' events of that mode, your server
// no longer knows its signing secret, and every delivery is rejected. Days later Stripe
// sends a failure notice about something that is not a problem.
//
// Disabling it rather than deleting it keeps the signing secret alive, so moving back is one
// call and not a new secret.
func (c *Client) SetEndpointEnabled(ctx context.Context, id string, enabled bool) (*Endpoint, error) {
	f := url.Values{}
	f.Set("disabled", strconv.FormatBool(!enabled))
	var ep Endpoint
	if err := c.post(ctx, "/v1/webhook_endpoints/"+url.PathEscape(id), f, "", &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// DeleteEndpoint deletes a webhook endpoint.
func (c *Client) DeleteEndpoint(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/webhook_endpoints/"+url.PathEscape(id), nil, "", nil)
}

// EnsureRequest is what [Client.EnsureEndpoint] is asked for.
type EnsureRequest struct {
	// URL of your own webhook handler.
	URL string
	// Description shows up on the Stripe dashboard. It is worth saying which project and
	// which tenant it is, because a shared account has more than one.
	Description string
	// Events are the events the project knows how to handle, and no others: an endpoint
	// subscribed to things nobody reads clutters the delivery log and makes it look as if
	// there were work pending.
	Events []string
	// HaveSecret says whether you already hold the signing secret for this URL. If an
	// endpoint is already there and you do not have its secret, it cannot be used and it
	// cannot be recovered: you need Recreate.
	HaveSecret bool
	// Recreate deletes whatever is there and creates a new one. It is the only way to
	// recover a lost secret.
	Recreate bool
}

// EnsureAction says what [Client.EnsureEndpoint] did.
type EnsureAction string

const (
	// ActionNone: it was already there, with the right events and with the secret in hand.
	ActionNone EnsureAction = "nothing to do"
	// ActionCreated: it was created. The only case where [EnsureResult.Secret] is filled in.
	ActionCreated EnsureAction = "created"
	// ActionRecreated: it was deleted and created again. Also carries the secret.
	ActionRecreated EnsureAction = "recreated"
	// ActionEventsUpdated: it was already there and its event list was corrected.
	ActionEventsUpdated EnsureAction = "events corrected"
)

// EnsureResult is the result of [Client.EnsureEndpoint].
type EnsureResult struct {
	Action   EnsureAction
	Endpoint *Endpoint
	// Secret is the signing secret, and it is only there when the endpoint has just been
	// created. It has to be stored at that moment or it is lost forever.
	Secret string
	// Mode is the mode that was acted on, read from the key's prefix. It is worth showing
	// to whoever runs the command every time: webhook endpoints are independent per mode,
	// and getting it wrong means the real events go to a handler that does not know their
	// secret.
	Mode Mode
}

// EnsureEndpoint leaves the account with a single endpoint pointing at your URL, subscribed
// to exactly the events you ask for and with no API version pinned.
//
// If one is already there and you do not hold its secret, it does NOT touch it on its own:
// it returns an error saying you need [EnsureRequest.Recreate], because deleting a webhook
// endpoint is a decision for whoever owns the account, not for a library.
func (c *Client) EnsureEndpoint(ctx context.Context, req EnsureRequest) (*EnsureResult, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("stripepay: this client has no key")
	}
	mode, err := c.Mode()
	if err != nil {
		return nil, err
	}
	res := &EnsureResult{Mode: mode}

	existing, err := c.FindEndpoint(ctx, req.URL)
	if err != nil {
		return nil, err
	}
	if existing != nil && !req.Recreate {
		if !req.HaveSecret {
			return nil, fmt.Errorf("stripepay: there is an endpoint pointing at %s (%s) but we do "+
				"not hold its signing secret, and Stripe never shows it again: come back with "+
				"Recreate to delete it and create a new one", req.URL, existing.ID)
		}
		if !existing.HasEvents(req.Events) {
			updated, err := c.UpdateEndpointEvents(ctx, existing.ID, req.Events)
			if err != nil {
				return nil, err
			}
			res.Action, res.Endpoint = ActionEventsUpdated, updated
			return res, nil
		}
		res.Action, res.Endpoint = ActionNone, existing
		return res, nil
	}

	action := ActionCreated
	if existing != nil {
		if err := c.DeleteEndpoint(ctx, existing.ID); err != nil {
			return nil, fmt.Errorf("deleting endpoint %s: %w", existing.ID, err)
		}
		action = ActionRecreated
	}
	ep, err := c.CreateEndpoint(ctx, req.URL, req.Description, req.Events)
	if err != nil {
		return nil, err
	}
	res.Action, res.Endpoint, res.Secret = action, ep, ep.Secret
	return res, nil
}

// StaleEndpoints are the endpoints on the account that point at your own server but not at
// the URL you use now.
//
// They exist to catch the failure that actually happens: a service is switched from test to
// live, the endpoint of the new mode is created, and the old one stays alive pointing at the
// same server, which does not know its secret. Stripe keeps delivering to it, the server
// rejects every delivery with "invalid signature", and days later a failure notice arrives
// about something that is not a problem at all.
//
// Note that webhook endpoints are per mode, and this call only sees the ones in the key's
// mode. The other mode's has to be looked for with the other mode's key.
func (c *Client) StaleEndpoints(ctx context.Context, host, currentURL string) ([]Endpoint, error) {
	list, err := c.ListEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	var out []Endpoint
	for _, ep := range list {
		if ep.URL == currentURL {
			continue
		}
		u, err := url.Parse(ep.URL)
		if err != nil || !strings.EqualFold(u.Host, host) {
			continue
		}
		out = append(out, ep)
	}
	return out, nil
}

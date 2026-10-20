package stripepay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Subscription is the part of a Stripe subscription that is used to decide whether access
// continues: whether it is yours, whether it is alive, and until when it is paid.
type Subscription struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	// Status is Stripe's own: incomplete, incomplete_expired, trialing, active, past_due,
	// canceled, unpaid or paused. "active" is paid; "past_due" is a renewal that failed
	// and is being retried (what to do meanwhile is the application's decision); "canceled"
	// is over.
	Status            string            `json:"status"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	CurrentPeriodEnd  int64             `json:"current_period_end"` // up to API version 2025-03-31
	Metadata          map[string]string `json:"metadata"`
	Items             struct {
		Data []struct {
			CurrentPeriodEnd int64 `json:"current_period_end"` // from API version 2025-04-30 on
		} `json:"data"`
	} `json:"items"`
}

// PeriodEnd is the end of the period that is paid, whichever way the account's API version
// reports it (on the subscription, or on its items). Zero if Stripe gave neither.
func (s *Subscription) PeriodEnd() time.Time {
	end := s.CurrentPeriodEnd
	for _, it := range s.Items.Data {
		if it.CurrentPeriodEnd > end {
			end = it.CurrentPeriodEnd
		}
	}
	if end == 0 {
		return time.Time{}
	}
	return time.Unix(end, 0).UTC()
}

// Reference is the reference the subscription was created with ([SessionParams.Reference]).
func (s *Subscription) Reference() string { return s.Metadata["reference"] }

// IsOurSubscription reports whether this subscription was created by this project: the
// same strict comparison as [Client.IsOurs], on the mark that [Client.CreateSession] puts on
// the subscription.
func (c *Client) IsOurSubscription(s *Subscription) bool {
	return s != nil && s.Metadata[MetadataService] == c.service
}

// Invoice is one charge of a subscription: the first one and every renewal.
type Invoice struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	// Status is draft, open, paid, uncollectible or void.
	Status     string `json:"status"`
	Currency   string `json:"currency"`
	AmountPaid int64  `json:"amount_paid"`
	AmountDue  int64  `json:"amount_due"`
	// BillingReason is "subscription_create" for the first invoice, "subscription_cycle"
	// for a renewal, "subscription_update" for a change in the middle of a period.
	BillingReason string `json:"billing_reason"`

	// Subscription is where the account's API version up to 2025-03-31 puts the id of the
	// subscription; from 2025-04-30 it is under Parent. Use [Invoice.SubscriptionID].
	Subscription        string `json:"subscription"`
	SubscriptionDetails struct {
		Metadata map[string]string `json:"metadata"`
	} `json:"subscription_details"`
	Parent struct {
		SubscriptionDetails struct {
			Subscription string            `json:"subscription"`
			Metadata     map[string]string `json:"metadata"`
		} `json:"subscription_details"`
	} `json:"parent"`

	Tax        int64 `json:"tax"` // up to API version 2025-03-31
	TotalTaxes []struct {
		Amount int64 `json:"amount"`
	} `json:"total_taxes"` // from 2025-04-30 on
	Lines struct {
		Data []struct {
			Period struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			} `json:"period"`
		} `json:"data"`
	} `json:"lines"`
}

// SubscriptionID is the subscription this invoice charges, or "" for an invoice that has
// none (a one-off invoice).
func (i *Invoice) SubscriptionID() string {
	if i.Subscription != "" {
		return i.Subscription
	}
	return i.Parent.SubscriptionDetails.Subscription
}

// SubscriptionMetadata is the metadata of the subscription as the invoice carries it, which
// is enough to tell a neighbour's invoice without another call. Nil if it carries none.
func (i *Invoice) SubscriptionMetadata() map[string]string {
	if i.SubscriptionDetails.Metadata != nil {
		return i.SubscriptionDetails.Metadata
	}
	return i.Parent.SubscriptionDetails.Metadata
}

// TaxCents is the tax included in what was charged.
func (i *Invoice) TaxCents() int64 {
	t := i.Tax
	for _, x := range i.TotalTaxes {
		t += x.Amount
	}
	return t
}

// NetCents is what came in for the service, without tax.
func (i *Invoice) NetCents() int64 { return i.AmountPaid - i.TaxCents() }

// PaidThrough is the end of the period this invoice pays for: the date until which access
// is owed. Zero if the invoice has no lines with a period.
func (i *Invoice) PaidThrough() time.Time {
	var end int64
	for _, l := range i.Lines.Data {
		if l.Period.End > end {
			end = l.Period.End
		}
	}
	if end == 0 {
		return time.Time{}
	}
	return time.Unix(end, 0).UTC()
}

// SubscriptionEvent is what a handler of [Handler.OnSubscription] receives, both re-read
// from the API: the subscription the event is about and, for the `invoice.*` events, the
// invoice. Invoice is nil for the `customer.subscription.*` events.
type SubscriptionEvent struct {
	Subscription *Subscription
	Invoice      *Invoice
}

// Subscription reads the event's object as a subscription: only for `customer.subscription.*`.
func (e *Event) Subscription() (*Subscription, error) {
	var s Subscription
	if err := json.Unmarshal(e.Data.Object, &s); err != nil {
		return nil, fmt.Errorf("stripepay: the object of %s is not a subscription: %w", e.Type, err)
	}
	return &s, nil
}

// Invoice reads the event's object as an invoice: only for `invoice.*` events.
func (e *Event) Invoice() (*Invoice, error) {
	var i Invoice
	if err := json.Unmarshal(e.Data.Object, &i); err != nil {
		return nil, fmt.Errorf("stripepay: the object of %s is not an invoice: %w", e.Type, err)
	}
	return &i, nil
}

// Subscription re-reads a subscription from the API.
func (c *Client) Subscription(ctx context.Context, id string) (*Subscription, error) {
	if id == "" {
		return nil, fmt.Errorf("stripepay: the subscription id is required")
	}
	var s Subscription
	if err := c.get(ctx, "/v1/subscriptions/"+url.PathEscape(id), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Invoice re-reads an invoice from the API.
func (c *Client) Invoice(ctx context.Context, id string) (*Invoice, error) {
	if id == "" {
		return nil, fmt.Errorf("stripepay: the invoice id is required")
	}
	var i Invoice
	if err := c.get(ctx, "/v1/invoices/"+url.PathEscape(id), &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// CancelSubscription stops a subscription. With atPeriodEnd the customer keeps what they
// paid for until the period ends and nothing more is charged; without it the subscription
// ends now, and no refund is made (that is [Client.Refund]).
func (c *Client) CancelSubscription(ctx context.Context, id string, atPeriodEnd bool) (*Subscription, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("stripepay: this client has no key")
	}
	if id == "" {
		return nil, fmt.Errorf("stripepay: the subscription id is required")
	}
	var s Subscription
	if atPeriodEnd {
		err := c.post(ctx, "/v1/subscriptions/"+url.PathEscape(id), url.Values{"cancel_at_period_end": {"true"}}, "", &s)
		return &s, err
	}
	err := c.delete(ctx, "/v1/subscriptions/"+url.PathEscape(id), nil, &s)
	return &s, err
}

// ResumeSubscription takes back a cancellation made with atPeriodEnd, while the period has
// not ended.
func (c *Client) ResumeSubscription(ctx context.Context, id string) (*Subscription, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("stripepay: this client has no key")
	}
	if id == "" {
		return nil, fmt.Errorf("stripepay: the subscription id is required")
	}
	var s Subscription
	err := c.post(ctx, "/v1/subscriptions/"+url.PathEscape(id), url.Values{"cancel_at_period_end": {"false"}}, "", &s)
	return &s, err
}

// PortalSession opens Stripe's customer portal for a customer: the page where they change
// the card, see their invoices and cancel, so that the application does not have to build
// any of that. It returns the address to send the customer to; they come back to returnURL.
//
// The portal has to be set up once on the Stripe dashboard (Settings, Billing, Customer
// portal) in each mode; without it Stripe answers with an error saying so.
func (c *Client) PortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("stripepay: this client has no key")
	}
	if customerID == "" || returnURL == "" {
		return "", fmt.Errorf("stripepay: the customer and the address to return to are required")
	}
	var out struct {
		URL string `json:"url"`
	}
	f := url.Values{"customer": {customerID}, "return_url": {returnURL}}
	if err := c.post(ctx, "/v1/billing_portal/sessions", f, "", &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

// InvoiceExpectation is what you expected of one period of a subscription, read from YOUR
// OWN row.
type InvoiceExpectation struct {
	// Reference is the one the subscription was created with.
	Reference string
	// Currency, in ISO. Empty means do not check it.
	Currency string
	// MinNetCents is the least that must have come in for the period, net of tax. Zero
	// means do not check the amount.
	MinNetCents int64
}

// CheckInvoice reports whether this invoice of this subscription is yours, really paid, and
// carries what you expected. It returns nil when the period it pays for may be delivered.
//
// It is [Client.Check] for the periods after the first: the same reasons, with the same
// meaning ([ErrForeign] is silence, [ErrNotPaid] and [ErrExpired] are ordinary, the rest
// are incidents). The invoice and the subscription must both be the ones re-read from the
// API.
func (c *Client) CheckInvoice(inv *Invoice, sub *Subscription, e InvoiceExpectation) error {
	if inv == nil || sub == nil {
		return reject(nil, ErrForeign, "there is no invoice or subscription")
	}
	if !c.IsOurSubscription(sub) {
		return reject(&Session{ID: inv.ID}, ErrForeign, "metadata[%s] = %q, and we are %q",
			MetadataService, sub.Metadata[MetadataService], c.service)
	}
	id := &Session{ID: inv.ID}
	if inv.SubscriptionID() != sub.ID {
		return reject(id, ErrReference, "the invoice charges %q, not subscription %q", inv.SubscriptionID(), sub.ID)
	}
	if e.Reference == "" {
		return reject(id, ErrReference, "it was not told which reference to expect")
	}
	if sub.Reference() != e.Reference {
		return reject(id, ErrReference, "the subscription's reference is %q, expected %q", sub.Reference(), e.Reference)
	}
	switch inv.Status {
	case "paid":
	case "void", "uncollectible":
		return reject(id, ErrExpired, "status = %q", inv.Status)
	default:
		return reject(id, ErrNotPaid, "status = %q", inv.Status)
	}
	if e.Currency != "" && !strings.EqualFold(inv.Currency, e.Currency) {
		return reject(id, ErrCurrency, "%q, expected %q", inv.Currency, e.Currency)
	}
	if e.MinNetCents > 0 {
		if net := inv.NetCents(); net < e.MinNetCents {
			return reject(id, ErrAmount, "%d net cents came in and %d were meant to be charged", net, e.MinNetCents)
		}
	}
	return nil
}

// AddInvoiceItemParams is a line added to an invoice that is still a draft.
type AddInvoiceItemParams struct {
	Customer string
	Invoice  string
	// AmountCents may be negative: a discount, or a correction to what the subscription's price says.
	AmountCents int64
	Currency    string
	Description string
	// IdempotencyKey stops a redelivery of the event from adding the line twice. Required.
	IdempotencyKey string
}

// AddInvoiceItem adds a line to a draft invoice. It is how a subscription whose amount varies from one period to the next is
// charged: the subscription carries a base price, and when `invoice.created` announces the next invoice (which stays a
// draft for about an hour) the project adds the difference between the base and what this period costs.
func (c *Client) AddInvoiceItem(ctx context.Context, p AddInvoiceItemParams) error {
	if !c.Enabled() {
		return fmt.Errorf("stripepay: this client has no key")
	}
	if p.Customer == "" || p.Invoice == "" || p.Currency == "" || p.IdempotencyKey == "" {
		return fmt.Errorf("stripepay: the customer, the invoice, the currency and an idempotency key are required")
	}
	if p.AmountCents == 0 {
		return fmt.Errorf("stripepay: a line of 0 is no use; do not add it")
	}
	f := url.Values{"customer": {p.Customer}, "invoice": {p.Invoice}, "currency": {strings.ToLower(p.Currency)},
		"amount": {fmt.Sprint(p.AmountCents)}, "metadata[" + MetadataService + "]": {c.service}}
	if p.Description != "" {
		f.Set("description", p.Description)
	}
	return c.post(ctx, "/v1/invoiceitems", f, p.IdempotencyKey, nil)
}

package stripepay

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TaxBehavior says whether the price you write already includes tax or is the net amount.
type TaxBehavior string

const (
	// TaxInclusive: the price is what the buyer pays, tax included. It is how a price has
	// to be shown to a consumer in the EU.
	TaxInclusive TaxBehavior = "inclusive"
	// TaxExclusive: the price is the net amount and tax is added on top. It only makes
	// sense with Stripe Tax enabled: with nobody to add the tax, the buyer would pay only
	// the net amount.
	TaxExclusive TaxBehavior = "exclusive"
)

// immediateMethods are the payment methods that pay on the spot, and the default value of
// [SessionParams.PaymentMethodTypes].
//
// `card` covers Apple Pay and Google Pay, which are wallets on top of a card rather than
// separate payment methods. `link` is Stripe's saved-payment method, and also confirms
// straight away.
var immediateMethods = []string{"card", "link"}

// deferredMethods are the methods that do NOT confirm at the time: Stripe closes the
// session and the money turns up hours or days later, or never.
//
// With one of those enabled, `checkout.session.completed` arrives with `payment_status:
// "unpaid"`, and any project that believes the event ships what it sells for free. They are
// never accepted here: [Client.CreateSession] refuses to offer them and [Client.Check]
// refuses to treat them as paid.
var deferredMethods = map[string]bool{
	"acss_debit": true, "au_becs_debit": true, "bacs_debit": true, "boleto": true,
	"customer_balance": true, "konbini": true, "multibanco": true, "oxxo": true,
	"sepa_debit": true, "sofort": true,
}

// SessionParams are the parameters of a Checkout Session.
//
// The amount and the description are ALWAYS decided by the server: none of this may come
// from the browser, or the buyer decides what they pay.
type SessionParams struct {
	// Reference is your own identifier for the row this payment pays for: the order, the
	// booking. It goes to `client_reference_id`, it shows up in the Stripe dashboard, and
	// it is how a payment is reconciled with what you have to deliver.
	//
	// It is REQUIRED, and it must be unguessable: the whole verification hangs off it. If
	// your project also carries somebody else's reference, that goes in Metadata, not here.
	Reference string

	// What is being charged for.
	Name        string
	Description string
	AmountCents int64
	Currency    string // ISO, e.g. "EUR"
	TaxBehavior TaxBehavior
	TaxCode     string // Stripe product tax code, e.g. "txcd_20060045"

	// The buyer.
	CustomerEmail string
	// Locale is the language of the payment page. It has to be one Stripe accepts, or
	// creating the session fails: see [Locale] and [LocaleSupported]. Empty means Stripe's
	// default, which is the buyer's browser.
	Locale                string
	CollectBillingAddress bool // needed to know the country and split the tax yourself
	CollectTaxID          bool // asks for the VAT number and puts it on the receipt
	CreateCustomer        bool // creates the Customer even for a one-off payment

	// Where the buyer comes back to. SuccessURL must carry `{CHECKOUT_SESSION_ID}`, which
	// Stripe substitutes: see [SessionIDPlaceholder].
	SuccessURL string
	CancelURL  string

	// AutomaticTax switches **Stripe Tax** on for this session: Stripe works out the tax
	// from the country and the customer type, and charges 0.5 % of the sale for it. It is
	// not needed to advertise a tax-inclusive price, because then there is nothing to
	// compute at payment time.
	AutomaticTax bool

	// CreateInvoice makes Stripe generate its own receipt. In many countries that is not a
	// legally valid invoice: whoever must issue one does it elsewhere.
	CreateInvoice bool

	// ExpiresAt expires the session, so that what is being paid for is not held forever.
	// Stripe requires between 30 minutes and 24 hours.
	ExpiresAt time.Time

	// IdempotencyKey stops a double click from opening two payments. Left empty,
	// [Client.CreateSession] derives one from Reference.
	IdempotencyKey string

	// PaymentMethodTypes are the methods offered. Empty means the immediate ones (card and
	// Link). Putting a deferred method here is an error, not an option.
	PaymentMethodTypes []string

	// CustomFields are extra questions on the payment page, up to three (Stripe's limit).
	CustomFields []CustomField

	// Metadata is whatever the project wants recorded on the session. The "service" key is
	// reserved: the client fills it in from its [Options.Service].
	Metadata map[string]string
}

// CustomField is one extra question asked on the payment page, for what only the buyer can
// tell you and Stripe does not ask for by itself: a billing address for a proper invoice, a
// purchase order number. Stripe allows three at most.
//
// The label is shown as written: a Checkout Session is one buyer's payment page, so it is
// the caller who writes it in the language that page is in (see [SessionParams.Locale]).
//
// The answers come back on the paid session: see [Session.CustomField].
type CustomField struct {
	// Key is how the answer is found again, and it has to be unique in the session.
	Key string
	// Label is what the buyer reads.
	Label string
	// Optional lets the buyer leave it empty. Think twice before asking for something
	// required: a field that only some buyers need blocks the payment of everyone else.
	Optional bool
	// MaxLength caps what can be typed. Zero leaves Stripe's own limit.
	MaxLength int
}

// SessionIDPlaceholder is what Stripe substitutes with the session id in the SuccessURL.
const SessionIDPlaceholder = "{CHECKOUT_SESSION_ID}"

// MetadataService is the metadata key that carries the project's mark.
const MetadataService = "service"

// Address is the buyer's billing address.
type Address struct {
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	PostalCode string `json:"postal_code"`
	City       string `json:"city"`
	State      string `json:"state"`
	Country    string `json:"country"`
}

// TaxID is a VAT number declared by the buyer. Stripe checks its FORMAT, not its validity:
// validation against the tax authority is asynchronous and happens later.
type TaxID struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// CustomerDetails is who paid, as far as Stripe knows.
type CustomerDetails struct {
	Email   string   `json:"email"`
	Name    string   `json:"name"`
	Address *Address `json:"address"`
	TaxIDs  []TaxID  `json:"tax_ids"`
}

// Session is a Checkout Session: the payment page of one particular charge.
//
// Only the fields that are used to verify it, or to deliver what was sold, are here.
// Adding one is cheap; reading all of them buys nothing.
type Session struct {
	ID                string            `json:"id"`
	URL               string            `json:"url"`
	Mode              string            `json:"mode"`           // must be "payment"
	Status            string            `json:"status"`         // open | complete | expired
	PaymentStatus     string            `json:"payment_status"` // paid | unpaid | no_payment_required
	AmountTotal       int64             `json:"amount_total"`
	Currency          string            `json:"currency"`
	ClientReferenceID string            `json:"client_reference_id"`
	PaymentIntent     string            `json:"payment_intent"`
	Metadata          map[string]string `json:"metadata"`
	TotalDetails      struct {
		AmountTax int64 `json:"amount_tax"`
	} `json:"total_details"`
	CustomerDetails *CustomerDetails `json:"customer_details"`
	// CustomFields are the answers to [SessionParams.CustomFields]. Read them with
	// [Session.CustomField].
	CustomFields []CustomFieldAnswer `json:"custom_fields"`
}

// CustomFieldAnswer is what the buyer wrote in a [CustomField].
type CustomFieldAnswer struct {
	Key  string `json:"key"`
	Text struct {
		Value string `json:"value"`
	} `json:"text"`
}

// NetCents is what came in for the goods, without tax.
//
// With a tax-inclusive price and no Stripe Tax, the tax Stripe reports is zero and the net
// amount is the total. It is subtracted anyway so that the day Stripe Tax is switched on,
// the amount comparison does not quietly start passing when less money arrived than was
// meant to be charged.
func (s *Session) NetCents() int64 { return s.AmountTotal - s.TotalDetails.AmountTax }

// Country is the buyer's billing country, or "" when the address was not collected.
func (s *Session) Country() string {
	if s.CustomerDetails == nil || s.CustomerDetails.Address == nil {
		return ""
	}
	return s.CustomerDetails.Address.Country
}

// Email is the address of whoever paid, or "".
func (s *Session) Email() string {
	if s.CustomerDetails == nil {
		return ""
	}
	return s.CustomerDetails.Email
}

// Name is the name of whoever paid, or "".
func (s *Session) Name() string {
	if s.CustomerDetails == nil {
		return ""
	}
	return s.CustomerDetails.Name
}

// CustomField is what the buyer answered to the custom field with this key, trimmed, or ""
// if it was left empty or never asked.
func (s *Session) CustomField(key string) string {
	for _, a := range s.CustomFields {
		if a.Key == key {
			return strings.TrimSpace(a.Text.Value)
		}
	}
	return ""
}

// FirstTaxID is the VAT number the buyer gave, or "".
func (s *Session) FirstTaxID() string {
	if s.CustomerDetails == nil || len(s.CustomerDetails.TaxIDs) == 0 {
		return ""
	}
	return s.CustomerDetails.TaxIDs[0].Value
}

// CreateSession opens a payment page.
func (c *Client) CreateSession(ctx context.Context, p SessionParams) (*Session, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("stripepay: this client has no key and cannot charge")
	}
	if strings.TrimSpace(p.Reference) == "" {
		return nil, fmt.Errorf("stripepay: SessionParams.Reference is required: " +
			"it is how the payment is reconciled with what has to be delivered")
	}
	if p.AmountCents <= 0 {
		return nil, fmt.Errorf("stripepay: SessionParams.AmountCents must be positive (it is %d)", p.AmountCents)
	}
	if p.Currency == "" {
		return nil, fmt.Errorf("stripepay: SessionParams.Currency is required")
	}
	if p.Name == "" {
		return nil, fmt.Errorf("stripepay: SessionParams.Name is required: it is what the buyer will read")
	}
	if !strings.Contains(p.SuccessURL, SessionIDPlaceholder) {
		return nil, fmt.Errorf("stripepay: SuccessURL has to carry %s, or the return page "+
			"will not know which session to show", SessionIDPlaceholder)
	}
	if p.TaxBehavior == TaxExclusive && !p.AutomaticTax {
		return nil, fmt.Errorf("stripepay: tax_behavior %q says the price is the net amount and "+
			"that somebody has to add the tax, and without AutomaticTax nobody does", TaxExclusive)
	}
	methods := p.PaymentMethodTypes
	if len(methods) == 0 {
		methods = immediateMethods
	}
	for _, m := range methods {
		if deferredMethods[m] {
			return nil, fmt.Errorf("stripepay: %q is a deferred payment method and is not accepted: "+
				"the session would close before the money arrived", m)
		}
	}
	if p.Locale != "" && !LocaleSupported(p.Locale) {
		return nil, fmt.Errorf("stripepay: Stripe does not accept the locale %q; pass one it does "+
			"or run it through Locale() first", p.Locale)
	}
	if len(p.CustomFields) > 3 {
		return nil, fmt.Errorf("stripepay: Stripe accepts at most 3 custom fields, and %d were given",
			len(p.CustomFields))
	}
	seen := map[string]bool{}
	for _, cf := range p.CustomFields {
		switch {
		case strings.TrimSpace(cf.Key) == "":
			return nil, fmt.Errorf("stripepay: a custom field needs a Key: it is how its answer is read back")
		case strings.TrimSpace(cf.Label) == "":
			return nil, fmt.Errorf("stripepay: the custom field %q needs a Label: it is what the buyer reads", cf.Key)
		case seen[cf.Key]:
			return nil, fmt.Errorf("stripepay: two custom fields with the key %q", cf.Key)
		}
		seen[cf.Key] = true
	}
	if _, reserved := p.Metadata[MetadataService]; reserved {
		return nil, fmt.Errorf("stripepay: the %q metadata key is set by the client, not by the project", MetadataService)
	}

	f := url.Values{}
	f.Set("mode", "payment")
	f.Set("success_url", p.SuccessURL)
	if p.CancelURL != "" {
		f.Set("cancel_url", p.CancelURL)
	}
	f.Set("client_reference_id", p.Reference)
	for _, m := range methods {
		f.Add("payment_method_types[]", m)
	}

	f.Set("line_items[0][quantity]", "1")
	f.Set("line_items[0][price_data][currency]", strings.ToLower(p.Currency))
	f.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(p.AmountCents, 10))
	f.Set("line_items[0][price_data][product_data][name]", p.Name)
	if p.Description != "" {
		f.Set("line_items[0][price_data][product_data][description]", p.Description)
	}
	if p.TaxCode != "" {
		f.Set("line_items[0][price_data][product_data][tax_code]", p.TaxCode)
	}
	if p.TaxBehavior != "" {
		f.Set("line_items[0][price_data][tax_behavior]", string(p.TaxBehavior))
	}

	if p.AutomaticTax {
		f.Set("automatic_tax[enabled]", "true")
	}
	if p.CollectTaxID {
		f.Set("tax_id_collection[enabled]", "true")
	}
	if p.CreateInvoice {
		f.Set("invoice_creation[enabled]", "true")
	}
	if p.CollectBillingAddress {
		f.Set("billing_address_collection", "required")
	}
	if p.CreateCustomer {
		f.Set("customer_creation", "always")
	}
	if p.CustomerEmail != "" {
		f.Set("customer_email", p.CustomerEmail)
	}
	if p.Locale != "" {
		f.Set("locale", p.Locale)
	}
	if !p.ExpiresAt.IsZero() {
		f.Set("expires_at", strconv.FormatInt(p.ExpiresAt.Unix(), 10))
	}

	for i, cf := range p.CustomFields {
		at := "custom_fields[" + strconv.Itoa(i) + "]"
		f.Set(at+"[key]", cf.Key)
		f.Set(at+"[type]", "text")
		f.Set(at+"[label][type]", "custom")
		f.Set(at+"[label][custom]", cf.Label)
		if cf.Optional {
			f.Set(at+"[optional]", "true")
		}
		if cf.MaxLength > 0 {
			f.Set(at+"[text][maximum_length]", strconv.Itoa(cf.MaxLength))
		}
	}

	// The project's mark, which is the first thing verification looks at.
	f.Set("metadata["+MetadataService+"]", c.service)
	// Sorted, so that the same call twice builds the same request and a test can compare it.
	keys := make([]string, 0, len(p.Metadata))
	for k := range p.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f.Set("metadata["+k+"]", p.Metadata[k])
	}

	idem := p.IdempotencyKey
	if idem == "" {
		idem = c.service + ":session:" + p.Reference
	}

	var s Session
	if err := c.post(ctx, "/v1/checkout/sessions", f, idem, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Session re-reads a session from the API.
//
// This is what you have to do before delivering anything, even though the webhook event
// arrives signed: the event payload is a snapshot of the moment it happened, and the state
// that counts is the state now.
func (c *Client) Session(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, fmt.Errorf("stripepay: the session id is required")
	}
	var s Session
	if err := c.get(ctx, "/v1/checkout/sessions/"+url.PathEscape(id), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

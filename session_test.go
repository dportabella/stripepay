package stripepay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeStripe stands in for Stripe and records the request it is sent, so that the call we
// would really make can be asserted without leaving the machine.
type fakeStripe struct {
	srv    *httptest.Server
	path   string
	method string
	form   url.Values
	header http.Header
	reply  string
	status int
}

func newFakeStripe(t *testing.T, reply string) *fakeStripe {
	t.Helper()
	f := &fakeStripe{reply: reply, status: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.path, f.method, f.form, f.header = r.URL.Path, r.Method, r.Form, r.Header.Clone()
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.reply))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStripe) client(t *testing.T, service string) *Client {
	t.Helper()
	c, err := New(Options{Secret: "sk_test_abc", WebhookSecret: "whsec_abc", Service: service})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURL(f.srv.URL)
	return c
}

func goodParams() SessionParams {
	return SessionParams{
		Reference:   "ord_7f3a",
		Name:        "The guide, PDF",
		AmountCents: 9000,
		Currency:    "EUR",
		SuccessURL:  "https://example.test/stripe_callback?session_id=" + SessionIDPlaceholder,
		CancelURL:   "https://example.test/guides/",
	}
}

func TestCreateSessionSendsWhatItShould(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1","url":"https://checkout.stripe.com/x"}`)
	c := f.client(t, "shop")

	p := goodParams()
	p.Description = "Ninety pages, in PDF"
	p.TaxCode = "txcd_10302000"
	p.TaxBehavior = TaxInclusive
	p.CustomerEmail = "buyer@example.test"
	p.Locale = "fr"
	p.CollectTaxID = true
	p.CreateInvoice = true
	p.CollectBillingAddress = true
	p.CreateCustomer = true
	p.ExpiresAt = time.Unix(1800000000, 0)
	p.Metadata = map[string]string{"product": "guide"}

	s, err := c.CreateSession(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "cs_test_1" {
		t.Errorf("session read wrong: %+v", s)
	}
	if f.path != "/v1/checkout/sessions" {
		t.Errorf("path = %q", f.path)
	}
	for k, want := range map[string]string{
		"mode":                                              "payment",
		"client_reference_id":                               "ord_7f3a",
		"line_items[0][quantity]":                           "1",
		"line_items[0][price_data][currency]":               "eur",
		"line_items[0][price_data][unit_amount]":            "9000",
		"line_items[0][price_data][product_data][name]":     "The guide, PDF",
		"line_items[0][price_data][product_data][tax_code]": "txcd_10302000",
		"line_items[0][price_data][tax_behavior]":           "inclusive",
		"tax_id_collection[enabled]":                        "true",
		"invoice_creation[enabled]":                         "true",
		"billing_address_collection":                        "required",
		"customer_creation":                                 "always",
		"customer_email":                                    "buyer@example.test",
		"locale":                                            "fr",
		"expires_at":                                        "1800000000",
		"metadata[service]":                                 "shop",
		"metadata[product]":                                 "guide",
	} {
		if got := f.form.Get(k); got != want {
			t.Errorf("%s = %q, wanted %q", k, got, want)
		}
	}
	// No Stripe Tax unless it is asked for: it costs money.
	if f.form.Has("automatic_tax[enabled]") {
		t.Error("automatic_tax must not be sent unless asked for: Stripe Tax charges 0.5 %")
	}
	// Immediate methods by default.
	got := f.form["payment_method_types[]"]
	if len(got) != 2 || got[0] != "card" || got[1] != "link" {
		t.Errorf("payment_method_types = %v, wanted the immediate ones", got)
	}
	// An idempotency key always, even when none is given.
	if f.header.Get("Idempotency-Key") == "" {
		t.Error("the Idempotency-Key header is missing: a double click would open two payments")
	}
	if f.header.Get("Authorization") != "Bearer sk_test_abc" {
		t.Errorf("Authorization = %q", f.header.Get("Authorization"))
	}
}

// TestCreateSessionRefusesDeferredMethods: the decision is that they are never accepted, and
// it is enforced at both ends. This is the creating end: Stripe must not even offer them.
func TestCreateSessionRefusesDeferredMethods(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1"}`)
	c := f.client(t, "shop")
	p := goodParams()
	p.PaymentMethodTypes = []string{"card", "sepa_debit"}
	_, err := c.CreateSession(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "deferred") {
		t.Fatalf("sepa_debit should be refused: %v", err)
	}
}

func TestCreateSessionValidations(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1"}`)
	c := f.client(t, "shop")

	for _, tc := range []struct {
		name    string
		mutate  func(*SessionParams)
		wantErr string
	}{
		{"no reference", func(p *SessionParams) { p.Reference = "" }, "Reference"},
		{"zero amount", func(p *SessionParams) { p.AmountCents = 0 }, "AmountCents"},
		{"negative amount", func(p *SessionParams) { p.AmountCents = -1 }, "AmountCents"},
		{"no currency", func(p *SessionParams) { p.Currency = "" }, "Currency"},
		{"no name", func(p *SessionParams) { p.Name = "" }, "Name"},
		{"SuccessURL without the session placeholder",
			func(p *SessionParams) { p.SuccessURL = "https://example.test/thanks/" }, SessionIDPlaceholder},
		{"a net price with nobody to add the tax",
			func(p *SessionParams) { p.TaxBehavior = TaxExclusive }, "without AutomaticTax"},
		{"trying to set the project mark by hand",
			func(p *SessionParams) { p.Metadata = map[string]string{MetadataService: "someone-else"} }, "set by the client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := goodParams()
			tc.mutate(&p)
			_, err := c.CreateSession(context.Background(), p)
			if err == nil {
				t.Fatal("it should fail")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the error should say %q: %v", tc.wantErr, err)
			}
		})
	}
}

func TestCreateSessionWithoutAKeyCannotCharge(t *testing.T) {
	c, _ := New(Options{Service: "shop"})
	if _, err := c.CreateSession(context.Background(), goodParams()); err == nil {
		t.Fatal("without a key there is nothing to charge with")
	}
}

func TestSessionRereadsFromTheAPI(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1","payment_status":"paid","mode":"payment",
		"status":"complete","amount_total":9000,"currency":"eur",
		"client_reference_id":"ord_7f3a","metadata":{"service":"shop"}}`)
	c := f.client(t, "shop")

	s, err := c.Session(context.Background(), "cs_test_1")
	if err != nil {
		t.Fatal(err)
	}
	if f.method != http.MethodGet || f.path != "/v1/checkout/sessions/cs_test_1" {
		t.Errorf("request = %s %s", f.method, f.path)
	}
	if err := c.Check(s, Expectation{Reference: "ord_7f3a", Currency: "EUR", MinNetCents: 9000}); err != nil {
		t.Errorf("the re-read session should pass the check: %v", err)
	}
	if _, err := c.Session(context.Background(), ""); err == nil {
		t.Error("without an id it should fail")
	}
}

func TestStripeErrorsAreReported(t *testing.T) {
	f := newFakeStripe(t, `{"error":{"message":"Invalid request"}}`)
	f.status = 400
	c := f.client(t, "shop")
	_, err := c.Session(context.Background(), "cs_1")
	if err == nil || !strings.Contains(err.Error(), "Invalid request") {
		t.Fatalf("Stripe's error has to be propagated: %v", err)
	}
}

// TestANonExistentSessionIsSaidToBeNonExistent: "it does not exist" and "Stripe is not
// answering" lead to different decisions. Without telling them apart, a return page handed
// an invented identifier ends up claiming a payment is being confirmed.
func TestANonExistentSessionIsSaidToBeNonExistent(t *testing.T) {
	f := newFakeStripe(t, `{"error":{"code":"resource_missing","message":"No such checkout.session: 'cs_no'"}}`)
	f.status = 404
	c := f.client(t, "shop")

	_, err := c.Session(context.Background(), "cs_no")
	if !errors.Is(err, ErrNoSuchObject) {
		t.Fatalf("it should be ErrNoSuchObject: %v", err)
	}

	// Any other error is NOT: it cannot be mistaken for "it does not exist".
	f.status, f.reply = 500, `{"error":{"message":"api error"}}`
	if _, err := c.Session(context.Background(), "cs_1"); errors.Is(err, ErrNoSuchObject) {
		t.Errorf("a server error does not mean it does not exist: %v", err)
	}
}

func TestMissingPermissionIsSaidPlainly(t *testing.T) {
	f := newFakeStripe(t, `{"error":{"message":"This key does not have the required permissions."}}`)
	f.status = 403
	c := f.client(t, "shop")
	_, err := c.ListEndpoints(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lacks a permission") {
		t.Fatalf("the usual restricted-key case has to be said plainly: %v", err)
	}
}

func TestRefundSendsAmountOnlyWhenPartial(t *testing.T) {
	f := newFakeStripe(t, `{"id":"re_1","amount":3250,"currency":"eur","status":"succeeded"}`)
	c := f.client(t, "shop")

	r, err := c.Refund(context.Background(), RefundParams{
		PaymentIntent: "pi_123", AmountCents: 3250, Reason: "cancelled", IdempotencyKey: "idem-1"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "re_1" || r.Amount != 3250 {
		t.Errorf("refund read wrong: %+v", r)
	}
	if f.form.Get("amount") != "3250" {
		t.Errorf("partial refund: amount = %q", f.form.Get("amount"))
	}
	if f.form.Get("metadata[reason]") != "cancelled" || f.form.Get("metadata[service]") != "shop" {
		t.Errorf("refund metadata: %v", f.form)
	}

	// Full: Stripe reads the absence of "amount" as "give everything back".
	if _, err := c.Refund(context.Background(), RefundParams{
		PaymentIntent: "pi_123", IdempotencyKey: "idem-2"}); err != nil {
		t.Fatal(err)
	}
	if f.form.Has("amount") {
		t.Errorf("full refund: there should be no amount, there is %q", f.form.Get("amount"))
	}

	if _, err := c.Refund(context.Background(), RefundParams{}); err == nil {
		t.Error("without a payment_intent it should fail")
	}
}

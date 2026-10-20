package stripepay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func subscriptionParams() SessionParams {
	p := goodParams()
	p.Reference = "sub_ref"
	p.Name = "Backups, quarterly"
	p.AmountCents = 1900
	p.Recurring = &Recurring{Interval: "month", Count: 3}
	p.Metadata = map[string]string{"plan": "quarterly"}
	p.CreateCustomer = true
	return p
}

func TestASubscriptionSessionIsSentAsOne(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1","url":"https://checkout.stripe.com/x","mode":"subscription"}`)
	c := f.client(t, "shop")
	if _, err := c.CreateSession(context.Background(), subscriptionParams()); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"mode": "subscription",
		"line_items[0][price_data][recurring][interval]":       "month",
		"line_items[0][price_data][recurring][interval_count]": "3",
		"line_items[0][price_data][unit_amount]":               "1900",
		"subscription_data[metadata][service]":                 "shop",
		"subscription_data[metadata][reference]":               "sub_ref",
		"subscription_data[metadata][plan]":                    "quarterly",
		"metadata[service]":                                    "shop",
		"client_reference_id":                                  "sub_ref",
	} {
		if got := f.form.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if f.form.Has("customer_creation") {
		t.Error("a subscription always has a customer and Stripe refuses customer_creation in that mode")
	}
}

func TestAOneOffPaymentHasNoSubscriptionData(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1"}`)
	c := f.client(t, "shop")
	if _, err := c.CreateSession(context.Background(), goodParams()); err != nil {
		t.Fatal(err)
	}
	if f.form.Get("mode") != "payment" || f.form.Has("subscription_data[metadata][service]") || f.form.Has("line_items[0][price_data][recurring][interval]") {
		t.Fatalf("a one-off payment must be exactly what it was: %v", f.form)
	}
}

func TestBadRecurringIsRefusedBeforeStripeIsCalled(t *testing.T) {
	f := newFakeStripe(t, `{}`)
	c := f.client(t, "shop")
	for name, change := range map[string]func(*SessionParams){
		"interval":          func(p *SessionParams) { p.Recurring.Interval = "fortnight" },
		"no interval":       func(p *SessionParams) { p.Recurring.Interval = "" },
		"negative count":    func(p *SessionParams) { p.Recurring.Count = -1 },
		"too large a count": func(p *SessionParams) { p.Recurring.Count = 53 },
		"invoice creation":  func(p *SessionParams) { p.CreateInvoice = true },
		"deferred method":   func(p *SessionParams) { p.PaymentMethodTypes = []string{"sepa_debit"} },
	} {
		p := subscriptionParams()
		change(&p)
		if _, err := c.CreateSession(context.Background(), p); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if f.path != "" {
		t.Fatalf("Stripe was called: %s", f.path)
	}
}

// Both shapes of the API: up to 2025-03-31 the invoice carries `subscription` and the
// subscription its `current_period_end`; from 2025-04-30 they moved to `parent` and to the items.
const (
	oldInvoice = `{"id":"in_1","status":"paid","currency":"eur","amount_paid":1900,"tax":0,"billing_reason":"subscription_cycle","subscription":"sub_1",
		"subscription_details":{"metadata":{"service":"shop","reference":"sub_ref"}},"lines":{"data":[{"period":{"start":100,"end":2000}}]}}`
	newInvoice = `{"id":"in_2","status":"paid","currency":"eur","amount_paid":2299,"total_taxes":[{"amount":399}],"billing_reason":"subscription_cycle",
		"parent":{"subscription_details":{"subscription":"sub_1","metadata":{"service":"shop","reference":"sub_ref"}}},"lines":{"data":[{"period":{"start":100,"end":3000}}]}}`
	oldSub = `{"id":"sub_1","customer":"cus_1","status":"active","current_period_end":2000,"metadata":{"service":"shop","reference":"sub_ref"}}`
	newSub = `{"id":"sub_1","customer":"cus_1","status":"active","items":{"data":[{"current_period_end":3000}]},"metadata":{"service":"shop","reference":"sub_ref"}}`
)

func TestBothShapesOfTheAPIAreRead(t *testing.T) {
	f := newFakeStripe(t, oldSub)
	c := f.client(t, "shop")
	s, err := c.Subscription(context.Background(), "sub_1")
	if err != nil || s.PeriodEnd() != time.Unix(2000, 0).UTC() || f.path != "/v1/subscriptions/sub_1" {
		t.Fatalf("old: %+v, %v, %s", s, err, f.path)
	}
	f.reply = newSub
	if s, _ = c.Subscription(context.Background(), "sub_1"); s.PeriodEnd() != time.Unix(3000, 0).UTC() {
		t.Fatalf("new: %v", s.PeriodEnd())
	}
	f.reply = oldInvoice
	i, err := c.Invoice(context.Background(), "in_1")
	if err != nil || i.SubscriptionID() != "sub_1" || i.NetCents() != 1900 || i.PaidThrough() != time.Unix(2000, 0).UTC() {
		t.Fatalf("old invoice: %+v, %v", i, err)
	}
	f.reply = newInvoice
	i, _ = c.Invoice(context.Background(), "in_2")
	if i.SubscriptionID() != "sub_1" || i.NetCents() != 1900 || i.TaxCents() != 399 || i.SubscriptionMetadata()["service"] != "shop" {
		t.Fatalf("new invoice: %+v", i)
	}
}

func TestCheckInvoice(t *testing.T) {
	c, _ := New(Options{Secret: "sk_test_x", Service: "shop"})
	good := func() (*Invoice, *Subscription) {
		return &Invoice{ID: "in_1", Status: "paid", Currency: "eur", AmountPaid: 1900, Subscription: "sub_1"},
			&Subscription{ID: "sub_1", Status: "active", Metadata: map[string]string{"service": "shop", "reference": "sub_ref"}}
	}
	exp := InvoiceExpectation{Reference: "sub_ref", Currency: "EUR", MinNetCents: 1900}
	if inv, sub := good(); c.CheckInvoice(inv, sub, exp) != nil {
		t.Fatal("a good invoice must pass")
	}
	for name, tc := range map[string]struct {
		change func(*Invoice, *Subscription)
		want   error
	}{
		"a neighbour's":        {func(i *Invoice, s *Subscription) { s.Metadata["service"] = "booking" }, ErrForeign},
		"another subscription": {func(i *Invoice, s *Subscription) { i.Subscription = "sub_9" }, ErrReference},
		"another reference":    {func(i *Invoice, s *Subscription) { s.Metadata["reference"] = "other" }, ErrReference},
		"open":                 {func(i *Invoice, s *Subscription) { i.Status = "open" }, ErrNotPaid},
		"void":                 {func(i *Invoice, s *Subscription) { i.Status = "void" }, ErrExpired},
		"uncollectible":        {func(i *Invoice, s *Subscription) { i.Status = "uncollectible" }, ErrExpired},
		"currency":             {func(i *Invoice, s *Subscription) { i.Currency = "usd" }, ErrCurrency},
		"amount":               {func(i *Invoice, s *Subscription) { i.AmountPaid = 1000 }, ErrAmount},
		"tax is not revenue":   {func(i *Invoice, s *Subscription) { i.Tax = 400 }, ErrAmount},
	} {
		inv, sub := good()
		tc.change(inv, sub)
		if err := c.CheckInvoice(inv, sub, exp); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if err := c.CheckInvoice(nil, nil, exp); !errors.Is(err, ErrForeign) {
		t.Errorf("nothing: %v", err)
	}
}

func TestCancelResumeAndPortal(t *testing.T) {
	f := newFakeStripe(t, `{"id":"sub_1","status":"active","cancel_at_period_end":true,"url":"https://billing.stripe.com/p/x"}`)
	c := f.client(t, "shop")
	ctx := context.Background()
	if s, err := c.CancelSubscription(ctx, "sub_1", true); err != nil || !s.CancelAtPeriodEnd || f.method != "POST" || f.path != "/v1/subscriptions/sub_1" || f.form.Get("cancel_at_period_end") != "true" {
		t.Fatalf("at the period end: %v %s %s %v", err, f.method, f.path, f.form)
	}
	if _, err := c.CancelSubscription(ctx, "sub_1", false); err != nil || f.method != "DELETE" || f.path != "/v1/subscriptions/sub_1" {
		t.Fatalf("now: %v %s %s", err, f.method, f.path)
	}
	if _, err := c.ResumeSubscription(ctx, "sub_1"); err != nil || f.form.Get("cancel_at_period_end") != "false" {
		t.Fatalf("resume: %v %v", err, f.form)
	}
	u, err := c.PortalSession(ctx, "cus_1", "https://example.test/account")
	if err != nil || u != "https://billing.stripe.com/p/x" || f.path != "/v1/billing_portal/sessions" || f.form.Get("customer") != "cus_1" || f.form.Get("return_url") == "" {
		t.Fatalf("portal: %q %v %s %v", u, err, f.path, f.form)
	}
	if _, err := c.PortalSession(ctx, "", "x"); err == nil {
		t.Fatal("a portal needs a customer")
	}
}

// subscriptionProbe is a Handler with a fake Stripe that answers by path, to see what reaches OnSubscription.
type subscriptionProbe struct {
	h      *Handler
	calls  []string
	got    []*SubscriptionEvent
	errs   []error
	claims map[string]bool
}

func newSubscriptionProbe(t *testing.T, service, subReply, invReply string) *subscriptionProbe {
	t.Helper()
	p := &subscriptionProbe{claims: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls = append(p.calls, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/v1/subscriptions/") {
			w.Write([]byte(subReply))
		} else {
			w.Write([]byte(invReply))
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := New(Options{Secret: "sk_test_abc", WebhookSecret: whsec, Service: service})
	c.SetBaseURL(srv.URL)
	handle := func(_ context.Context, _ *Event, s *SubscriptionEvent) error {
		p.got = append(p.got, s)
		return nil
	}
	p.h = &Handler{Client: c, OnSubscription: map[string]func(context.Context, *Event, *SubscriptionEvent) error{
		"invoice.paid": handle, "customer.subscription.deleted": handle},
		Seen: func(_ context.Context, id, _ string) (bool, error) {
			seen := p.claims[id]
			p.claims[id] = true
			return seen, nil
		},
		OnError: func(_ context.Context, _ *Event, err error) { p.errs = append(p.errs, err) }}
	return p
}

func (p *subscriptionProbe) deliver(t *testing.T, id, typ, object string) int {
	t.Helper()
	body := []byte(`{"id":"` + id + `","type":"` + typ + `","data":{"object":` + object + `}}`)
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", Sign(whsec, time.Now(), body))
	w := httptest.NewRecorder()
	p.h.ServeHTTP(w, req)
	return w.Code
}

func TestARenewalInvoiceReachesTheProjectReReadFromTheAPI(t *testing.T) {
	p := newSubscriptionProbe(t, "shop", oldSub, oldInvoice)
	// The payload says "open"; the API says paid: the API is what counts.
	stale := strings.Replace(oldInvoice, `"status":"paid"`, `"status":"open"`, 1)
	if code := p.deliver(t, "evt_1", "invoice.paid", stale); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(p.got) != 1 || p.got[0].Invoice.Status != "paid" || p.got[0].Subscription.ID != "sub_1" || len(p.errs) != 0 {
		t.Fatalf("got %+v, errs %v", p.got, p.errs)
	}
	if p.deliver(t, "evt_1", "invoice.paid", stale); len(p.got) != 1 {
		t.Fatal("the same event twice must be handled once")
	}
}

func TestANeighboursInvoiceIsDroppedInSilence(t *testing.T) {
	neighbour := strings.NewReplacer(`"service":"shop"`, `"service":"booking"`)
	p := newSubscriptionProbe(t, "shop", neighbour.Replace(oldSub), neighbour.Replace(oldInvoice))
	// Its payload says whose it is: not even a call to the API.
	p.deliver(t, "evt_1", "invoice.paid", neighbour.Replace(oldInvoice))
	// One whose payload does not say: found out after one call, still in silence.
	bare := `{"id":"in_3","status":"paid","subscription":"sub_1"}`
	p.deliver(t, "evt_2", "invoice.paid", bare)
	if len(p.got) != 0 || len(p.errs) != 0 {
		t.Fatalf("a neighbour's invoice is not an incident: %v %v", p.got, p.errs)
	}
	if len(p.calls) != 1 || p.calls[0] != "/v1/subscriptions/sub_1" {
		t.Fatalf("calls = %v", p.calls)
	}
}

func TestAnInvoiceOfNoSubscriptionIsDropped(t *testing.T) {
	p := newSubscriptionProbe(t, "shop", oldSub, oldInvoice)
	p.deliver(t, "evt_1", "invoice.paid", `{"id":"in_9","status":"paid"}`)
	if len(p.got) != 0 || len(p.calls) != 0 || len(p.errs) != 0 {
		t.Fatalf("got %v calls %v errs %v", p.got, p.calls, p.errs)
	}
}

func TestACancelledSubscriptionReachesTheProject(t *testing.T) {
	canceled := strings.Replace(oldSub, `"status":"active"`, `"status":"canceled"`, 1)
	p := newSubscriptionProbe(t, "shop", canceled, oldInvoice)
	p.deliver(t, "evt_1", "customer.subscription.deleted", canceled)
	if len(p.got) != 1 || p.got[0].Subscription.Status != "canceled" || p.got[0].Invoice != nil {
		t.Fatalf("got %+v", p.got)
	}
	// And a neighbour's cancellation does not.
	other := strings.Replace(canceled, `"service":"shop"`, `"service":"booking"`, 1)
	p.deliver(t, "evt_2", "customer.subscription.deleted", other)
	if len(p.got) != 1 || len(p.errs) != 0 {
		t.Fatalf("got %d, errs %v", len(p.got), p.errs)
	}
}

func TestATrialSubscriptionWithAnUpfrontChargeIsSentAsOne(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1","mode":"subscription"}`)
	c := f.client(t, "shop")
	p := subscriptionParams()
	p.Recurring = &Recurring{Interval: "month", Count: 3, TrialEnd: time.Now().Add(10 * 24 * time.Hour), UpfrontCents: 1900}
	p.TaxBehavior = TaxInclusive
	if _, err := c.CreateSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"subscription_data[trial_end]":                   f.form.Get("subscription_data[trial_end]"),
		"payment_method_collection":                      "always",
		"line_items[0][price_data][recurring][interval]": "month",
		"line_items[1][price_data][unit_amount]":         "1900",
		"line_items[1][price_data][tax_behavior]":        "inclusive",
		"line_items[1][quantity]":                        "1",
	} {
		if got := f.form.Get(k); got != want || got == "" {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if f.form.Has("line_items[1][price_data][recurring][interval]") {
		t.Error("the upfront charge is a one-off line")
	}
}

func TestBadTrialsAreRefusedBeforeStripeIsCalled(t *testing.T) {
	f := newFakeStripe(t, `{}`)
	c := f.client(t, "shop")
	for name, rec := range map[string]*Recurring{
		"too soon":                {Interval: "month", TrialEnd: time.Now().Add(24 * time.Hour)},
		"too far":                 {Interval: "month", TrialEnd: time.Now().Add(3 * 365 * 24 * time.Hour)},
		"upfront without a trial": {Interval: "month", UpfrontCents: 1900},
		"negative upfront":        {Interval: "month", TrialEnd: time.Now().Add(10 * 24 * time.Hour), UpfrontCents: -1},
	} {
		p := subscriptionParams()
		p.Recurring = rec
		if _, err := c.CreateSession(context.Background(), p); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if f.path != "" {
		t.Fatalf("Stripe was called: %s", f.path)
	}
}

func TestAddInvoiceItem(t *testing.T) {
	f := newFakeStripe(t, `{"id":"ii_1"}`)
	c := f.client(t, "shop")
	err := c.AddInvoiceItem(context.Background(), AddInvoiceItemParams{Customer: "cus_1", Invoice: "in_1", AmountCents: -500, Currency: "EUR", Description: "adjustment", IdempotencyKey: "k1"})
	if err != nil || f.path != "/v1/invoiceitems" || f.form.Get("invoice") != "in_1" || f.form.Get("amount") != "-500" || f.form.Get("currency") != "eur" ||
		f.form.Get("customer") != "cus_1" || f.header.Get("Idempotency-Key") != "k1" || f.form.Get("metadata[service]") != "shop" {
		t.Fatalf("%v %s %v %v", err, f.path, f.form, f.header)
	}
	for name, p := range map[string]AddInvoiceItemParams{
		"nothing to add": {Customer: "c", Invoice: "i", Currency: "EUR", IdempotencyKey: "k"},
		"no key":         {Customer: "c", Invoice: "i", Currency: "EUR", AmountCents: 1},
		"no invoice":     {Customer: "c", Currency: "EUR", AmountCents: 1, IdempotencyKey: "k"},
	} {
		if err := c.AddInvoiceItem(context.Background(), p); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

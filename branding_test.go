package stripepay

import (
	"context"
	"strings"
	"testing"
)

func TestTheNameOnThePaymentPageIsMandatory(t *testing.T) {
	for _, o := range []Options{
		{Secret: "sk_test_x", Service: "shop"},
		{Secret: "sk_test_x", Service: "shop", DisplayName: "   "},
		{Service: "shop"},
	} {
		if _, err := New(o); err == nil || !strings.Contains(err.Error(), "DisplayName") {
			t.Errorf("%+v should fail naming DisplayName: %v", o, err)
		}
	}
}

func TestADisplayNameAndTheAccountNameContradictEachOther(t *testing.T) {
	_, err := New(Options{Secret: "sk_test_x", Service: "shop", DisplayName: "Shop", KeepAccountName: true})
	if err == nil || !strings.Contains(err.Error(), "KeepAccountName") {
		t.Fatalf("both set should fail: %v", err)
	}
}

func TestEverySessionCarriesTheDisplayNameAndTheVersionThatHasIt(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1"}`)
	c, err := New(Options{Secret: "sk_test_abc", Service: "shop", DisplayName: " Shop Name "})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURL(f.srv.URL)
	for name, recurring := range map[string]*Recurring{"one-off": nil, "subscription": {Interval: "month"}} {
		p := goodParams()
		p.Recurring = recurring
		if _, err := c.CreateSession(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if got := f.form.Get("branding_settings[display_name]"); got != "Shop Name" {
			t.Errorf("%s: display_name = %q", name, got)
		}
		if got := f.header.Get("Stripe-Version"); got != "2025-09-30.clover" {
			t.Errorf("%s: Stripe-Version = %q", name, got)
		}
	}
	// Only the request that needs it says a version: what is read keeps the account's.
	if _, err := c.Session(context.Background(), "cs_test_1"); err != nil {
		t.Fatal(err)
	}
	if got := f.header.Get("Stripe-Version"); got != "" {
		t.Errorf("reading a session must not pin a version: %q", got)
	}
}

func TestKeepAccountNameSendsNoBranding(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1"}`)
	c, err := New(Options{Secret: "sk_test_abc", Service: "shop", KeepAccountName: true, Account: "acct_1Abc"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURL(f.srv.URL)
	if _, err := c.CreateSession(context.Background(), goodParams()); err != nil {
		t.Fatal(err)
	}
	if f.form.Has("branding_settings[display_name]") || f.header.Get("Stripe-Version") != "" {
		t.Errorf("the account's own name must show: %v %v", f.form, f.header)
	}
}

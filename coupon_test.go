package stripepay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeCoupons is Stripe's coupon endpoint: it holds at most one coupon and records what was posted.
func fakeCoupons(t *testing.T, existing string) (*Client, *[]url.Values) {
	t.Helper()
	var posted []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/coupons/case-20" && existing != "":
			io.WriteString(w, existing)
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"resource_missing","message":"No such coupon"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/coupons":
			posted = append(posted, r.PostForm)
			io.WriteString(w, `{"id":"case-20","percent_off":20,"duration":"once","valid":true}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{Secret: "sk_test_abc", Service: "shop", DisplayName: "Shop Name"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURL(srv.URL)
	return c, &posted
}

var caseCoupon = CouponParams{ID: "case-20", Name: "20% for your story", PercentOff: 20}

func TestAMissingCouponIsCreatedWithoutARedemptionLimit(t *testing.T) {
	c, posted := fakeCoupons(t, "")
	if err := c.EnsureCoupon(context.Background(), caseCoupon); err != nil {
		t.Fatal(err)
	}
	if len(*posted) != 1 {
		t.Fatalf("posted %d times", len(*posted))
	}
	f := (*posted)[0]
	for k, want := range map[string]string{"id": "case-20", "name": "20% for your story", "percent_off": "20", "duration": "once", "metadata[" + MetadataService + "]": "shop"} {
		if got := f.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if f.Has("max_redemptions") {
		t.Error("a coupon must not be limited in redemptions")
	}
}

func TestAMatchingCouponIsLeftAlone(t *testing.T) {
	c, posted := fakeCoupons(t, `{"id":"case-20","percent_off":20.0,"duration":"once","valid":true}`)
	if err := c.EnsureCoupon(context.Background(), caseCoupon); err != nil {
		t.Fatal(err)
	}
	if len(*posted) != 0 {
		t.Error("an existing coupon must not be written")
	}
}

func TestACouponThatSaysSomethingElseIsAnError(t *testing.T) {
	for name, existing := range map[string]string{
		"another percentage": `{"id":"case-20","percent_off":50,"duration":"once","valid":true}`,
		"another duration":   `{"id":"case-20","percent_off":20,"duration":"forever","valid":true}`,
		"no longer valid":    `{"id":"case-20","percent_off":20,"duration":"once","valid":false}`,
		"limited":            `{"id":"case-20","percent_off":20,"duration":"once","valid":true,"max_redemptions":10}`,
		"expiring":           `{"id":"case-20","percent_off":20,"duration":"once","valid":true,"redeem_by":1900000000}`,
	} {
		c, _ := fakeCoupons(t, existing)
		if err := c.EnsureCoupon(context.Background(), caseCoupon); err == nil || !strings.Contains(err.Error(), "case-20") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestACouponNeedsAnIdAndASensiblePercentage(t *testing.T) {
	c, _ := fakeCoupons(t, "")
	for _, p := range []CouponParams{{Name: "x", PercentOff: 20}, {ID: "a", PercentOff: 20}, {ID: "a", Name: "x"}, {ID: "a", Name: "x", PercentOff: 100}} {
		if err := c.EnsureCoupon(context.Background(), p); err == nil {
			t.Errorf("%+v was accepted", p)
		}
	}
}

package stripepay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// expiring stands in for Stripe with one session in the given state: it expires an open one and refuses any other, as Stripe does.
func expiring(t *testing.T, state string, broken bool) (*Client, *[]string) {
	t.Helper()
	asked := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*asked = append(*asked, r.Method+" "+r.URL.EscapedPath())
		switch {
		case r.URL.Path == "/v1/checkout/sessions/cs_none/expire" || r.URL.Path == "/v1/checkout/sessions/cs_none":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"resource_missing","message":"No such checkout.session"}}`))
		case broken:
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
		case r.Method == "POST" && r.URL.Path == "/v1/checkout/sessions/cs_1/expire":
			if state != "open" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"message":"Only Checkout Sessions with a status in [\"open\"] can be expired."}}`))
				return
			}
			state = "expired"
			_, _ = w.Write([]byte(`{"id":"cs_1","status":"expired","payment_status":"unpaid"}`))
		case r.Method == "GET" && r.URL.Path == "/v1/checkout/sessions/cs_1":
			_, _ = w.Write([]byte(`{"id":"cs_1","status":"` + state + `"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{Secret: "sk_test_x", WebhookSecret: "whsec_x", Service: "svc", DisplayName: "Svc"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURL(srv.URL)
	return c, asked
}

func TestAnOpenSessionIsExpired(t *testing.T) {
	c, asked := expiring(t, "open", false)
	s, err := c.ExpireSession(context.Background(), "cs_1")
	if err != nil || s.Status != "expired" || len(*asked) != 1 || (*asked)[0] != "POST /v1/checkout/sessions/cs_1/expire" {
		t.Fatal(s, err, *asked)
	}
}

func TestASessionThatIsNoLongerOpenSaysHowItEnded(t *testing.T) {
	for _, state := range []string{"expired", "complete"} {
		c, _ := expiring(t, state, false)
		s, err := c.ExpireSession(context.Background(), "cs_1")
		if err != nil || s.Status != state {
			t.Fatal(state, s, err)
		}
	}
}

func TestASessionThatMayStillBePaidIsAnError(t *testing.T) {
	c, _ := expiring(t, "open", true)
	if s, err := c.ExpireSession(context.Background(), "cs_1"); err == nil {
		t.Fatal("no error", s)
	}
	if _, err := c.ExpireSession(context.Background(), "cs_none"); !errors.Is(err, ErrNoSuchObject) {
		t.Fatal(err)
	}
	if _, err := c.ExpireSession(context.Background(), ""); err == nil {
		t.Fatal("an empty id")
	}
}

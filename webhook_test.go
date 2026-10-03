package stripepay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const whsec = "whsec_testsecret"

func webhookClient(t *testing.T, service string) *Client {
	t.Helper()
	c, err := New(Options{Secret: "sk_test_abc", WebhookSecret: whsec, Service: service})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerifyWebhook(t *testing.T) {
	c := webhookClient(t, "shop")
	now := time.Now()
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":"cs_1"}}}`)

	t.Run("a good signature", func(t *testing.T) {
		ev, err := c.VerifyWebhook(body, Sign(whsec, now, body), now, DefaultTolerance)
		if err != nil {
			t.Fatalf("it should verify: %v", err)
		}
		if ev.ID != "evt_1" || ev.Type != "checkout.session.completed" {
			t.Fatalf("event read wrong: %+v", ev)
		}
		if ev.ObjectID() != "cs_1" {
			t.Errorf("ObjectID = %q", ev.ObjectID())
		}
		if string(ev.Raw()) != string(body) {
			t.Error("Raw has to return the body exactly as it arrived")
		}
	})

	t.Run("signed with another secret", func(t *testing.T) {
		if _, err := c.VerifyWebhook(body, Sign("whsec_another", now, body), now, DefaultTolerance); err == nil {
			t.Fatal("it should be rejected")
		}
	})

	t.Run("a tampered body", func(t *testing.T) {
		sig := Sign(whsec, now, body)
		bad := []byte(`{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":"cs_OTHER"}}}`)
		if _, err := c.VerifyWebhook(bad, sig, now, DefaultTolerance); err == nil {
			t.Fatal("it should be rejected: the signature is over the bytes, not over the meaning")
		}
	})

	t.Run("a replayed old event", func(t *testing.T) {
		old := now.Add(-10 * time.Minute)
		if _, err := c.VerifyWebhook(body, Sign(whsec, old, body), now, DefaultTolerance); err == nil {
			t.Fatal("a timestamp outside the tolerance should be rejected")
		}
	})

	t.Run("a malformed header", func(t *testing.T) {
		for _, h := range []string{"", "t=123", "v1=abc", "anything at all"} {
			if _, err := c.VerifyWebhook(body, h, now, DefaultTolerance); err == nil {
				t.Errorf("header %q should fail", h)
			}
		}
	})

	t.Run("without a secret nothing can be checked", func(t *testing.T) {
		without, _ := New(Options{Secret: "sk_test_abc", Service: "shop"})
		if _, err := without.VerifyWebhook(body, Sign(whsec, now, body), now, DefaultTolerance); err == nil {
			t.Fatal("without the webhook secret no event may be accepted")
		}
	})

	t.Run("a body that is not JSON", func(t *testing.T) {
		bad := []byte(`not json`)
		if _, err := c.VerifyWebhook(bad, Sign(whsec, now, bad), now, DefaultTolerance); err == nil {
			t.Fatal("it should fail")
		}
	})
}

// handlerProbe builds a Handler with a fake Stripe behind it and records what happens.
type handlerProbe struct {
	h        *Handler
	fake     *fakeStripe
	claimed  map[string]bool
	handled  []string
	sessions []*Session
	errs     []error
	handleIn func() error
}

func newHandlerProbe(t *testing.T, service, sessionReply string, events ...string) *handlerProbe {
	t.Helper()
	f := newFakeStripe(t, sessionReply)
	c := f.client(t, service)
	c.webhookSecret = whsec

	p := &handlerProbe{fake: f, claimed: map[string]bool{}}
	on := map[string]func(context.Context, *Event, *Session) error{}
	for _, e := range events {
		on[e] = func(ctx context.Context, ev *Event, s *Session) error {
			p.handled = append(p.handled, ev.Type)
			p.sessions = append(p.sessions, s)
			if p.handleIn != nil {
				return p.handleIn()
			}
			return nil
		}
	}
	p.h = &Handler{
		Client: c,
		Seen: func(ctx context.Context, id, typ string) (bool, error) {
			if p.claimed[id] {
				return true, nil
			}
			p.claimed[id] = true
			return false, nil
		},
		On:      on,
		OnError: func(ctx context.Context, ev *Event, err error) { p.errs = append(p.errs, err) },
		// Background left nil: the work runs inside the request, so the test does not wait.
	}
	return p
}

func (p *handlerProbe) post(t *testing.T, body string, sign bool) int {
	t.Helper()
	now := time.Now()
	sig := Sign(whsec, now, []byte(body))
	if !sign {
		sig = Sign("whsec_another", now, []byte(body))
	}
	r := httptest.NewRequest(http.MethodPost, "/webhook/stripe", strings.NewReader(body))
	r.Header.Set("Stripe-Signature", sig)
	w := httptest.NewRecorder()
	p.h.ServeHTTP(w, r)
	return w.Code
}

func eventBody(id, typ, sessionID, service string) string {
	return fmt.Sprintf(`{"id":%q,"type":%q,"data":{"object":{"id":%q,"metadata":{"service":%q}}}}`,
		id, typ, sessionID, service)
}

const paidSessionJSON = `{"id":"cs_1","mode":"payment","status":"complete","payment_status":"paid",
	"amount_total":9000,"currency":"eur","client_reference_id":"ord_1","payment_intent":"pi_1",
	"metadata":{"service":"shop"}}`

func TestHandlerDeliversOurOwnPaidSession(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")

	if code := p.post(t, eventBody("evt_1", "checkout.session.completed", "cs_1", "shop"), true); code != 200 {
		t.Fatalf("code %d", code)
	}
	if len(p.handled) != 1 {
		t.Fatalf("the handler should have run once, it ran %d times", len(p.handled))
	}
	// The session that reaches the handler must be the one RE-READ from the API, not the
	// one in the event payload: that one is a snapshot of the moment it happened.
	if p.fake.path != "/v1/checkout/sessions/cs_1" {
		t.Errorf("it did not re-read the session from the API, it asked for %q", p.fake.path)
	}
	s := p.sessions[0]
	if s.PaymentStatus != "paid" || s.ClientReferenceID != "ord_1" {
		t.Errorf("the handler's session is not the re-read one: %+v", s)
	}
	if len(p.errs) != 0 {
		t.Errorf("there should be no alert: %v", p.errs)
	}
}

// TestHandlerIsSilentAboutTheNeighbour: the Stripe account is shared and Stripe delivers
// every event to every endpoint. Another project's sale is answered 200 and left alone, and
// above all NOT alerted: that would be a false alarm on every charge the neighbour takes.
func TestHandlerIsSilentAboutTheNeighbour(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")

	if code := p.post(t, eventBody("evt_2", "checkout.session.completed", "cs_9", "booking"), true); code != 200 {
		t.Fatalf("code %d", code)
	}
	if len(p.handled) != 0 {
		t.Error("the handler should not have run")
	}
	if len(p.errs) != 0 {
		t.Errorf("nothing should have been alerted: %v", p.errs)
	}
	if p.fake.path != "" {
		t.Errorf("nothing should have been re-read from the API either: %q", p.fake.path)
	}
}

func TestHandlerHandlesEachEventOnce(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	body := eventBody("evt_3", "checkout.session.completed", "cs_1", "shop")

	for i := 0; i < 3; i++ {
		if code := p.post(t, body, true); code != 200 {
			t.Fatalf("code %d", code)
		}
	}
	if len(p.handled) != 1 {
		t.Fatalf("three redeliveries should do the work once, it was done %d times", len(p.handled))
	}
}

func TestHandlerRejectsABadSignature(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	if code := p.post(t, eventBody("evt_4", "checkout.session.completed", "cs_1", "shop"), false); code != 400 {
		t.Fatalf("code %d, wanted 400", code)
	}
	if len(p.handled) != 0 {
		t.Error("nothing should have been done")
	}
	if len(p.errs) != 1 {
		t.Errorf("a signature that does not match does have to be alerted: %v", p.errs)
	}
}

func TestHandlerIgnoresEventsNobodyHandles(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	if code := p.post(t, eventBody("evt_5", "checkout.session.expired", "cs_1", "shop"), true); code != 200 {
		t.Fatalf("code %d", code)
	}
	if len(p.handled) != 0 || len(p.errs) != 0 {
		t.Errorf("a type the project does not handle is ignored in silence: %v %v", p.handled, p.errs)
	}
}

func TestHandlerReportsWhatTheProjectCouldNotDo(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	p.handleIn = func() error { return errors.New("the file could not be stamped") }

	if code := p.post(t, eventBody("evt_6", "checkout.session.completed", "cs_1", "shop"), true); code != 200 {
		t.Fatalf("code %d", code)
	}
	if len(p.errs) != 1 || !strings.Contains(p.errs[0].Error(), "stamped") {
		t.Fatalf("the project's error has to reach OnError: %v", p.errs)
	}
}

func TestHandlerReportsWhenItCannotRereadTheSession(t *testing.T) {
	p := newHandlerProbe(t, "shop", `{"error":{"message":"No such checkout session"}}`, "checkout.session.completed")
	p.fake.status = 404

	if code := p.post(t, eventBody("evt_7", "checkout.session.completed", "cs_1", "shop"), true); code != 200 {
		t.Fatalf("code %d", code)
	}
	if len(p.handled) != 0 {
		t.Error("without being able to re-read the session, nothing is delivered")
	}
	if len(p.errs) != 1 || !strings.Contains(p.errs[0].Error(), "re-reading") {
		t.Fatalf("it should have alerted that the session could not be re-read: %v", p.errs)
	}
}

func TestHandlerNeedsToBeConfigured(t *testing.T) {
	for _, h := range []*Handler{
		{},
		{Client: webhookClient(t, "shop")},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/w", strings.NewReader("{}")))
		if w.Code != 500 {
			t.Errorf("a half-configured Handler has to fail visibly, it returned %d", w.Code)
		}
	}
}

func TestHandlerOnlyAcceptsPost(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	w := httptest.NewRecorder()
	p.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webhook/stripe", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("code %d", w.Code)
	}
}

// TestHandlerRetriesWhenItCannotClaim: if it cannot be known whether the event was already
// handled, a 500 and a Stripe redelivery are better than a 200 that loses it.
func TestHandlerRetriesWhenItCannotClaim(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	p.h.Seen = func(ctx context.Context, id, typ string) (bool, error) {
		return false, errors.New("the database is not answering")
	}
	if code := p.post(t, eventBody("evt_8", "checkout.session.completed", "cs_1", "shop"), true); code != 500 {
		t.Fatalf("code %d, wanted 500 so that Stripe retries", code)
	}
}

// TestHandlerRunsTheWorkInTheBackgroundWhenAsked: Stripe waits up to ten seconds for the
// webhook's answer before redirecting the buyer, which is why the work comes afterwards.
func TestHandlerRunsTheWorkInTheBackgroundWhenAsked(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	var pending func()
	p.h.Background = func(name string, fn func()) { pending = fn }

	if code := p.post(t, eventBody("evt_9", "checkout.session.completed", "cs_1", "shop"), true); code != 200 {
		t.Fatalf("code %d", code)
	}
	if len(p.handled) != 0 {
		t.Fatal("the work should not have run inside the request")
	}
	if pending == nil {
		t.Fatal("no background work was scheduled")
	}
	pending()
	if len(p.handled) != 1 {
		t.Errorf("the background work did not run: %v", p.handled)
	}
}

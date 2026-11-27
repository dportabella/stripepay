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

func TestWorkThatFailsIsDoneWhenStripeSendsTheEventAgain(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	fails := 1
	p.handleIn = func() error {
		if fails > 0 {
			fails--
			return errors.New("the database is not answering")
		}
		return nil
	}
	body := eventBody("evt_1", "checkout.session.completed", "cs_1", "shop")
	if code := p.post(t, body, true); code != 500 {
		t.Errorf("failed work was answered %d: Stripe would not send it again", code)
	}
	if code := p.post(t, body, true); code != 200 || len(p.handled) != 2 {
		t.Fatalf("the redelivery: code %d, the work ran %d times", code, len(p.handled))
	}
}

func TestAnEventIsNotLostWhenItsStorageDoesNotAnswer(t *testing.T) {
	body := eventBody("evt_2", "checkout.session.completed", "cs_1", "shop")
	down := errors.New("the database is not answering")

	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	asks := p.h.Handled
	p.h.Handled = func(context.Context, string, string) (bool, error) { return false, down }
	if code := p.post(t, body, true); code != 500 || len(p.handled) != 0 || len(p.errs) != 1 {
		t.Fatalf("cannot ask: %d %v %v", code, p.handled, p.errs)
	}
	p.h.Handled = asks
	records := p.h.MarkHandled
	p.h.MarkHandled = func(context.Context, string, string) error { return down }
	if code := p.post(t, body, true); code != 500 || len(p.errs) != 2 {
		t.Fatalf("cannot record: %d %v", code, p.errs)
	}
	p.h.MarkHandled = records
	if code := p.post(t, body, true); code != 200 || len(p.handled) != 2 || !p.recorded["evt_2"] {
		t.Fatalf("the redelivery: %d %v", code, p.handled)
	}
}

// A verdict is the same however many times Stripe sends the event: it is told once and not asked for again.
func TestAVerdictIsNotAskedForAgain(t *testing.T) {
	for reason, told := range map[error]int{ErrNotPaid: 1, ErrAmount: 1, ErrForeign: 0} {
		p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
		p.handleIn = func() error { return fmt.Errorf("order 7: %w", reject(&Session{ID: "cs_1"}, reason, "")) }
		if reason == ErrForeign {
			p.handleIn = func() error { return reason }
		}
		body := eventBody("evt_3", "checkout.session.completed", "cs_1", "shop")
		for range 2 {
			if code := p.post(t, body, true); code != 200 {
				t.Fatalf("%v: %d", reason, code)
			}
		}
		if len(p.handled) != 1 || len(p.errs) != told {
			t.Fatalf("%v: ran %d times, told %v", reason, len(p.handled), p.errs)
		}
	}
}

func TestADeliveryOfAnEventBeingWorkedOnIsAskedForLater(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	body := eventBody("evt_4", "checkout.session.completed", "cs_1", "shop")
	var second int
	p.handleIn = func() error {
		if len(p.handled) == 1 {
			second = p.post(t, body, true)
		}
		return nil
	}
	if code := p.post(t, body, true); code != 200 || second != 409 || len(p.handled) != 1 {
		t.Fatalf("%d, the second %d, ran %d times", code, second, len(p.handled))
	}
}

// Stripe stops waiting for a slow answer; the work goes on, within its own limit.
func TestTheWorkGoesOnWhenStripeStopsWaiting(t *testing.T) {
	p := newHandlerProbe(t, "shop", paidSessionJSON, "checkout.session.completed")
	p.h.WorkTimeout = time.Second
	var after error
	gone, leave := context.WithCancel(context.Background())
	leave()
	p.h.On["checkout.session.completed"] = func(ctx context.Context, _ *Event, _ *Session) error {
		after = ctx.Err()
		return nil
	}
	if code := p.postFrom(t, gone, eventBody("evt_5", "checkout.session.completed", "cs_1", "shop")); code != 200 || after != nil || !p.recorded["evt_5"] {
		t.Fatalf("%d %v", code, after)
	}
}

func (p *handlerProbe) postFrom(t *testing.T, ctx context.Context, body string) int {
	t.Helper()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/webhook/stripe", strings.NewReader(body))
	r.Header.Set("Stripe-Signature", Sign(whsec, time.Now(), []byte(body)))
	w := httptest.NewRecorder()
	p.h.ServeHTTP(w, r)
	return w.Code
}

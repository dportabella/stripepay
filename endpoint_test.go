package stripepay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeAccount stands in for Stripe with an in-memory list of webhook endpoints, so that the
// whole EnsureEndpoint dance can be exercised: find, delete, create, correct.
type fakeAccount struct {
	srv     *httptest.Server
	list    []Endpoint
	deleted []string
}

type fakeRequest struct {
	method string
	path   string
	form   url.Values
}

func newFakeAccount(t *testing.T, initial ...Endpoint) (*fakeAccount, []fakeRequest) {
	t.Helper()
	a := &fakeAccount{list: initial}
	var reqs []fakeRequest
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		reqs = append(reqs, fakeRequest{r.Method, r.URL.Path, r.Form})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/webhook_endpoints":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": a.list})
		case r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/v1/webhook_endpoints/")
			a.deleted = append(a.deleted, id)
			kept := a.list[:0]
			for _, e := range a.list {
				if e.ID != id {
					kept = append(kept, e)
				}
			}
			a.list = kept
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "deleted": true})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/webhook_endpoints":
			ep := Endpoint{
				ID:            "we_new",
				URL:           r.Form.Get("url"),
				Status:        "enabled",
				Description:   r.Form.Get("description"),
				EnabledEvents: r.Form["enabled_events[]"],
				Secret:        "whsec_just_created",
			}
			a.list = append(a.list, ep)
			_ = json.NewEncoder(w).Encode(ep)
		case r.Method == http.MethodPost:
			id := strings.TrimPrefix(r.URL.Path, "/v1/webhook_endpoints/")
			for i := range a.list {
				if a.list[i].ID == id {
					a.list[i].EnabledEvents = r.Form["enabled_events[]"]
					_ = json.NewEncoder(w).Encode(a.list[i])
					return
				}
			}
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "no such endpoint"}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a, reqs
}

func (a *fakeAccount) client(t *testing.T, key string) *Client {
	t.Helper()
	c, err := New(Options{Secret: key, Service: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURL(a.srv.URL)
	return c
}

var testEvents = []string{"checkout.session.completed", "checkout.session.expired"}

func TestEnsureEndpointCreatesItAndGivesTheSecretOnce(t *testing.T) {
	account, _ := newFakeAccount(t)
	c := account.client(t, "sk_live_abc")

	res, err := c.EnsureEndpoint(context.Background(), EnsureRequest{
		URL: "https://example.test/api/webhook", Description: "Shop",
		Events: testEvents,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionCreated {
		t.Errorf("action = %q", res.Action)
	}
	if res.Secret != "whsec_just_created" {
		t.Errorf("the secret is only available when it is created, and it is not there: %q", res.Secret)
	}
	if res.Mode != ModeLive {
		t.Errorf("mode = %q: it has to come from the key's prefix", res.Mode)
	}
	if !res.Endpoint.HasEvents(testEvents) {
		t.Errorf("events = %v", res.Endpoint.EnabledEvents)
	}
	// No API version pinned: that is what lets us read whichever JSON we ask for.
	if res.Endpoint.APIVersion != "" {
		t.Errorf("api_version = %q, and none should be pinned", res.Endpoint.APIVersion)
	}
}

// TestEnsureEndpointDoesNotDeleteWhatItWasNotAskedTo: if one is already there and we do not
// hold its secret, the library does NOT delete it on its own. Deleting a webhook endpoint on
// somebody's account is a decision for whoever owns it.
func TestEnsureEndpointDoesNotDeleteWhatItWasNotAskedTo(t *testing.T) {
	target := "https://example.test/api/webhook"
	account, _ := newFakeAccount(t, Endpoint{ID: "we_old", URL: target, EnabledEvents: testEvents})
	c := account.client(t, "sk_live_abc")

	_, err := c.EnsureEndpoint(context.Background(), EnsureRequest{URL: target, Events: testEvents})
	if err == nil {
		t.Fatal("it should fail: we do not hold its secret")
	}
	if !strings.Contains(err.Error(), "Recreate") {
		t.Errorf("the error has to say the way out: %v", err)
	}
	if len(account.deleted) != 0 {
		t.Errorf("it should not have deleted anything: %v", account.deleted)
	}
}

func TestEnsureEndpointDoesNothingWhenItIsAlreadyRight(t *testing.T) {
	target := "https://example.test/api/webhook"
	account, _ := newFakeAccount(t, Endpoint{ID: "we_good", URL: target, EnabledEvents: testEvents})
	c := account.client(t, "sk_live_abc")

	res, err := c.EnsureEndpoint(context.Background(), EnsureRequest{
		URL: target, Events: testEvents, HaveSecret: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionNone {
		t.Errorf("action = %q, wanted %q", res.Action, ActionNone)
	}
	if res.Secret != "" {
		t.Error("nothing was created: there can be no new secret")
	}
}

// TestEnsureEndpointFixesTheEventList: an endpoint subscribed to things nobody reads
// clutters the delivery log, and one not subscribed to what is needed means it never
// arrives. It is corrected without touching the secret.
func TestEnsureEndpointFixesTheEventList(t *testing.T) {
	target := "https://example.test/api/webhook"
	account, _ := newFakeAccount(t, Endpoint{ID: "we_short", URL: target,
		EnabledEvents: []string{"checkout.session.completed"}})
	c := account.client(t, "sk_live_abc")

	res, err := c.EnsureEndpoint(context.Background(), EnsureRequest{
		URL: target, Events: testEvents, HaveSecret: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionEventsUpdated {
		t.Fatalf("action = %q", res.Action)
	}
	if !res.Endpoint.HasEvents(testEvents) {
		t.Errorf("events = %v", res.Endpoint.EnabledEvents)
	}
	if len(account.deleted) != 0 {
		t.Errorf("correcting the list does not mean deleting it: %v", account.deleted)
	}
}

func TestEnsureEndpointRecreatesWhenAsked(t *testing.T) {
	target := "https://example.test/api/webhook"
	account, _ := newFakeAccount(t, Endpoint{ID: "we_lost", URL: target, EnabledEvents: testEvents})
	c := account.client(t, "sk_live_abc")

	res, err := c.EnsureEndpoint(context.Background(), EnsureRequest{
		URL: target, Events: testEvents, Recreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionRecreated {
		t.Errorf("action = %q", res.Action)
	}
	if res.Secret == "" {
		t.Error("recreating it is the only way to recover the secret, and it is not there")
	}
	if len(account.deleted) != 1 || account.deleted[0] != "we_lost" {
		t.Errorf("it should have deleted the old one: %v", account.deleted)
	}
}

// TestStaleEndpointsFindsTheOneThatWasLeftBehind: the failure that actually happens. An
// endpoint from before, pointing at the same server but at another path, receiving events
// nobody can verify.
func TestStaleEndpointsFindsTheOneThatWasLeftBehind(t *testing.T) {
	current := "https://example.test/api/webhook"
	account, _ := newFakeAccount(t,
		Endpoint{ID: "we_current", URL: current},
		Endpoint{ID: "we_old", URL: "https://example.test/stripe/webhook"},
		Endpoint{ID: "we_other_project", URL: "https://other.test/api/webhook"},
	)
	c := account.client(t, "sk_live_abc")

	stale, err := c.StaleEndpoints(context.Background(), "example.test", current)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != "we_old" {
		t.Fatalf("it should have found only the old one on our own host: %+v", stale)
	}
}

func TestEndpointCreationNeedsEventsAndURL(t *testing.T) {
	account, _ := newFakeAccount(t)
	c := account.client(t, "sk_live_abc")
	ctx := context.Background()

	if _, err := c.CreateEndpoint(ctx, "", "x", testEvents); err == nil {
		t.Error("without a URL it should fail")
	}
	if _, err := c.CreateEndpoint(ctx, "https://x.test/w", "x", nil); err == nil {
		t.Error("without events it should fail")
	}
	if _, err := c.UpdateEndpointEvents(ctx, "we_1", nil); err == nil {
		t.Error("without events it should fail")
	}
}

func TestEnsureEndpointNeedsAKeyThatSaysItsMode(t *testing.T) {
	account, _ := newFakeAccount(t)
	ctx := context.Background()

	without, _ := New(Options{Service: "shop"})
	if _, err := without.EnsureEndpoint(ctx, EnsureRequest{URL: "https://x.test/w", Events: testEvents}); err == nil {
		t.Error("without a key it should fail")
	}
	odd := account.client(t, "does_not_look_like_a_key")
	if _, err := odd.EnsureEndpoint(ctx, EnsureRequest{URL: "https://x.test/w", Events: testEvents}); err == nil {
		t.Error("with a key that does not say its mode it should fail")
	}
}

func TestHasEvents(t *testing.T) {
	e := &Endpoint{EnabledEvents: []string{"b", "a"}}
	if !e.HasEvents([]string{"a", "b"}) {
		t.Error("the order must not matter")
	}
	if e.HasEvents([]string{"a"}) || e.HasEvents([]string{"a", "b", "c"}) {
		t.Error("no more and no fewer")
	}
}

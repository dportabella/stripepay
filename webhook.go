package stripepay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Event is a webhook event whose signature has already been checked.
type Event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`

	raw []byte
}

// Raw is the body exactly as it arrived, which is what the signature was checked over.
func (e *Event) Raw() []byte { return e.raw }

// Session reads the event's object as a Checkout Session. It only makes sense for
// `checkout.session.*` events.
func (e *Event) Session() (*Session, error) {
	var s Session
	if err := json.Unmarshal(e.Data.Object, &s); err != nil {
		return nil, fmt.Errorf("stripepay: the object of %s is not a session: %w", e.Type, err)
	}
	return &s, nil
}

// ObjectID is the id of the event's object, whatever its type.
func (e *Event) ObjectID() string {
	var o struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(e.Data.Object, &o)
	return o.ID
}

// DefaultTolerance is the window accepted between the signature's timestamp and now. It
// stops an old event from being replayed verbatim.
const DefaultTolerance = 5 * time.Minute

// VerifyWebhook checks the signature the way Stripe documents it: the signed payload is the
// timestamp, a dot and the RAW body; HMAC-SHA256 keyed with the endpoint's signing secret;
// constant-time comparison; and a timestamp tolerance.
//
// The body must be the real one, byte for byte: nothing may have parsed or rewritten it
// first, or the signature will never match.
func (c *Client) VerifyWebhook(payload []byte, sigHeader string, now time.Time, tolerance time.Duration) (*Event, error) {
	if c.webhookSecret == "" {
		return nil, fmt.Errorf("stripepay: the webhook signing secret is missing")
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(sigHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			sigs = append(sigs, kv[1])
		}
	}
	if ts == "" || len(sigs) == 0 {
		return nil, fmt.Errorf("stripepay: malformed Stripe-Signature header")
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("stripepay: invalid timestamp")
	}
	if d := now.Sub(time.Unix(secs, 0)); d > tolerance || d < -tolerance {
		return nil, fmt.Errorf("stripepay: timestamp outside the tolerance (%s)", d)
	}

	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	ok := false
	for _, s := range sigs {
		if hmac.Equal([]byte(expected), []byte(s)) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("stripepay: invalid signature")
	}

	var e Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil, fmt.Errorf("stripepay: the event is not valid JSON: %w", err)
	}
	e.raw = payload
	return &e, nil
}

// Sign signs a body the way Stripe would. It is meant for the tests of the projects that
// use this library: building the header by hand in every project is how you end up testing
// signatures that are not the real ones.
func Sign(webhookSecret string, ts time.Time, payload []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(t))
	mac.Write([]byte("."))
	mac.Write(payload)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Handler is the whole path of a Stripe webhook, which is where the mistakes that cost
// money hide. In order, it:
//
//  1. reads the body WITHOUT parsing it and checks its signature;
//  2. claims the event exactly once, through [Handler.Seen], so that a Stripe redelivery
//     does not do the work twice;
//  3. silently drops what belongs to another project on the same account;
//  4. answers 200 straight away, because Stripe waits up to ten seconds for that answer
//     before redirecting the buyer;
//  5. and only then **re-reads the session from the API** and calls the project.
//
// What it does NOT do, because it cannot know: find the row this session pays for and say
// what was meant to be charged. That is [Client.Check] with an [Expectation], and the
// project builds it inside its own handler.
//
// A warning about step 4: once 200 has gone out, Stripe will not redeliver the event. If
// the work afterwards fails, [Handler.OnError] is called and somebody has to look at it;
// the library does not retry on its own. That is the price of answering quickly, which is
// what Stripe asks for.
type Handler struct {
	// Client is required.
	Client *Client

	// Seen claims an event: it must return true if the event had already been seen, and
	// false the first time, recording it. It is required: without idempotency, a Stripe
	// redelivery delivers twice.
	Seen func(ctx context.Context, eventID, eventType string) (bool, error)

	// On are the events this project knows how to handle, by type. A type that is not here
	// is answered 200 and ignored. The session that reaches the handler is the one
	// RE-READ from the API, not the one in the event payload; for an event that carries no
	// session it is nil.
	On map[string]func(ctx context.Context, ev *Event, s *Session) error

	// OnError is called when one of YOUR OWN events could not be handled. [ErrForeign]
	// never reaches it: a neighbour's sale is not an incident.
	OnError func(ctx context.Context, ev *Event, err error)

	// Now, when given, replaces [time.Now] (for tests).
	Now func() time.Time

	// Tolerance, when given, replaces [DefaultTolerance].
	Tolerance time.Duration

	// Background, when given, runs the work left after answering. When nil, the work is
	// done inside the request, which is what tests want.
	Background func(name string, fn func())

	// WorkTimeout is the limit for the work done after answering. 30 s by default.
	WorkTimeout time.Duration
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) tolerance() time.Duration {
	if h.Tolerance > 0 {
		return h.Tolerance
	}
	return DefaultTolerance
}

func (h *Handler) fail(ctx context.Context, ev *Event, err error) {
	if h.OnError != nil {
		h.OnError(ctx, ev, err)
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Client == nil || h.Seen == nil {
		http.Error(w, "webhook misconfigured", http.StatusInternalServerError)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The raw body, with nothing touching it first: the signature is over these bytes.
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	ev, err := h.Client.VerifyWebhook(payload, r.Header.Get("Stripe-Signature"), h.now(), h.tolerance())
	if err != nil {
		h.fail(r.Context(), nil, err)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	seen, err := h.Seen(r.Context(), ev.ID, ev.Type)
	if err != nil {
		// Here a 500 is the better answer: Stripe will send it again and nothing is lost.
		h.fail(r.Context(), ev, fmt.Errorf("claiming the event: %w", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if seen {
		w.WriteHeader(http.StatusOK)
		return
	}

	handle := h.On[ev.Type]
	var sess *Session
	if strings.HasPrefix(ev.Type, "checkout.session.") {
		s, err := ev.Session()
		if err != nil {
			h.fail(r.Context(), ev, err)
			w.WriteHeader(http.StatusOK)
			return
		}
		// A sale from another project on the same account: complete silence.
		if !h.Client.IsOurs(s) {
			w.WriteHeader(http.StatusOK)
			return
		}
		sess = s
	}
	if handle == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)

	work := func() {
		timeout := h.WorkTimeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Re-read from the API: the event payload is a snapshot of the moment it happened,
		// and the state that decides whether to deliver is the state now.
		fresh := sess
		if sess != nil && h.Client.Enabled() {
			got, err := h.Client.Session(ctx, sess.ID)
			if err != nil {
				h.fail(ctx, ev, fmt.Errorf("re-reading session %s: %w", sess.ID, err))
				return
			}
			fresh = got
		}
		if err := handle(ctx, ev, fresh); err != nil {
			h.fail(ctx, ev, err)
		}
	}

	if h.Background != nil {
		h.Background("webhook "+ev.Type, work)
		return
	}
	work()
}

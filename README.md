# stripepay

A small Go library for Stripe Checkout: open a payment, register the webhook, and — above
all — **decide whether a payment really is yours**.

```bash
go get github.com/dportabella/stripepay
```

## Why it exists

A Stripe account can be shared by several projects, and **Stripe delivers every event to
every endpoint on the account**, not just to the one belonging to the project that made the
sale. That means three things it is easy to miss:

1. A session being paid **does not mean it is yours**.
2. `checkout.session.completed` having arrived **does not mean the money is there**: with a
   deferred payment method, Stripe closes the session first.
3. The reference matching **does not mean the amount matches**: the reference is fixed when
   the session is created, and the amount is decided by the session.

And the other way round: the neighbouring project's payment will reach your handler every
day, and **it is not an incident**. Treating it as an error means a false alarm on every one
of their sales.

[`Client.Check`](check.go) is the answer to all four, and it is the reason this package
exists. The rest is the minimum API wrapper needed to get there.

## How it is used

**Opening a payment.** The reference is required and has to be unguessable: it is the
identifier of your own row (the order, the booking), and the whole verification hangs off it.

```go
c, err := stripepay.New(stripepay.Options{
    Secret:        keys.Secret,        // sk_… or rk_…
    WebhookSecret: keys.WebhookSecret, // whsec_…
    Service:       "shop",             // who you are inside the account
})

s, err := c.CreateSession(ctx, stripepay.SessionParams{
    Reference:   order.ID,
    Name:        "The guide, PDF",
    AmountCents: 3900,
    Currency:    "EUR",
    TaxBehavior: stripepay.TaxInclusive,
    Locale:      "fr",
    SuccessURL:  base + "/stripe_callback?session_id=" + stripepay.SessionIDPlaceholder,
    CancelURL:   base + "/guides/",
    Metadata:    map[string]string{"product": order.Product},
})
// s.URL is where the buyer is sent.
```

**Receiving the webhook.** [`Handler`](webhook.go) does the whole path: it reads the body
without parsing it, checks its signature, claims the event exactly once, silently drops what
belongs to the neighbour, answers 200 straight away — Stripe waits up to ten seconds for
that answer before redirecting the buyer — and **re-reads the session from the API** before
calling you.

```go
h := &stripepay.Handler{
    Client: c,
    Seen:   store.SeenWebhook, // idempotency: your own table
    On: map[string]func(context.Context, *stripepay.Event, *stripepay.Session) error{
        "checkout.session.completed": func(ctx context.Context, ev *stripepay.Event, s *stripepay.Session) error {
            row, err := store.ByReference(ctx, s.ClientReferenceID)
            if err != nil {
                return err
            }
            if err := c.Check(s, stripepay.Expectation{
                Reference:   row.ID,
                Currency:    row.Currency,
                MinNetCents: row.AmountCents,
            }); err != nil {
                return err // see "The verdicts"
            }
            return deliver(ctx, row, s)
        },
    },
    OnError: alertTheAdmin,
}
mux.Handle("POST /webhook/stripe", h)
```

**Registering the webhook endpoint**, from a command of your own rather than by hand on the
Stripe dashboard:

```go
res, err := c.EnsureEndpoint(ctx, stripepay.EnsureRequest{
    URL:         base + "/webhook/stripe",
    Description: "Shop",
    Events:      []string{"checkout.session.completed", "checkout.session.expired"},
    HaveSecret:  keys.WebhookSecret != "",
})
// res.Mode says which mode was acted on: always show it.
// res.Secret is only filled in when the endpoint has just been created, and Stripe never
// shows it again.
```

It leaves the account with a single endpoint pointing at your URL, subscribed to exactly the
events you ask for, with **no API version pinned**, and it corrects the event list if it has
drifted. If an endpoint is already there and you do not hold its secret it refuses to touch
it, because deleting a webhook endpoint is a decision for whoever owns the account.
[`StaleEndpoints`](endpoint.go) finds the ones left behind pointing at your own server, and
[`SetEndpointEnabled`](endpoint.go) turns one off without deleting it. Those two are for the
failure that actually happens when a service moves between modes: endpoints are per mode, the
one you left does not go away on its own, and on a shared account it keeps receiving the other
projects' events of that mode and rejecting every one of them, because your server no longer
knows its signing secret. Disabling rather than deleting keeps the secret alive, so moving
back is one call and not a new secret.

## The verdicts

`Check` returns `nil` when you may deliver. Otherwise it returns a `*CheckError` wrapping a
reason, and **the difference between the reasons decides what to do**:

| Reason | What it is | What to do |
|---|---|---|
| `ErrForeign` | A sale of another project on the same account. | **Nothing, and alert nobody.** Alerting would be a false alarm on every one of their sales. |
| `ErrNotPaid` | The session is open: it is being paid. | Nothing. It will arrive. |
| `ErrExpired` | It expired without being paid. | Nothing. |
| `ErrDeferred` | Stripe closed the session without the money being there. | Do not deliver, and look into it: a deferred payment method has been enabled on the dashboard. |
| `ErrNotPayment`, `ErrReference`, `ErrCurrency`, `ErrAmount`, `ErrNoPaymentRequired` | One of **your own** sessions that does not add up. | Do not deliver, and **alert**: either it is your bug, or somebody is playing with you. |

```go
var bad *stripepay.CheckError
switch {
case errors.Is(err, stripepay.ErrForeign):
    return nil // silence
case errors.As(err, &bad):
    alertTheAdmin(bad.Error())
}
```

`Client.Session` tells one more case apart, because it leads to a different decision:
`ErrNoSuchObject` means the session **does not exist** (an invented identifier, or one from
another account or another mode), and it must not be confused with "Stripe is not
answering". Conflating them is how a return page handed an invented identifier ends up
claiming a payment is being confirmed.

## Deferred payments are never accepted

That is a decision, not a limitation. With SEPA Direct Debit, Boleto, Konbini or Multibanco,
Stripe closes the session and the money arrives days later, or never; whoever believes the
event ships what they sell for free. Here it is blocked **at both ends**:

- `CreateSession` asks only for methods that pay on the spot (by default card — with Apple
  Pay and Google Pay inside it — and Link), and **refuses** to create a session that
  includes any deferred one;
- `Check` returns `ErrDeferred` if one arrives anyway.

The second layer is there because the first depends on the project passing the list, and the
list of methods enabled on an account is changed with one click on the Stripe dashboard,
outside your repository.

## Design decisions, and why

**It does not use `stripe-go`, the official library.** Three reasons, none of them cosmetic:

- `stripe-go` keeps the API key in a **package-level global** (`stripe.Key`). A process that
  charges for more than one account — a multi-tenant service with one key per tenant, or one
  key per mode — cannot use that without contortions. Here the `Client` is explicit and
  there is no global state.
- `stripe-go` **rejects every event** the day the API version pinned on the webhook endpoint
  is not exactly the one it expects. That forces you to pin an `api_version` when you
  register the endpoint and to keep it in step with the library. Nothing is pinned here: we
  read the fields we ask for and Stripe's forward compatibility is enough.
- The surface most applications use is five calls. The official library's dependency tree,
  for five calls, does not pay for itself.

**Stripe Tax is off by default.** `automatic_tax` is what switches it on, and it costs 0.5 %
of every sale. If the price is advertised tax-inclusive there is nothing to compute at
payment time, and the breakdown and the invoice have to be produced elsewhere anyway. The
combination that **does** fail is `TaxExclusive` without `AutomaticTax`: that would mean the
price is the net amount and nobody adds the tax, which is quietly undercharging.

**The webhook delivers, not the return page.** Stripe's documentation is explicit: "You
can't rely on triggering fulfillment only from your Checkout landing page, because your
customers aren't guaranteed to visit that page." The return page may run the same idempotent
verification so the buyer does not have to wait, but the event is the authority.

**Once 200 has gone out, Stripe does not redeliver.** That is the price of answering quickly,
and it is stated on [`Handler`](webhook.go): if the work afterwards fails, `OnError` is
called and somebody has to look at it. The one case that deliberately answers 500 is when it
cannot be known whether the event was already handled, because then a redelivery is better
than losing it.

## The tests are the contract

[`testdata/sessions.json`](testdata/sessions.json) is a list of Stripe sessions with the
verdict each one must produce, and `TestEveryVerdictHasAVector` will not let a verdict be
added without a vector exercising it. If a twin of this library is ever written in another
language, it has to produce **exactly** these verdicts for **exactly** these bodies; it is
the only way two implementations do not drift apart in silence.

[`Sign`](webhook.go) signs a body the way Stripe would, so that the signature header does
not have to be built by hand in every project: that is how you end up testing signatures
that are not the real ones.

```bash
go test ./...
go test -cover ./...
```

## What it does NOT do, and will not

VAT arithmetic and legal invoices, your orders or bookings table, generating the unguessable
reference, and what gets delivered when a payment is good. All of that is each
application's own business. This library only says **whether the money is there and whether
it is yours**.

## Licence

MIT.

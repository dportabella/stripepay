package stripepay

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// RefundParams is a refund.
type RefundParams struct {
	// PaymentIntent is the charge being refunded (`pi_…`), as the session carries it.
	PaymentIntent string
	// AmountCents at 0 means refund everything: Stripe does that by itself when no amount
	// is sent. A positive amount is a partial refund, and it can be repeated as long as
	// the total charged is not exceeded.
	AmountCents int64
	// Reason is recorded in the refund's metadata, so that it is possible to know later
	// why it was made. It is not Stripe's own `reason` field, which only takes three
	// values of its own.
	Reason string
	// IdempotencyKey stops a retry from refunding twice. Always pass something stable: the
	// row id and the reason, for instance.
	IdempotencyKey string
}

// Refund is what Stripe returns for a refund.
type Refund struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
}

// Refund gives a payment back, in full or in part.
func (c *Client) Refund(ctx context.Context, p RefundParams) (*Refund, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("stripepay: this client has no key")
	}
	if p.PaymentIntent == "" {
		return nil, fmt.Errorf("stripepay: the payment_intent of the charge to refund is required")
	}
	f := url.Values{}
	f.Set("payment_intent", p.PaymentIntent)
	if p.AmountCents > 0 {
		f.Set("amount", strconv.FormatInt(p.AmountCents, 10))
	}
	if p.Reason != "" {
		f.Set("metadata[reason]", p.Reason)
	}
	f.Set("metadata["+MetadataService+"]", c.service)

	var r Refund
	if err := c.post(ctx, "/v1/refunds", f, p.IdempotencyKey, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

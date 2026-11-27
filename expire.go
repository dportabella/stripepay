package stripepay

import (
	"context"
	"fmt"
	"net/url"
)

// ExpireSession closes a payment page so that nobody can pay on it any more, and returns the session as it ended. A session that was no longer open is not an error: its Status says whether
// it expired or was completed, and a completed one may have been paid, so look before opening another page for the same thing. An error means the page may still take a payment.
func (c *Client) ExpireSession(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, fmt.Errorf("stripepay: the session id is required")
	}
	var s Session
	refused := c.post(ctx, "/v1/checkout/sessions/"+url.PathEscape(id)+"/expire", nil, "", &s)
	if refused == nil {
		return &s, nil
	}
	// Stripe refuses to expire what is not open, with the same kind of answer as other refusals: the session itself says which it was.
	now, err := c.Session(ctx, id)
	if err == nil && (now.Status == "expired" || now.Status == "complete") {
		return now, nil
	}
	return nil, fmt.Errorf("stripepay: the session %s was not expired: %w", id, refused)
}

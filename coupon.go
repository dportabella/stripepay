package stripepay

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// CouponParams is a percentage coupon of a one-off payment, with an id of your own so that
// [SessionParams.CouponID] can name it.
type CouponParams struct {
	ID   string
	Name string // what the buyer reads next to the discount on the payment page
	// PercentOff is a whole percentage, from 1 to 99: a free session would end as
	// `no_payment_required`, which [Client.Check] refuses.
	PercentOff int
}

// EnsureCoupon makes sure the coupon exists in the account as described: once per payment, no
// limit of redemptions and no expiry. A missing one is created. One that exists and says anything
// else is an error and is never modified: somebody changed it, and charging with it would charge a
// different price from the one the application shows.
func (c *Client) EnsureCoupon(ctx context.Context, p CouponParams) error {
	if !c.Enabled() {
		return fmt.Errorf("stripepay: this client has no key")
	}
	if p.ID == "" || p.Name == "" || p.PercentOff < 1 || p.PercentOff > 99 {
		return fmt.Errorf("stripepay: a coupon needs an id, a name and a percentage from 1 to 99: %+v", p)
	}
	var got struct {
		PercentOff     float64 `json:"percent_off"`
		Duration       string  `json:"duration"`
		Valid          bool    `json:"valid"`
		MaxRedemptions int64   `json:"max_redemptions"`
		RedeemBy       int64   `json:"redeem_by"`
	}
	err := c.get(ctx, "/v1/coupons/"+url.PathEscape(p.ID), &got)
	if errors.Is(err, ErrNoSuchObject) {
		f := url.Values{}
		f.Set("id", p.ID)
		f.Set("name", p.Name)
		f.Set("percent_off", strconv.Itoa(p.PercentOff))
		f.Set("duration", "once")
		f.Set("metadata["+MetadataService+"]", c.service)
		// No idempotency key: the fixed id already makes a repeat harmless, and a key would replay
		// the answer for a coupon deleted in the last 24 hours.
		if err := c.post(ctx, "/v1/coupons", f, "", nil); err != nil {
			return fmt.Errorf("stripepay: creating the coupon %s: %w", p.ID, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("stripepay: reading the coupon %s: %w", p.ID, err)
	}
	if got.PercentOff != float64(p.PercentOff) || got.Duration != "once" || !got.Valid || got.MaxRedemptions != 0 || got.RedeemBy != 0 {
		return fmt.Errorf("stripepay: the coupon %s is not %d%% once, unlimited and valid: %+v", p.ID, p.PercentOff, got)
	}
	return nil
}

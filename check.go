package stripepay

import (
	"errors"
	"fmt"
	"strings"
)

// The reasons a session cannot be taken at face value. They are compared with [errors.Is],
// and the difference between them is not academic: it decides what the caller must do.
//
//   - [ErrForeign] is the only one that is NOT an incident: it is a sale belonging to
//     another project on the same Stripe account. Answer 200, do nothing, and alert nobody.
//     Alerting would mean a false alarm on every single charge the neighbour takes.
//   - [ErrNotPaid] and [ErrExpired] are ordinary states along the way: still being paid, or
//     never paid. Nothing is delivered and there is nothing to investigate.
//   - [ErrDeferred] is a session Stripe closed without the money being there. It is never
//     accepted, and it is worth looking into: it means a deferred payment method has been
//     enabled on the Stripe dashboard.
//   - The rest — [ErrNotPayment], [ErrReference], [ErrCurrency], [ErrAmount],
//     [ErrNoPaymentRequired] — are YOUR OWN sessions that do not add up. Nothing is
//     delivered and somebody has to be told: either it is a bug of yours, or somebody is
//     playing with you.
var (
	ErrForeign           = errors.New("the session belongs to another project on the same account")
	ErrNotPaid           = errors.New("the session is not paid yet")
	ErrExpired           = errors.New("the session expired without being paid")
	ErrDeferred          = errors.New("deferred payment: the session closed without the money being there")
	ErrNotPayment        = errors.New("the session is not a one-off payment")
	ErrReference         = errors.New("the session's reference is not the one expected")
	ErrCurrency          = errors.New("the currency is not the one expected")
	ErrAmount            = errors.New("less money came in than was meant to be charged")
	ErrNoPaymentRequired = errors.New("the session required no payment")
)

// CheckError is what [Client.Check] returns when a session does not pass, with the detail
// needed to log it or put it in an alert. It wraps one of the reasons above.
type CheckError struct {
	SessionID string
	Detail    string
	Reason    error
}

func (e *CheckError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("session %s: %v", e.SessionID, e.Reason)
	}
	return fmt.Sprintf("session %s: %v (%s)", e.SessionID, e.Reason, e.Detail)
}

func (e *CheckError) Unwrap() error { return e.Reason }

func reject(s *Session, reason error, detail string, args ...any) error {
	id := "(no id)"
	if s != nil {
		id = s.ID
	}
	return &CheckError{SessionID: id, Detail: fmt.Sprintf(detail, args...), Reason: reason}
}

// Expectation is what you expected of this charge, read from YOUR OWN row and not from the
// session: it is the only way the comparison means anything.
type Expectation struct {
	// Reference must be the same one passed when the session was created.
	Reference string
	// Currency, in ISO. Empty means do not check it, which is rarely a good idea.
	Currency string
	// MinNetCents is the least that must have come in, net of tax. Normally the price you
	// advertised. Zero means do not check the amount.
	MinNetCents int64
}

// IsOurs reports whether this session was created by this project.
//
// The comparison is strict: a session without the mark is not yours. Accepting unmarked
// ones is the hole through which a purchase from another project on a shared account — or a
// session created by hand from the Stripe dashboard — passes for a sale of yours.
func (c *Client) IsOurs(s *Session) bool {
	return s != nil && s.Metadata[MetadataService] == c.service
}

// Check reports whether this session is yours, really paid, and carries what you expected.
//
// It returns nil when you may deliver. Anything else is a [CheckError] wrapping one of the
// reasons in this file; look at the difference between them before treating them all alike,
// because [ErrForeign] is not a problem and the last few are.
//
// That `payment_status` is "paid" cannot be taken for granted:
// `checkout.session.completed` fires when the session completes, and with a deferred
// payment method that happens BEFORE the money arrives. Neither can the amount: the
// reference is fixed when the session is created, but the amount is decided by the session,
// and a promotion code, an adjustable quantity or a price changed afterwards all move it.
// It is compared against what your own row says you meant to charge, not against a constant
// in the code.
func (c *Client) Check(s *Session, e Expectation) error {
	if s == nil {
		return reject(nil, ErrForeign, "there is no session")
	}
	if !c.IsOurs(s) {
		return reject(s, ErrForeign, "metadata[%s] = %q, and we are %q",
			MetadataService, s.Metadata[MetadataService], c.service)
	}
	if s.Mode != "payment" {
		return reject(s, ErrNotPayment, "mode = %q", s.Mode)
	}
	if e.Reference == "" {
		return reject(s, ErrReference, "it was not told which reference to expect")
	}
	if s.ClientReferenceID != e.Reference {
		return reject(s, ErrReference, "client_reference_id = %q, expected %q",
			s.ClientReferenceID, e.Reference)
	}

	switch s.PaymentStatus {
	case "paid":
		// Go ahead.
	case "no_payment_required":
		// A legitimate state in other contexts, but here it would mean something was
		// given away, which is never what you want.
		return reject(s, ErrNoPaymentRequired, "status = %q", s.Status)
	case "unpaid":
		switch s.Status {
		case "expired":
			return reject(s, ErrExpired, "")
		case "complete":
			return reject(s, ErrDeferred, "Stripe closed the session with payment_status "+
				"\"unpaid\": a deferred payment method is enabled on the account")
		default:
			return reject(s, ErrNotPaid, "status = %q", s.Status)
		}
	default:
		return reject(s, ErrNotPaid, "payment_status = %q, status = %q", s.PaymentStatus, s.Status)
	}

	if e.Currency != "" && !strings.EqualFold(s.Currency, e.Currency) {
		return reject(s, ErrCurrency, "%q, expected %q", s.Currency, e.Currency)
	}
	if e.MinNetCents > 0 {
		if net := s.NetCents(); net < e.MinNetCents {
			return reject(s, ErrAmount, "%d net cents came in and %d were meant to be charged",
				net, e.MinNetCents)
		}
	}
	return nil
}

package stripepay

import (
	"fmt"
	"strings"
)

// Mode is a Stripe account's mode: test or live. They are two separate worlds, with
// different keys, different webhook endpoints and different data.
type Mode string

const (
	ModeTest Mode = "test"
	ModeLive Mode = "live"
)

// Keys are the two secrets of one mode: the API key and the webhook signing secret.
type Keys struct {
	Secret        string
	WebhookSecret string
}

// ModeOf reads the mode from the key's prefix, which is where Stripe puts it
// (`sk_test_…`, `rk_live_…`).
func ModeOf(secret string) (Mode, error) {
	switch {
	case strings.Contains(secret, "_test_"):
		return ModeTest, nil
	case strings.Contains(secret, "_live_"):
		return ModeLive, nil
	default:
		return "", fmt.Errorf("stripepay: that does not look like a Stripe key (no _test_ or _live_ in the prefix)")
	}
}

// PickKeys picks the keys of the declared mode and checks that they really are of that mode.
//
// This is the check that prevents the two expensive, symmetrical mistakes: believing you
// are charging when you are not, and charging for real while believing you are testing. It
// looks at the prefix, which is what Stripe puts there, and refuses to start if it does not
// match.
//
// A mode with no key is legitimate: it means this client does not charge. Having the key of
// the OTHER mode and not the declared one is not: it means someone changed the mode and
// forgot the key, and the service would quietly stop charging.
func PickKeys(declared Mode, test, live Keys) (Keys, error) {
	var keys, other Keys
	switch declared {
	case ModeTest:
		keys, other = test, live
	case ModeLive:
		keys, other = live, test
	default:
		return Keys{}, fmt.Errorf("stripepay: unknown mode %q (%s | %s)", declared, ModeTest, ModeLive)
	}
	if keys.Secret == "" {
		if other.Secret != "" {
			return Keys{}, fmt.Errorf("stripepay: the declared mode is %q and its key is missing, "+
				"but the other mode's key is there: put in the right key or change the mode back", declared)
		}
		return keys, nil
	}
	if !strings.Contains(keys.Secret, "_"+string(declared)+"_") {
		return Keys{}, fmt.Errorf("stripepay: the key for mode %q is not of that mode "+
			"(Stripe marks them with _test_ or _live_ in the prefix)", declared)
	}
	return keys, nil
}

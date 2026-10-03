package stripepay

import (
	"context"
	"strings"
	"testing"
)

// TestLocaleNeverReturnsSomethingStripeRefuses: a locale Stripe does not know is a 400 that
// only shows up in production, and only for the language nobody tested with.
func TestLocaleNeverReturnsSomethingStripeRefuses(t *testing.T) {
	for _, lang := range []string{
		"en", "es", "fr", "de", "pt-BR", "zh-TW", // exact matches
		"pt-PT", "de-AT", "es-ES", "en-US", // base language
		"ca", "eu", "gl", "cy", "qqq", "", // nothing like it
	} {
		got := Locale(lang)
		if !LocaleSupported(got) {
			t.Errorf("Locale(%q) = %q, which Stripe would refuse", lang, got)
		}
	}
	for lang, want := range map[string]string{
		"en": "en", "pt-BR": "pt-BR", "pt-PT": "pt", "es-ES": "es",
		"ca": "auto", "": "auto",
	} {
		if got := Locale(lang); got != want {
			t.Errorf("Locale(%q) = %q, wanted %q", lang, got, want)
		}
	}
}

// TestCreateSessionRefusesALocaleStripeDoesNotKnow: the check that turns a production 400
// into a failure at the moment the mistake is made.
func TestCreateSessionRefusesALocaleStripeDoesNotKnow(t *testing.T) {
	f := newFakeStripe(t, `{"id":"cs_test_1"}`)
	c := f.client(t, "shop")

	p := goodParams()
	p.Locale = "ca"
	_, err := c.CreateSession(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "locale") {
		t.Fatalf("an unsupported locale has to fail here, not at Stripe: %v", err)
	}
	if f.path != "" {
		t.Error("it should not even have been sent to Stripe")
	}

	p.Locale = Locale("ca")
	if _, err := c.CreateSession(context.Background(), p); err != nil {
		t.Fatalf("after Locale() it has to go through: %v", err)
	}
}

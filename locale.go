package stripepay

import "strings"

// supportedLocales are the languages the Checkout page can be shown in. Stripe rejects
// anything else with a 400, which is a mistake that only shows up in production and only
// for the language nobody tested with.
//
// The list is Stripe's own, quoted back by the API when it refuses one:
// https://docs.stripe.com/api/checkout/sessions/create#create_checkout_session-locale
var supportedLocales = map[string]bool{
	"auto": true, "bg": true, "cs": true, "da": true, "de": true, "el": true,
	"en": true, "en-GB": true, "es": true, "es-419": true, "et": true, "fi": true,
	"fil": true, "fr": true, "fr-CA": true, "hr": true, "hu": true, "id": true,
	"it": true, "ja": true, "ko": true, "lt": true, "lv": true, "ms": true,
	"mt": true, "nb": true, "nl": true, "pl": true, "pt": true, "pt-BR": true,
	"ro": true, "ru": true, "sk": true, "sl": true, "sv": true, "th": true,
	"tr": true, "vi": true, "zh": true, "zh-HK": true, "zh-TW": true,
}

// LocaleSupported reports whether Stripe accepts this locale for a Checkout Session.
func LocaleSupported(locale string) bool { return supportedLocales[locale] }

// Locale turns a language tag into a locale Stripe accepts.
//
// An exact match is used as it is; otherwise the base language is tried, so "pt-PT" becomes
// "pt" and "de-AT" becomes "de"; and a language Stripe does not have at all becomes "auto",
// which lets the browser decide.
//
// "auto" is a floor, not an answer. A language Stripe does not support but whose speakers
// read another one perfectly well — Catalan and Spanish, say — deserves an explicit mapping
// in the application, which knows its own audience. This function only guarantees that
// whatever comes out will not be refused.
func Locale(lang string) string {
	if supportedLocales[lang] {
		return lang
	}
	if base, _, found := strings.Cut(lang, "-"); found && supportedLocales[base] {
		return base
	}
	return "auto"
}

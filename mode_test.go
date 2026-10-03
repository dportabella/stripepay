package stripepay

import (
	"strings"
	"testing"
)

func TestModeOf(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want Mode
		fail bool
	}{
		{"sk_test_51Abc", ModeTest, false},
		{"rk_test_51Abc", ModeTest, false},
		{"sk_live_51Abc", ModeLive, false},
		{"rk_live_51Abc", ModeLive, false},
		{"", "", true},
		{"whsec_abc", "", true},
		{"pk_live_abc", ModeLive, false}, // a publishable key also carries the mode
	} {
		got, err := ModeOf(tc.key)
		if tc.fail {
			if err == nil {
				t.Errorf("%q: should fail, it returned %q", tc.key, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: %q, %v; wanted %q", tc.key, got, err, tc.want)
		}
	}
}

// TestPickKeysRefusesAKeyOfTheOtherMode: the two expensive, symmetrical mistakes are
// believing you are charging when you are not, and charging for real while believing you
// are testing. Both have to fail at startup, not at sale time.
func TestPickKeysRefusesAKeyOfTheOtherMode(t *testing.T) {
	test := Keys{Secret: "rk_test_abc", WebhookSecret: "whsec_test"}
	live := Keys{Secret: "sk_live_abc", WebhookSecret: "whsec_live"}

	for _, tc := range []struct {
		name     string
		declared Mode
		test     Keys
		live     Keys
		want     string // the secret that must come out; empty means "no key"
		wantErr  string
	}{
		{"test", ModeTest, test, live, "rk_test_abc", ""},
		{"live", ModeLive, test, live, "sk_live_abc", ""},
		{"a tenant that does not charge", ModeTest, Keys{}, Keys{}, "", ""},
		{"the other mode's key in the declared slot", ModeLive,
			Keys{Secret: "sk_live_abc"}, Keys{Secret: "rk_test_abc"}, "", "not of that mode"},
		{"mode changed and key forgotten", ModeLive, test, Keys{}, "", "other mode's key is there"},
		{"a mode that does not exist", Mode("sandbox"), test, live, "", "unknown mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PickKeys(tc.declared, tc.test, tc.live)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("should fail, it returned %+v", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("the error should say %q: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("should not fail: %v", err)
			}
			if got.Secret != tc.want {
				t.Errorf("secret = %q, wanted %q", got.Secret, tc.want)
			}
		})
	}
}

func TestClientModeComesFromTheKey(t *testing.T) {
	c, err := New(Options{Secret: "rk_live_x", Service: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := c.Mode(); err != nil || m != ModeLive {
		t.Errorf("Mode = %q, %v", m, err)
	}
	if !c.Enabled() {
		t.Error("with a key, Enabled should be true")
	}
	if c.Service() != "shop" {
		t.Errorf("Service = %q", c.Service())
	}
	without, _ := New(Options{Service: "shop"})
	if without.Enabled() {
		t.Error("without a key, Enabled should be false")
	}
}

// TestServiceIsMandatory: without knowing who you are inside the account there is no way to
// tell your own charge from a neighbour's, and that is precisely what this package exists to
// do. Better not to let the client be built at all.
func TestServiceIsMandatory(t *testing.T) {
	if _, err := New(Options{Secret: "sk_test_x"}); err == nil {
		t.Fatal("without Service it should fail")
	}
	if _, err := New(Options{Secret: "sk_test_x", Service: "   "}); err == nil {
		t.Fatal("a blank Service should fail")
	}
}

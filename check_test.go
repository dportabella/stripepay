package stripepay

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// The verdicts a vector in testdata/sessions.json may ask for, and the real error each one
// stands for. The vectors are this library's contract: if a twin of it is ever written in
// another language, it has to return exactly these verdicts for exactly these bodies.
var verdicts = map[string]error{
	"ok":                  nil,
	"foreign":             ErrForeign,
	"not-paid":            ErrNotPaid,
	"expired":             ErrExpired,
	"deferred":            ErrDeferred,
	"not-payment":         ErrNotPayment,
	"reference":           ErrReference,
	"currency":            ErrCurrency,
	"amount":              ErrAmount,
	"no-payment-required": ErrNoPaymentRequired,
}

type vector struct {
	Name    string  `json:"name"`
	Comment string  `json:"comment"`
	Service string  `json:"service"`
	Session Session `json:"session"`
	Expect  struct {
		Reference   string `json:"reference"`
		Currency    string `json:"currency"`
		MinNetCents int64  `json:"min_net_cents"`
	} `json:"expect"`
	Want string `json:"want"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile("testdata/sessions.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 {
		t.Fatal("testdata/sessions.json is empty")
	}
	return vs
}

func TestCheckAgainstVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			want, known := verdicts[v.Want]
			if !known {
				t.Fatalf("the vector asks for verdict %q, which does not exist", v.Want)
			}
			c, err := New(Options{Secret: "sk_test_x", Service: v.Service})
			if err != nil {
				t.Fatal(err)
			}
			s := v.Session
			got := c.Check(&s, Expectation{
				Reference:   v.Expect.Reference,
				Currency:    v.Expect.Currency,
				MinNetCents: v.Expect.MinNetCents,
			})
			if want == nil {
				if got != nil {
					t.Fatalf("it should have been accepted and it was rejected: %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("it should have been rejected with %v and it was accepted", want)
			}
			if !errors.Is(got, want) {
				t.Fatalf("rejected with %v, expected %v", got, want)
			}
			// The detail has to be usable in an alert: it must say which session it is about.
			var ce *CheckError
			if !errors.As(got, &ce) {
				t.Fatalf("the error should be a *CheckError, it is %T", got)
			}
			if ce.SessionID != v.Session.ID {
				t.Errorf("the error talks about session %q and it should be %q", ce.SessionID, v.Session.ID)
			}
		})
	}
}

// TestEveryVerdictHasAVector: the verdicts are the reason this package exists. If one is
// added and nobody tests it, the contract stops being a contract.
func TestEveryVerdictHasAVector(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range loadVectors(t) {
		seen[v.Want] = true
	}
	for name := range verdicts {
		if !seen[name] {
			t.Errorf("no vector in testdata/sessions.json exercises verdict %q", name)
		}
	}
}

// TestForeignIsNotAnIncident: the distinction that keeps a shared account from filling the
// inbox with alerts. It is tested separately because it is the one that quietly breaks when
// somebody "simplifies" the error handling.
func TestForeignIsNotAnIncident(t *testing.T) {
	c, _ := New(Options{Secret: "sk_test_x", Service: "shop"})
	theirs := &Session{ID: "cs_1", Mode: "payment", Status: "complete", PaymentStatus: "paid",
		Metadata: map[string]string{MetadataService: "booking"}}
	err := c.Check(theirs, Expectation{Reference: "ord_1"})
	if !errors.Is(err, ErrForeign) {
		t.Fatalf("a neighbour's session has to be ErrForeign: %v", err)
	}
	// And it must not look like any of the ones that do have to be investigated.
	for _, serious := range []error{ErrReference, ErrAmount, ErrDeferred, ErrNotPayment} {
		if errors.Is(err, serious) {
			t.Errorf("ErrForeign cannot also be %v", serious)
		}
	}
}

// TestIsOurs: the comparison is strict, and a session without the mark is not ours.
func TestIsOurs(t *testing.T) {
	c, _ := New(Options{Secret: "sk_test_x", Service: "shop"})
	for _, tc := range []struct {
		name string
		s    *Session
		want bool
	}{
		{"ours", &Session{Metadata: map[string]string{MetadataService: "shop"}}, true},
		{"another project's", &Session{Metadata: map[string]string{MetadataService: "booking"}}, false},
		{"no mark", &Session{Metadata: map[string]string{}}, false},
		{"no metadata", &Session{}, false},
		{"no session", nil, false},
	} {
		if got := c.IsOurs(tc.s); got != tc.want {
			t.Errorf("%s: IsOurs = %v, wanted %v", tc.name, got, tc.want)
		}
	}
}

// TestCheckNeedsToKnowWhatToExpect: calling Check without saying which reference was
// expected would check everything except the thing that matters. It is not allowed.
func TestCheckNeedsToKnowWhatToExpect(t *testing.T) {
	c, _ := New(Options{Secret: "sk_test_x", Service: "shop"})
	s := &Session{ID: "cs_1", Mode: "payment", Status: "complete", PaymentStatus: "paid",
		ClientReferenceID: "ord_1", Metadata: map[string]string{MetadataService: "shop"}}
	if err := c.Check(s, Expectation{}); !errors.Is(err, ErrReference) {
		t.Fatalf("without Expectation.Reference it should fail with ErrReference: %v", err)
	}
}

func TestNetCentsAndAccessors(t *testing.T) {
	s := &Session{AmountTotal: 11500}
	s.TotalDetails.AmountTax = 1979
	if got := s.NetCents(); got != 9521 {
		t.Errorf("NetCents = %d, wanted 9521", got)
	}
	if s.Country() != "" || s.Email() != "" || s.Name() != "" || s.FirstTaxID() != "" {
		t.Error("with no customer_details the accessors must return empty and not panic")
	}
	s.CustomerDetails = &CustomerDetails{Email: "a@b.test", Name: "A B",
		Address: &Address{Country: "ES"}, TaxIDs: []TaxID{{Type: "es_cif", Value: "B1"}}}
	if s.Country() != "ES" || s.Email() != "a@b.test" || s.Name() != "A B" || s.FirstTaxID() != "B1" {
		t.Errorf("accessors read wrong: %+v", s.CustomerDetails)
	}
}

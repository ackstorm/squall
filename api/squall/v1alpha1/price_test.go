// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"testing"
)

// TestPrice_AcceptsUnquotedDecimal is D31. `maxPricePerHour: 1.20` in YAML
// reaches the API server as a JSON NUMBER. The old type was
// *resource.Quantity behind x-kubernetes-int-or-string, whose schema is
// anyOf:[integer,string] — a float is neither, so the apply was rejected
// with `must be type integer,string: "number"` before Quantity's own
// unmarshaller ever ran. Both spellings must work now.
func TestPrice_AcceptsUnquotedDecimal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unquoted decimal", `1.20`, "1.20"},
		{"quoted decimal", `"1.20"`, "1.20"},
		{"unquoted integer", `2`, "2"},
		{"quoted integer", `"2"`, "2"},
		{"trailing zeros preserved verbatim", `0.80`, "0.80"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p Price
			if err := json.Unmarshal([]byte(tc.in), &p); err != nil {
				t.Fatalf("Unmarshal(%s) errored: %v", tc.in, err)
			}
			if p.String() != tc.want {
				t.Fatalf("Unmarshal(%s) = %q, want %q", tc.in, p.String(), tc.want)
			}
		})
	}
}

// TestPrice_RoundTripsAsAString: whatever went in, dstack receives a string
// (its max_price is a plain JSON field and squall passes ranges through
// opaquely, F33). Marshalling must not turn 0.80 into 0.8 — the value is
// the user's, not ours to normalise.
func TestPrice_RoundTripsAsAString(t *testing.T) {
	var p Price
	if err := json.Unmarshal([]byte(`0.80`), &p); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"0.80"` {
		t.Fatalf("Marshal = %s, want \"0.80\"", out)
	}
}

// TestPrice_DecodeIsTotal is D70's resolution. Content validation cannot
// happen at admission (see the type doc comment), so it must not happen at
// decode time either — a decode error would be the ONLY signal a bad price
// ever produced, and nobody reads a controller log line. UnmarshalJSON must
// always succeed and must preserve whatever text arrived, garbage or not,
// so Validate has something meaningful to reject later.
func TestPrice_DecodeIsTotal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"nonsense word", `"cheap"`, "cheap"},
		{"malformed decimal", `"1.2.3"`, "1.2.3"},
		{"quantity milli suffix", `"2200m"`, "2200m"},
		{"empty string", `""`, ""},
		{"bool", `true`, "true"},
		{"object", `{}`, "{}"},
		{"array", `[]`, "[]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p Price
			if err := json.Unmarshal([]byte(tc.in), &p); err != nil {
				t.Fatalf("Unmarshal(%s) errored: %v, want no error (decoding a Model must always succeed)", tc.in, err)
			}
			if p.String() != tc.want {
				t.Fatalf("Unmarshal(%s) = %q, want %q", tc.in, p.String(), tc.want)
			}
		})
	}
}

// TestPrice_Validate_RejectsNonsense: a permissive TYPE is not a
// permissive MEANING. A price that is not a plain decimal must not reach
// the provisioner as a silent pass-through — a bad max_price either
// provisions nothing (looks like an empty market, D58/D67) or, worse, is
// misread. "2200m" is here, not accepted: it is a resource.Quantity milli
// suffix Price deliberately rejects (ledger D31 addendum) — there is no
// released Squall to be backward compatible with, and reading it as
// milli-dollars would make a $2.20 ceiling and a $2200 one indistinguishable
// in the CR text.
func TestPrice_Validate_RejectsNonsense(t *testing.T) {
	// "Inf"/"NaN"/negatives/zero are D117: strconv.ParseFloat returns a
	// nil error for all of them, and the string goes VERBATIM to dstack's
	// max_price where pydantic's float coercion accepts Inf too — an
	// unbounded ceiling through the one check documented as the money
	// safety valve. Zero and negatives yield zero offers, which D58
	// records as indistinguishable from an empty market.
	for _, in := range []string{"cheap", "1.2.3", "true", "{}", "[]", "", "2200m",
		"Inf", "+Inf", "-Inf", "inf", "NaN", "nan", "-1", "-0.5", "0"} {
		p := Price(in)
		if err := p.Validate(); err == nil {
			t.Fatalf("Price(%q).Validate() = nil, want an error", in)
		}
	}
}

// TestPrice_Validate_AcceptsDecimal: the flip side of the rejection table —
// Validate must not reject the exact spellings D31/D70 exist to accept.
func TestPrice_Validate_AcceptsDecimal(t *testing.T) {
	for _, in := range []string{"1.20", "2", "0.80", "2.20"} {
		p := Price(in)
		if err := p.Validate(); err != nil {
			t.Fatalf("Price(%q).Validate() = %v, want nil", in, err)
		}
	}
}

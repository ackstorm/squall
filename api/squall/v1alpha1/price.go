// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Price is a per-hour cost ceiling written the way a person writes money.
//
// It exists because the obvious type does not work. `*resource.Quantity`
// behind `x-kubernetes-int-or-string` publishes `anyOf: [integer, string]`,
// and a bare YAML `1.20` arrives as a JSON *number* — neither of those — so
// the API server rejects the object with `must be type integer,string:
// "number"` before Quantity's own permissive unmarshaller is ever reached
// (ledger D31). Quoting it works, but "quote your prices" is a trap laid for
// every user, and it is laid at the exact moment they are trying their first
// model.
//
// So: accept both spellings, keep the ORIGINAL TEXT, and hand dstack a
// string. The text is preserved deliberately — normalising 0.80 to 0.8 would
// silently rewrite a value the user chose, and `max_price` is opaque to
// squall by F33 anyway.
//
// A resource.Quantity "m" (milli) suffix (e.g. "2200m") is deliberately NOT
// accepted, even though the type this replaced would have parsed it as
// 2.2 (ledger D31 addendum). There has never been a released Squall, so
// there is no installed base of CRs to be compatible with, and the old
// consumer (engine.go's enginePlacement) now sends this string to dstack
// VERBATIM rather than through Quantity's AsDec() normalisation — so
// "2200m" would either be rejected by dstack or, worse, read literally as
// $2200/h instead of the $2.20/h a Quantity-typed field would have meant. A
// 1000x price ambiguity is not a spelling worth keeping for compatibility
// that does not exist.
//
// D70: a `+kubebuilder:validation:XValidation` CEL rule was tried here to
// restore admission-time content validation and DOES NOT WORK — the API
// server refuses to install the CRD at all, with or without a Price-shaped
// rule; even `rule: "true"` fails the same way. Kubernetes's CEL support
// requires a structural schema with a single concrete type per node to
// build its CEL type environment; `type: ""` + PreserveUnknownFields
// (needed below for D31) has no such type, so
// "failed to construct type information for x-kubernetes-validations
// rules: unable to convert structural schema to CEL declarations" is
// unconditional for this node, not a property of the rule text. Verified
// empirically against a real envtest (Kubernetes 1.31) apiserver; see D70
// in docs/references/deviations-and-findings.md for the full negative
// result and what else was tried. Do not re-add a CEL marker here without
// re-reading that entry first.
//
// D70's resolution (0.1.0 Task 2, fix round 1): since admission cannot
// validate content, decoding must not either. UnmarshalJSON below is total
// — it never fails — so a bad price never becomes a silent, unreadable
// decode error on every reconcile (a status a user will never see). Content
// validation moved to the separate Validate method, whose error is meant to
// be read by a human, in a status condition (Task 5 wires this in).
//
// +kubebuilder:validation:Type=""
// +kubebuilder:pruning:PreserveUnknownFields
type Price string

func (p Price) String() string { return string(p) }

// UnmarshalJSON is TOTAL: it never returns an error, so decoding a Model
// always succeeds regardless of what maxPricePerHour contains. This is
// deliberate (D70) — the API server cannot validate this field's content at
// admission (see the type doc comment), so a decode error here would be the
// ONLY signal a bad price ever produced, and it would land in a controller
// log line nobody reads rather than anywhere a user looks. Content
// validation is Validate, not this method.
//
// It accepts a JSON number or a JSON string and keeps the literal text
// (unquoted). Any other JSON shape (bool, object, array) is stored as its
// raw JSON text instead — still not silently dropped, just not yet judged;
// Validate rejects all of it as "not a number" like any other garbage.
func (p *Price) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal(b, &unquoted); err == nil {
			s = unquoted
		}
		// A malformed JSON string literal cannot reach here in practice:
		// encoding/json already requires b to be well-formed JSON before it
		// ever calls UnmarshalJSON. Falling through with the raw bytes on
		// the (unreachable) error path keeps this method total regardless.
	}
	*p = Price(s)
	return nil
}

// Validate reports whether p is a plain decimal number — quoted or
// unquoted, and nothing else. This is the content check UnmarshalJSON used
// to perform before D70's resolution moved it here: permissive about
// SPELLING was never meant to mean permissive about MEANING. An unparseable
// ceiling would either match no offer (indistinguishable from an empty
// market, D58) or, worse, be read as something the CR author never wrote.
//
// A resource.Quantity "m" (milli) suffix, e.g. "2200m", is deliberately
// REJECTED (ledger D31 addendum): there is no released Squall and no
// installed base of CRs carrying it, and the consumer now sends this string
// to dstack verbatim rather than through Quantity's AsDec() normalisation —
// so "2200m" would mean $2200/h, not the $2.20/h a Quantity-typed field
// would have read it as. A 1000x price ambiguity is not worth keeping for
// compatibility that does not exist.
//
// The error is meant to be read by a person: Task 5's reconciler surfaces
// it verbatim on the Model's Schedulable condition and refuses to
// provision rather than guess at a cost ceiling the user did not actually
// write.
func (p Price) Validate() error {
	v, err := strconv.ParseFloat(string(p), 64)
	if err != nil {
		return fmt.Errorf("maxPricePerHour: %q is not a plain decimal number, e.g. 1.20 or \"1.20\"", string(p))
	}
	// D117: ParseFloat happily returns "Inf", "NaN" and negatives, and this
	// string goes VERBATIM to dstack's max_price, where pydantic's float
	// coercion accepts Inf too — so without these three rejections the one
	// check documented as squall's money safety valve waves through an
	// unbounded cost ceiling. Zero and negatives are rejected as well: a
	// non-positive ceiling yields zero offers, which D58 records as
	// indistinguishable from an empty market.
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return fmt.Errorf("maxPricePerHour: %q is not a finite number — an infinite or NaN ceiling is no ceiling at all", string(p))
	}
	if v <= 0 {
		return fmt.Errorf("maxPricePerHour: %q must be a positive amount per hour", string(p))
	}
	return nil
}

// MarshalJSON always emits a string, so what is stored in etcd and what is
// sent to dstack agree, and a round-trip through the API server does not
// change the spelling the user wrote.
func (p Price) MarshalJSON() ([]byte, error) { return json.Marshal(string(p)) }

// SPDX-License-Identifier: MIT

package v1alpha1

import "time"

// ActivityPath is the HTTP path squall-proxy (Phase 9) serves on every
// replica and squall-controller polls to gather §6's idle evidence: one
// call per replica returning every Model it currently routes to, not one
// call per (replica, Model) pair. It lives here, not in
// internal/controller/squall, for the same reason as DemandAnnotation: a
// controller-runtime-free squall-proxy must be able to import this
// contract cheaply.
const ActivityPath = "/internal/activity"

// +kubebuilder:object:generate=false

// ModelActivity is one Model's activity as reported by a single replica.
// Not a CRD field (never reachable from Model's runtime.Object graph) —
// generate=false skips controller-gen's DeepCopy, which otherwise assumes
// any Time field is metav1.Time.
//
// A replica MUST omit a Model entirely from ActivityReport.Models if it has
// never routed to it — that absence is "no data", distinct from the
// explicit zero value ModelActivity{} ("0 in-flight, never served"). Callers
// decoding this wire contract must check for key presence and must never
// default a missing key to InFlight: 0 — see the "Ambiguous" definition in
// docs/plans/2026-08-26-block-7-8-sleep-and-drain.md §1.
type ModelActivity struct {
	// InFlight is the number of requests this replica has accepted for the
	// Model but not yet finished proxying, at the instant this report was
	// generated. Negative is never valid and must be treated as ambiguous
	// by the reader, not clamped to zero.
	InFlight int `json:"inFlight"`

	// LastRequestAt is when this replica last accepted a request for the
	// Model. The zero value means "never" and is not itself ambiguous: a
	// replica newly routing to a Model it has never served, reporting
	// InFlight: 0 and a zero LastRequestAt, is legitimate idle evidence.
	LastRequestAt time.Time `json:"lastRequestAt"`

	// LastSuccessAt is when this replica last COMMITTED a forward for the
	// Model — i.e. the gateway answered something other than 502/503 and
	// the response was streamed to a real client. It is §6's evidence (b):
	// first-party proof the engine is serving, distinct from LastRequestAt
	// (which records acceptance, not success).
	//
	// The zero value means "no successful forward observed by this
	// replica" and is NOT ambiguous. A reader MUST treat a report that
	// omits this field entirely as zero, never as ambiguous — an older
	// proxy replica mid-rollout must not wedge §6's sleep aggregation.
	LastSuccessAt time.Time `json:"lastSuccessAt"`

	// FailuresSinceSuccess is how many requests this replica has failed for
	// this Model since it last delivered a 2xx IN FULL. Reset to 0 by every
	// success, so it is a CONSECUTIVE-failure count, not a lifetime total.
	//
	// It is the evidence floor under the unhealthy teardown (spec.
	// unhealthyFailureThreshold). Time alone has a cheap counter-example: a
	// Model that served fine, went quiet for twenty minutes and then failed ONE
	// request has a twenty-minute-old last success and current traffic, and
	// would be torn down on the strength of that single failure.
	//
	// An older proxy replica mid-rollout omits this field and it decodes to 0,
	// which reads as "no failure evidence here" and can only ever PREVENT a
	// teardown. That is the safe direction and is deliberate.
	FailuresSinceSuccess int `json:"failuresSinceSuccess"`
}

// +kubebuilder:object:generate=false

// ActivityReport is the full body squall-proxy serves at ActivityPath: one
// replica's view of every Model it currently routes to.
type ActivityReport struct {
	Models map[string]ModelActivity `json:"models"`
}

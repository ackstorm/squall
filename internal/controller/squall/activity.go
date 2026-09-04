// SPDX-License-Identifier: MIT

package squall

import "time"

// ActivityQuery is one proxy replica's answer, decoded from its
// ActivityPath response, for a single Model — the input to
// aggregateActivity. It is the impure layer's job (gatherActivity) to
// produce these; aggregateActivity itself does no I/O.
type ActivityQuery struct {
	// Address identifies the replica this query is for; it must match one
	// of aggregateActivity's expected addresses to count.
	Address string

	// OK is false for any transport-level failure: timeout, refusal,
	// non-200, or an unparseable body. False overrides every other field.
	OK bool

	// NoData is true when the replica's report was well-formed but had no
	// key for this Model — "never routed to it", not "0 in-flight". Must
	// never be defaulted to false by a caller that simply forgot to check
	// for the key.
	//
	// It is EVIDENCE OF IDLENESS, not ambiguity, and aggregateActivity
	// treats it as such — see the "never routed" paragraph there. What it
	// must not do is contribute a LastRequestAt: a replica that never saw
	// the Model has no timestamp to offer, and letting its zero value into
	// the aggregation would be indistinguishable from "last seen at the
	// epoch".
	NoData bool

	InFlight      int
	LastRequestAt time.Time
	LastSuccessAt time.Time

	// FailuresSinceSuccess is this replica's consecutive-failure count for the
	// Model. Absent from an older proxy's report and therefore 0, which can
	// only PREVENT a teardown — the safe direction.
	FailuresSinceSuccess int
}

// aggregateActivity is §6's pure idle-evidence aggregation (block 7+8 plan
// §1, Task 7.1): the expected set of replica addresses (this pass's fresh
// EndpointSlice listing) plus each one's query result, folded into a single
// ActivityEvidence. No I/O, no clock reads — now is supplied by the
// caller, purely to detect an ambiguous future LastRequestAt.
//
// Complete is true only when every expected address has exactly one
// unambiguous, on-time query result. An empty expected set is never
// vacuously complete (block 7+8 plan §7.1's EndpointSlice-list-error case
// collapses to this by construction: gatherActivity must pass an empty
// expected slice on a List error, never silently treat it as "zero
// addresses observed").
//
// A replica that has NEVER ROUTED to this Model (NoData) counts as idle.
// This reading is load-bearing and was learned the expensive way — MEASURED
// LIVE 2026-08-29: the proxy Deployment runs two replicas, one client with
// keep-alive pinned every request to ONE of them, and the other's report
// stayed `{"models":{}}` forever. Treating that as ambiguous made
// sleepDue() return false on EVERY pass, so the 1->0 flip was unreachable
// for the entire lifetime of the deployment. The GPU was never released by
// squall; the bill only stopped because dstack's own run failed. A sleep
// path that cannot fire is not a cautious sleep path, it is an absent one.
//
// It is SOUND, not a relaxation of "1->0 fails safe": ActivityTracker.Begin
// inserts a Model's key BEFORE any upstream call and never removes it, so a
// replica holding an in-flight request for this Model always has a key for
// it. No key therefore PROVES zero in-flight there. The genuinely ambiguous
// case — a replica that may be serving but whose report could not be read —
// is OK: false, which still poisons completeness above.
//
// A report with no key is still complete evidence of zero in-flight work; the
// durable controller anchor supplies the time bound when every replica has
// never seen this Model. That keeps a freshly woken Model able to sleep after
// its demand has aged without pretending an absent timestamp is recent data.
func aggregateActivity(expected []string, queries []ActivityQuery, now time.Time) ActivityEvidence {
	if len(expected) == 0 {
		return ActivityEvidence{}
	}

	want := make(map[string]struct{}, len(expected))
	for _, addr := range expected {
		want[addr] = struct{}{}
	}

	got := make(map[string]ActivityQuery, len(queries))
	for _, q := range queries {
		if _, ok := want[q.Address]; !ok {
			continue // stale/removed replica's result is not counted (T6).
		}
		got[q.Address] = q
	}
	if len(got) != len(want) {
		return ActivityEvidence{} // T5: something expected was never queried.
	}

	allIdle := true
	sawData := false
	failures := 0
	var newest, newestSuccess time.Time
	for addr := range want {
		q := got[addr]
		if !q.OK || q.InFlight < 0 || q.LastRequestAt.After(now) {
			return ActivityEvidence{} // ambiguous or unreachable -> incomplete.
		}
		if q.NoData {
			// Idle here, and contributing no timestamp — see the doc
			// comment. Deliberately NOT counted towards sawData: a report
			// with no key is not data about when this Model was last used.
			continue
		}
		sawData = true
		if q.InFlight != 0 {
			allIdle = false
		}
		if q.LastRequestAt.After(newest) {
			newest = q.LastRequestAt
		}
		if q.LastSuccessAt.After(newestSuccess) {
			newestSuccess = q.LastSuccessAt
		}
		// SUMMED, not maxed: the question the unhealthy verdict asks is "have
		// we tried enough times to be sure", and three failures spread over
		// three replicas is the same amount of evidence as three on one.
		if q.FailuresSinceSuccess > 0 {
			failures += q.FailuresSinceSuccess
		}
	}

	if !sawData {
		// Every replica answered, none had ever routed to this Model. The
		// absence of a key proves zero in-flight work; status supplies time.
		return ActivityEvidence{Complete: true, AllIdle: true}
	}

	return ActivityEvidence{
		Complete:             true,
		AnyData:              true,
		AllIdle:              allIdle,
		NewestLastRequestAt:  newest,
		NewestLastSuccessAt:  newestSuccess,
		FailuresSinceSuccess: failures,
	}
}

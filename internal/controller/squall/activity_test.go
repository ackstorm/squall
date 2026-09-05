// SPDX-License-Identifier: MIT

package squall

import (
	"testing"
	"time"
)

// TestAggregateActivity is the pure table test for §6's idle-evidence
// aggregation (Task 7.1). It is the block 7+8 plan's T2-T5: T1's time
// comparison and T7's pin gate live one layer up, in phase.go's Decide
// and sleepDue, since aggregateActivity itself has no notion of
// spec.IdleTimeout or spec.MinReplicas.
func TestAggregateActivity(t *testing.T) {
	t1 := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	now := t2.Add(time.Hour)

	tests := []struct {
		name string

		expected []string
		queries  []ActivityQuery

		wantComplete bool
		wantAllIdle  bool
		wantNewest   time.Time
	}{
		{
			name:         "no addresses expected -> incomplete (never vacuously complete)",
			expected:     nil,
			queries:      nil,
			wantComplete: false,
		},
		{
			name:     "all idle -> complete, all idle, newest is the max LastRequestAt",
			expected: []string{"10.0.0.1:8000", "10.0.0.2:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, InFlight: 0, LastRequestAt: t1},
				{Address: "10.0.0.2:8000", OK: true, InFlight: 0, LastRequestAt: t2},
			},
			wantComplete: true,
			wantAllIdle:  true,
			wantNewest:   t2,
		},
		{
			// T2: A in-flight, B idle -> no flip. AND semantics, not OR.
			name:     "one replica in-flight -> complete but not all idle",
			expected: []string{"10.0.0.1:8000", "10.0.0.2:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, InFlight: 1, LastRequestAt: t1},
				{Address: "10.0.0.2:8000", OK: true, InFlight: 0, LastRequestAt: t1},
			},
			wantComplete: true,
			wantAllIdle:  false,
		},
		{
			// T3 (canonical): B unreachable, A idle -> no flip. A query
			// error must never be read as InFlight: 0.
			name:     "one replica unreachable -> incomplete, not vacuously idle",
			expected: []string{"10.0.0.1:8000", "10.0.0.2:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, InFlight: 0, LastRequestAt: t1},
				{Address: "10.0.0.2:8000", OK: false},
			},
			wantComplete: false,
		},
		{
			// THE 2026-08-29 BUG, in one case. Two proxy replicas, one
			// client with keep-alive: every request pinned to A, and B's
			// report stayed `{"models":{}}` for the deployment's whole
			// life. Reading B as ambiguous made this incomplete on every
			// pass, so the 1->0 flip was unreachable and the GPU was never
			// released. B has no key, so B provably holds nothing.
			name:     "one replica never routed to this model -> complete and idle",
			expected: []string{"10.0.0.1:8000", "10.0.0.2:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, InFlight: 0, LastRequestAt: t1},
				{Address: "10.0.0.2:8000", OK: true, NoData: true},
			},
			wantComplete: true,
			wantAllIdle:  true,
			wantNewest:   t1,
		},
		{
			// A no-data replica must not veto a BUSY one either way round:
			// the busy one still decides AllIdle.
			name:     "one replica never routed, the other is busy -> complete, not idle",
			expected: []string{"10.0.0.1:8000", "10.0.0.2:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, InFlight: 3, LastRequestAt: t1},
				{Address: "10.0.0.2:8000", OK: true, NoData: true},
			},
			wantComplete: true,
			wantAllIdle:  false,
			wantNewest:   t1,
		},
		{
			// Every replica answered and proved it has no in-flight key;
			// status supplies the missing last-request timestamp.
			name:     "no replica has data -> complete and idle",
			expected: []string{"10.0.0.1:8000", "10.0.0.2:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, NoData: true},
				{Address: "10.0.0.2:8000", OK: true, NoData: true},
			},
			wantComplete: true,
			wantAllIdle:  true,
		},
		{
			// NoData is idleness; unreachable is still ambiguity. Keeping
			// these apart is the whole point — this must stay incomplete.
			name:     "one replica never routed, another unreachable -> incomplete",
			expected: []string{"10.0.0.1:8000", "10.0.0.2:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, NoData: true},
				{Address: "10.0.0.2:8000", OK: false},
			},
			wantComplete: false,
		},
		{
			// Ambiguous: negative InFlight.
			name:     "negative InFlight -> incomplete",
			expected: []string{"10.0.0.1:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, InFlight: -1, LastRequestAt: t1},
			},
			wantComplete: false,
		},
		{
			// Ambiguous: LastRequestAt in the future.
			name:     "future LastRequestAt -> incomplete",
			expected: []string{"10.0.0.1:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, InFlight: 0, LastRequestAt: now.Add(time.Hour)},
			},
			wantComplete: false,
		},
		{
			// T5: a replica the caller listed as expected but did not
			// include a query result for at all (not yet queried) must
			// not be silently dropped from the completeness check.
			name:     "expected replica missing from queries entirely -> incomplete",
			expected: []string{"10.0.0.1:8000", "10.0.0.2:8000", "10.0.0.3:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, InFlight: 0, LastRequestAt: t1},
				{Address: "10.0.0.2:8000", OK: true, InFlight: 0, LastRequestAt: t1},
			},
			wantComplete: false,
		},
		{
			// A query result for an address the caller no longer lists
			// as expected (stale/removed replica) must not count toward
			// completeness or idleness either.
			name:     "extra query result for an unexpected address is ignored",
			expected: []string{"10.0.0.1:8000"},
			queries: []ActivityQuery{
				{Address: "10.0.0.1:8000", OK: true, InFlight: 0, LastRequestAt: t1},
				{Address: "10.0.0.9:8000", OK: true, InFlight: 5, LastRequestAt: t1},
			},
			wantComplete: true,
			wantAllIdle:  true,
			wantNewest:   t1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregateActivity(tt.expected, tt.queries, now)

			if got.Complete != tt.wantComplete {
				t.Fatalf("Complete = %v, want %v", got.Complete, tt.wantComplete)
			}
			if !tt.wantComplete {
				return // AllIdle/NewestLastRequestAt are meaningless otherwise.
			}
			if got.AllIdle != tt.wantAllIdle {
				t.Errorf("AllIdle = %v, want %v", got.AllIdle, tt.wantAllIdle)
			}
			if tt.wantAllIdle && !got.NewestLastRequestAt.Equal(tt.wantNewest) {
				t.Errorf("NewestLastRequestAt = %v, want %v", got.NewestLastRequestAt, tt.wantNewest)
			}
		})
	}
}

// TestAggregateActivity_LastSuccessAt_DoesNotAffectCompleteness is the
// rollout-safety rule: adding a field must not turn an older replica's
// report into "ambiguous" and wedge §6's sleep for the whole cluster.
func TestAggregateActivity_LastSuccessAt_DoesNotAffectCompleteness(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	queries := []ActivityQuery{
		{Address: "10.0.0.1", OK: true, InFlight: 0, LastRequestAt: now.Add(-time.Hour)}, // no LastSuccessAt: an older replica
		{Address: "10.0.0.2", OK: true, InFlight: 0, LastRequestAt: now.Add(-time.Hour), LastSuccessAt: now.Add(-2 * time.Minute)},
	}

	got := aggregateActivity([]string{"10.0.0.1", "10.0.0.2"}, queries, now)
	if !got.Complete {
		t.Fatal("Complete = false because one replica omitted LastSuccessAt; a mid-rollout replica must not wedge sleep")
	}
	if !got.AllIdle {
		t.Fatal("AllIdle = false, want true: both replicas report 0 in-flight")
	}
	if want := now.Add(-2 * time.Minute); !got.NewestLastSuccessAt.Equal(want) {
		t.Fatalf("NewestLastSuccessAt = %v, want %v (the newest across replicas)", got.NewestLastSuccessAt, want)
	}
}

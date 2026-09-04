// SPDX-License-Identifier: MIT

package squall

import (
	"testing"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestHasDemand closes D23: hasDemand's malformed-DemandAnnotation branch
// (the fail-toward-false half of D21) had zero direct test coverage —
// every other test in this package feeds Decide's hasDemand bool directly
// or writes a well-formed RFC3339 value. Fail-toward-false is a deliberate
// design choice (D21, ACCEPTED): a malformed value must never be read as
// "demand forever", the pre-TTL bug self-expiry replaced.
func TestHasDemand(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	const ttlSeconds = 300 // ScaleDownDelaySeconds

	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{
			name:        "no annotation at all -> false",
			annotations: nil,
			want:        false,
		},
		{
			name:        "malformed value (not RFC3339) -> false, fail toward no-demand",
			annotations: map[string]string{squallv1alpha1.DemandAnnotation: "not-a-timestamp"},
			want:        false,
		},
		{
			name:        "empty string value -> false",
			annotations: map[string]string{squallv1alpha1.DemandAnnotation: ""},
			want:        false,
		},
		{
			name:        "well-formed, within TTL -> true",
			annotations: map[string]string{squallv1alpha1.DemandAnnotation: now.Add(-time.Minute).Format(time.RFC3339)},
			want:        true,
		},
		{
			name:        "well-formed, expired past TTL -> false (self-expiry)",
			annotations: map[string]string{squallv1alpha1.DemandAnnotation: now.Add(-time.Hour).Format(time.RFC3339)},
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &squallv1alpha1.Model{}
			m.Annotations = tt.annotations
			m.Spec.ScaleDownDelaySeconds = ttlSeconds

			got := hasDemand(m, now)
			if got != tt.want {
				t.Errorf("hasDemand() = %v, want %v", got, tt.want)
			}
		})
	}
}

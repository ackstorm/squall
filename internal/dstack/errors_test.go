// SPDX-License-Identifier: MIT

package dstack

import (
	"errors"
	"testing"
)

// TestClassifyError pins the MEASURED error contract of dstack 0.21.2
// (docs/references/dstack-real-api.md §8.1). dstack answers HTTP 400 for
// BOTH "not found" and "CAS conflict" — the status line cannot distinguish
// them, so the body must. Keying off 404/409, as this client did before,
// silently disables F20 and F18 against a real server.
func TestClassifyError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "200 is not an error",
			status: 200,
			body:   `{"id":"x"}`,
			want:   nil,
		},
		{
			name:   "400 + resource_not_exists is ErrNotFound",
			status: 400,
			body:   `{"detail":[{"msg":"Run not found","code":"resource_not_exists"}]}`,
			want:   ErrNotFound,
		},
		{
			name:   "400 + resource-has-been-changed is ErrResourceChanged",
			status: 400,
			body:   `{"detail":[{"msg":"Failed to apply plan. Resource has been changed. Try again or use force apply.","code":"error"}]}`,
			want:   ErrResourceChanged,
		},
		{
			name:   "403 + object detail is ErrUnauthorized",
			status: 403,
			body:   `{"detail":{"msg":"Invalid token","code":null}}`,
			want:   ErrUnauthorized,
		},
		{
			name:   "400 with an unrecognised code is a generic error",
			status: 400,
			body:   `{"detail":[{"msg":"something else entirely","code":"error"}]}`,
			want:   nil, // asserted separately: non-nil, but none of the sentinels
		},
		{
			name:   "500 with an unparseable body is still an error",
			status: 500,
			body:   `<html>gateway timeout</html>`,
			want:   nil, // asserted separately
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyError(tc.status, []byte(tc.body))

			switch {
			case tc.want != nil:
				if !errors.Is(got, tc.want) {
					t.Fatalf("classifyError(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
				}
			case tc.status == 200:
				if got != nil {
					t.Fatalf("classifyError(200, ...) = %v, want nil", got)
				}
			default:
				if got == nil {
					t.Fatalf("classifyError(%d, %s) = nil, want a non-nil error", tc.status, tc.body)
				}
				for _, sentinel := range []error{ErrNotFound, ErrResourceChanged, ErrUnauthorized} {
					if errors.Is(got, sentinel) {
						t.Fatalf("classifyError(%d, %s) = %v, must NOT match sentinel %v", tc.status, tc.body, got, sentinel)
					}
				}
			}
		})
	}
}

// TestClassifyError_NotFoundIsNotStatus404 is the anti-regression for the
// exact bug this task fixes: a real dstack NEVER answers 404 for a missing
// run, so a 404-keyed mapping must not be what produces ErrNotFound.
func TestClassifyError_NotFoundIsNotStatus404(t *testing.T) {
	if got := classifyError(404, []byte(`{"detail":"Service main/x not found"}`)); errors.Is(got, ErrNotFound) {
		t.Fatal("a bare 404 produced ErrNotFound: the run API answers 400 + code, and 404 belongs to the service proxy (F23), not to run lookup")
	}
}

// TestClassifyError_CannotOverrideActiveRun pins the sentinel that keeps a
// routine spec edit from silently wedging every future wake.
//
// dstack answers HTTP 400 with the generic code "error" for this, exactly as
// it does for the CAS conflict, so the message is the only discriminator and
// the two must not be confused: a CAS conflict should be re-read and retried,
// this one can never succeed on retry.
func TestClassifyError_CannotOverrideActiveRun(t *testing.T) {
	body := []byte(`{"detail":[{"code":"error","msg":"Cannot override active run"}]}`)
	err := classifyError(400, body)

	if !errors.Is(err, ErrCannotOverride) {
		t.Fatalf("got %v, want ErrCannotOverride", err)
	}
	if errors.Is(err, ErrResourceChanged) {
		t.Fatal("must not be confused with the CAS conflict: retrying this one can never succeed")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrCannotOverride", err)
	}
}

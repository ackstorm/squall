// SPDX-License-Identifier: Apache-2.0

package dstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrUnauthorized is returned when the dstack server rejects our token.
// F23 keeps this distinct from every other failure: an auth fault is never
// a reason to wake anything, and never a transient to retry.
var ErrUnauthorized = errors.New("dstack: unauthorized")

// errorEnvelope is dstack's error body. `detail` is polymorphic: a LIST of
// {msg, code} for API errors, a bare OBJECT for auth errors, and a plain
// string from the service proxy. Measured on 0.21.2 — see
// docs/references/dstack-real-api.md §8.1.
type errorEnvelope struct {
	Detail json.RawMessage `json:"detail"`
}

type errorDetail struct {
	Msg  string `json:"msg"`
	Code string `json:"code"`
}

// resourceNotExistsCode is dstack's own code for a missing resource.
const resourceNotExistsCode = "resource_not_exists"

// resourceChangedMarker identifies the CAS conflict. dstack tags it with
// the generic code "error", so the message is the only discriminator it
// gives us. Matching a substring of an upstream string is fragile by
// nature; it is pinned by the Tier-1 e2e (task 6), which fails loudly if
// upstream ever rewords it.
const resourceChangedMarker = "resource has been changed"

// cannotOverrideMarker identifies dstack refusing an apply whose spec differs
// from the live run's in ANYTHING but the replica count. Same shape as the CAS
// marker above: dstack gives it the generic code "error", so the message is
// the only discriminator, and it is equally fragile to an upstream rewording.
//
// It needs its own sentinel because it is the one dstack error that makes
// `0->1` fail CLOSED, which the invariant forbids. MEASURED 2026-08-31 (the
// D115 addendum): a flip that sent SSHKeyPub "" was refused this way on every
// retry while the GPU billed for two hours. The same 400 answers a wake after
// any routine `spec.env` edit or secret rotation, so without a sentinel an
// ordinary configuration change wedges every future wake behind a mute
// backoff.
const cannotOverrideMarker = "cannot override active run"

// HTTPError carries the raw HTTP status code dstack answered, for callers
// that need to react to a status none of the sentinels above cover — e.g.
// BackendConfigured's 200-vs-400 (D67), for which dstack has no dedicated
// error code. It wraps whatever error an ordinary caller would have seen
// (via Unwrap), so errors.Is against ErrNotFound / ErrResourceChanged /
// ErrUnauthorized keeps working exactly as before this type existed.
type HTTPError struct {
	StatusCode int
	err        error
}

func (e *HTTPError) Error() string { return e.err.Error() }
func (e *HTTPError) Unwrap() error { return e.err }

// classifyError maps one dstack response to this package's sentinels.
// Status codes alone are NOT sufficient: dstack answers HTTP 400 for both
// "not found" and "CAS conflict".
func classifyError(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	return &HTTPError{StatusCode: status, err: classifyErrorBody(status, body)}
}

// classifyErrorBody is classifyError's non-2xx path, split out so
// HTTPError's StatusCode can wrap every branch below in one place rather
// than duplicating the wrap at each return.
func classifyErrorBody(status int, body []byte) error {
	details := parseDetails(body)
	for _, d := range details {
		if d.Code == resourceNotExistsCode {
			return fmt.Errorf("%w: %s", ErrNotFound, d.Msg)
		}
		if strings.Contains(strings.ToLower(d.Msg), resourceChangedMarker) {
			return fmt.Errorf("%w: %s", ErrResourceChanged, d.Msg)
		}
		if strings.Contains(strings.ToLower(d.Msg), cannotOverrideMarker) {
			return fmt.Errorf("%w: %s", ErrCannotOverride, d.Msg)
		}
	}

	if status == 401 || status == 403 {
		return fmt.Errorf("%w: %s", ErrUnauthorized, summarise(details, body))
	}
	return fmt.Errorf("dstack: http %d: %s", status, summarise(details, body))
}

// parseDetails copes with all three `detail` shapes, returning an empty
// slice rather than an error when the body is not dstack's envelope at all
// (an intermediary's HTML error page, say).
func parseDetails(body []byte) []errorDetail {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || len(env.Detail) == 0 {
		return nil
	}

	var list []errorDetail
	if err := json.Unmarshal(env.Detail, &list); err == nil {
		return list
	}
	var one errorDetail
	if err := json.Unmarshal(env.Detail, &one); err == nil {
		return []errorDetail{one}
	}
	var msg string
	if err := json.Unmarshal(env.Detail, &msg); err == nil {
		return []errorDetail{{Msg: msg}}
	}
	return nil
}

func summarise(details []errorDetail, body []byte) string {
	if len(details) > 0 && details[0].Msg != "" {
		return details[0].Msg
	}
	const max = 256
	if len(body) > max {
		return string(body[:max])
	}
	return string(body)
}

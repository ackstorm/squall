// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postJSON POSTs body (already-encoded JSON) to path and returns the
// response with its body fully read and the response closed, so callers
// never leak a connection mid-test.
func postJSON(t *testing.T, srv *httptest.Server, token, path, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of POST %s: %v", path, err)
	}
	return resp, buf
}

// applyOverHTTP walks the real two-step flow (get_plan -> apply) against
// srv, using currentResource verbatim as the CAS anchor ("null" for none).
// It returns apply's raw response so callers can assert on status/body.
func applyOverHTTP(t *testing.T, srv *httptest.Server, token, name string, replicas int, currentResource string) (*http.Response, []byte) {
	t.Helper()
	planBody := fmt.Sprintf(`{"run_spec":{"run_name":%q,"configuration":{"replicas":%d}}}`, name, replicas)
	planResp, planRaw := postJSON(t, srv, token, "/api/project/main/runs/get_plan", planBody)
	if planResp.StatusCode != http.StatusOK {
		t.Fatalf("get_plan: got %d, want 200: %s", planResp.StatusCode, planRaw)
	}
	var plan struct {
		RunSpec json.RawMessage `json:"run_spec"`
	}
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		t.Fatalf("decode get_plan response: %v", err)
	}

	applyBody := fmt.Sprintf(`{"plan":{"run_spec":%s,"current_resource":%s},"force":false}`, plan.RunSpec, currentResource)
	return postJSON(t, srv, token, "/api/project/main/runs/apply", applyBody)
}

// TestHTTP_ApplyAndGateway_ShareStateWithDirectCalls proves the HTTP surface
// and the direct Go calls drive one state machine, not two: a mutation made
// through one surface must be visible through the other.
func TestHTTP_ApplyAndGateway_ShareStateWithDirectCalls(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	resp, _ := applyOverHTTP(t, srv, ValidToken, "qwen", 1, "null")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply: got status %d, want 200", resp.StatusCode)
	}

	// Flip to 0 via a DIRECT call — must be visible over HTTP.
	run, err := s.Get("qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, Current: run})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/gateway/qwen", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+ValidToken)
	gwResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /gateway/qwen: %v", err)
	}
	_ = gwResp.Body.Close()
	if gwResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("gateway over HTTP must see the direct-call flip: got %d, want 503", gwResp.StatusCode)
	}

	// And a direct call must see state mutated over HTTP (the apply above).
	if code := s.GatewayGet("qwen", ValidToken); code != http.StatusServiceUnavailable {
		t.Fatalf("direct GatewayGet must see state applied over HTTP: got %d, want 503", code)
	}
}

// TestHTTP_Apply_RejectsForceAndStaleCurrent covers both F18 halves over the
// real wire: force is refused, and a stale CAS anchor is rejected — both as
// HTTP 400, MEASURED (§8.1), never 409.
func TestHTTP_Apply_RejectsForceAndStaleCurrent(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	planBody := `{"run_spec":{"run_name":"qwen","configuration":{"replicas":1}}}`
	_, planRaw := postJSON(t, srv, ValidToken, "/api/project/main/runs/get_plan", planBody)
	var plan struct {
		RunSpec json.RawMessage `json:"run_spec"`
	}
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		t.Fatalf("decode get_plan response: %v", err)
	}
	forcedBody := fmt.Sprintf(`{"plan":{"run_spec":%s,"current_resource":null},"force":true}`, plan.RunSpec)
	resp, body := postJSON(t, srv, ValidToken, "/api/project/main/runs/apply", forcedBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("F18: force over HTTP must be refused with 400, got %d: %s", resp.StatusCode, body)
	}

	applyResp, applyBody := applyOverHTTP(t, srv, ValidToken, "qwen", 1, "null")
	if applyResp.StatusCode != http.StatusOK {
		t.Fatalf("seed apply: got %d, want 200: %s", applyResp.StatusCode, applyBody)
	}

	staleResp, staleBody := applyOverHTTP(t, srv, ValidToken, "qwen", 0, `{"id":"no-such-run","deployment_num":999}`)
	if staleResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("F18: stale current_resource over HTTP must be rejected with 400 (measured §8.1), got %d: %s", staleResp.StatusCode, staleBody)
	}
	var errBody struct {
		Detail []struct {
			Code string `json:"code"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(staleBody, &errBody); err != nil || len(errBody.Detail) == 0 {
		t.Fatalf("stale current_resource error body = %s, want a detail list", staleBody)
	}
}

func TestHTTP_Gateway_BadToken_Is403(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/gateway/qwen", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /gateway/qwen: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("F23: bad token must be 403, got %d", resp.StatusCode)
	}
}

// TestHTTP_Gateway_BadTokenBeatsUnknownService covers F23's anti-leak
// ordering over the HTTP surface: a bad token against a service that does
// not exist must still be 403, not 404.
func TestHTTP_Gateway_BadTokenBeatsUnknownService(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/gateway/no-such-service", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /gateway/no-such-service: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("F23: bad token against an unknown service must be 403 over HTTP too, got %d", resp.StatusCode)
	}
}

// TestHTTP_Gateway_RegisteredAwake_Is200 covers the gateway happy path over
// the HTTP surface, which otherwise no test in the suite reaches.
func TestHTTP_Gateway_RegisteredAwake_Is200(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/gateway/qwen", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+ValidToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /gateway/qwen: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("F23: registered + awake must be 200 over HTTP, got %d", resp.StatusCode)
	}
}

// TestHTTP_GetPlan_MalformedJSON_Is400 covers the decode-error path in
// handleGetPlan.
func TestHTTP_GetPlan_MalformedJSON_Is400(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	resp, _ := postJSON(t, srv, ValidToken, "/api/project/main/runs/get_plan", `{not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON body must be 400, got %d", resp.StatusCode)
	}
}

// TestHTTP_GetPlan_BadToken_Is403 covers auth on the run-management surface.
func TestHTTP_GetPlan_BadToken_Is403(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	resp, _ := postJSON(t, srv, "wrong-token", "/api/project/main/runs/get_plan", `{"run_spec":{"run_name":"qwen","configuration":{"replicas":1}}}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad token on get_plan must be 403, got %d", resp.StatusCode)
	}
}

// TestHTTP_GetRun_UnknownIsResourceNotExists covers Get's ErrNotFound
// mapping over the wire, MEASURED (§8.1): HTTP 400 + resource_not_exists,
// never 404.
func TestHTTP_GetRun_UnknownIsResourceNotExists(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	resp, body := postJSON(t, srv, ValidToken, "/api/project/main/runs/get", `{"run_name":"no-such-service"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET on unknown name: got %d, want 400 (measured §8.1): %s", resp.StatusCode, body)
	}
}

// TestHTTP_GetRun_TerminalIsSuccessWithStatus proves F20 survives the HTTP
// hop, MEASURED (§9.4): a terminated run's run-management GET succeeds,
// reporting a terminal Status — never 400/404.
func TestHTTP_GetRun_TerminalIsSuccessWithStatus(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	s.Terminate("qwen")

	resp, body := postJSON(t, srv, ValidToken, "/api/project/main/runs/get", `{"run_name":"qwen"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("F20: GET on a terminal run must be 200 (measured §9.4), got %d: %s", resp.StatusCode, body)
	}
	var wire struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if wire.Status != statusTerminated {
		t.Fatalf("F20: GET on a terminal run: status = %q, want %q", wire.Status, statusTerminated)
	}
}

// TestHTTP_GetRun_Found returns the current run state over the wire.
func TestHTTP_GetRun_Found(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	resp, body := postJSON(t, srv, ValidToken, "/api/project/main/runs/get", `{"run_name":"qwen"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET run: got %d, want 200", resp.StatusCode)
	}
	var wire struct {
		RunSpec struct {
			RunName       string `json:"run_name"`
			Configuration struct {
				Replicas replicasWire `json:"replicas"`
			} `json:"configuration"`
		} `json:"run_spec"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if wire.RunSpec.RunName != "qwen" || wire.RunSpec.Configuration.Replicas.Min != 1 {
		t.Fatalf("GET run body = %+v, want run_name=qwen replicas.min=1", wire)
	}
}

// TestHTTP_GetRun_BadToken_Is403 proves the run-management surface enforces
// auth.
func TestHTTP_GetRun_BadToken_Is403(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	resp, _ := postJSON(t, srv, "wrong-token", "/api/project/main/runs/get", `{"run_name":"qwen"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad token on GET run must be 403, got %d", resp.StatusCode)
	}
}

// TestHTTP_GetRun_BadTokenBeatsUnknownRun proves the auth check runs BEFORE
// the existence lookup on the run-management surface too, mirroring F23's
// gateway anti-leak ordering.
func TestHTTP_GetRun_BadTokenBeatsUnknownRun(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	resp, _ := postJSON(t, srv, "wrong-token", "/api/project/main/runs/get", `{"run_name":"no-such-service"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad token against an unknown run must be 403, not leak via a not-found response: got %d", resp.StatusCode)
	}
}

// TestHTTP_DeleteRun_RemovesAndReadsBackNotFound exercises delete's happy
// path over HTTP and proves the removal is visible on the Get route too.
func TestHTTP_DeleteRun_RemovesAndReadsBackNotFound(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	// D56: a live run must be stopped before it can be deleted, exactly as
	// the real server requires.
	stopResp, stopBody := postJSON(t, srv, ValidToken, "/api/project/main/runs/stop", `{"runs_names":["qwen"],"abort":true}`)
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("STOP run: got %d, want 200: %s", stopResp.StatusCode, stopBody)
	}

	resp, body := postJSON(t, srv, ValidToken, "/api/project/main/runs/delete", `{"runs_names":["qwen"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE run: got %d, want 200: %s", resp.StatusCode, body)
	}

	getResp, _ := postJSON(t, srv, ValidToken, "/api/project/main/runs/get", `{"run_name":"qwen"}`)
	if getResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET after DELETE: got %d, want 400 (measured §8.1)", getResp.StatusCode)
	}
}

// TestHTTP_DeleteRun_UnknownIsResourceNotExists maps ErrNotFound on delete
// over the wire.
func TestHTTP_DeleteRun_UnknownIsResourceNotExists(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	resp, _ := postJSON(t, srv, ValidToken, "/api/project/main/runs/delete", `{"runs_names":["no-such-service"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("DELETE unknown: got %d, want 400 (measured §8.1)", resp.StatusCode)
	}
}

// TestHTTP_DeleteRun_BadToken_Is403 covers the delete route's auth check.
func TestHTTP_DeleteRun_BadToken_Is403(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	resp, _ := postJSON(t, srv, "wrong-token", "/api/project/main/runs/delete", `{"runs_names":["qwen"]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad token on DELETE run must be 403, got %d", resp.StatusCode)
	}
}

// TestHTTP_ListRuns_ReturnsAllKnownRuns covers the orphan-diff listing
// route, MEASURED (§9.4): a terminal run stays listed, with its terminal
// Status — not excluded, correcting this fake's earlier assumption.
func TestHTTP_ListRuns_ReturnsAllKnownRuns(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	s.MustApply(t, ApplyRequest{Name: "llama", Replicas: 0})
	s.MustApply(t, ApplyRequest{Name: "dead", Replicas: 1})
	s.Terminate("dead")

	resp, body := postJSON(t, srv, ValidToken, "/api/runs/list", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list runs: got %d, want 200: %s", resp.StatusCode, body)
	}
	var wires []struct {
		Status  string `json:"status"`
		RunSpec struct {
			RunName string `json:"run_name"`
		} `json:"run_spec"`
	}
	if err := json.Unmarshal(body, &wires); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(wires) != 3 {
		t.Fatalf("list runs: got %d runs, want 3 (dstack keeps terminal runs too): %+v", len(wires), wires)
	}
	byName := map[string]string{}
	for _, w := range wires {
		byName[w.RunSpec.RunName] = w.Status
	}
	if byName["dead"] != statusTerminated {
		t.Fatalf("list runs: dead run's status = %q, want %q, and it must not be excluded", byName["dead"], statusTerminated)
	}
}

// TestHTTP_ListRuns_BadToken_Is403 covers the list route's auth check.
func TestHTTP_ListRuns_BadToken_Is403(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	resp, _ := postJSON(t, srv, "wrong-token", "/api/runs/list", `{}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad token on list runs must be 403, got %d", resp.StatusCode)
	}
}

// TestHTTP_DeleteRun_RefusesAnActiveRun pins the wire shape of D56 so a
// caller cannot regress against the fake and pass. The status code and the
// message are both measured from dstack 0.21.2.
func TestHTTP_DeleteRun_RefusesAnActiveRun(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	resp, body := postJSON(t, srv, ValidToken, "/api/project/main/runs/delete", `{"runs_names":["qwen"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("DELETE on a live run: got %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Cannot delete active runs") {
		t.Fatalf("body = %s, want dstack's own refusal message", body)
	}

	getResp, _ := postJSON(t, srv, ValidToken, "/api/project/main/runs/get", `{"run_name":"qwen"}`)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("the run must survive a refused DELETE: got %d", getResp.StatusCode)
	}
}

// TestHTTP_Fleets_GetUnknownIsResourceNotExists mirrors the real dstack
// server's measured response (docs/references/dstack-real-api.md §9.8): a
// fleet name nothing has created answers 400/resource_not_exists, driving
// HTTPClient.EnsureFleet's fall-through to create.
func TestHTTP_Fleets_GetUnknownIsResourceNotExists(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	resp, body := postJSON(t, srv, ValidToken, "/api/project/main/fleets/get", `{"name":"squall-auto-vastai"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("fleets/get on an unknown name: got %d, want 400: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "resource_not_exists") {
		t.Fatalf("body = %s, want dstack's own resource_not_exists code", body)
	}
}

// TestHTTP_Fleets_ApplyThenGetSucceeds walks LIVE-7/D83's whole create path
// over the real wire: get_plan then apply must leave the fleet visible to a
// SUBSEQUENT fleets/get — the same state, not two disconnected mocks.
func TestHTTP_Fleets_ApplyThenGetSucceeds(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	planResp, planBody := postJSON(t, srv, ValidToken, "/api/project/main/fleets/get_plan",
		`{"spec":{"configuration":{"type":"fleet","name":"squall-auto-vastai","nodes":"0..","backends":["vastai"]}}}`)
	if planResp.StatusCode != http.StatusOK {
		t.Fatalf("fleets/get_plan: got %d, want 200: %s", planResp.StatusCode, planBody)
	}
	var plan struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(planBody, &plan); err != nil {
		t.Fatalf("decode get_plan response: %v", err)
	}

	applyResp, applyBody := postJSON(t, srv, ValidToken, "/api/project/main/fleets/apply",
		fmt.Sprintf(`{"plan":{"spec":%s},"force":false}`, plan.Spec))
	if applyResp.StatusCode != http.StatusOK {
		t.Fatalf("fleets/apply: got %d, want 200: %s", applyResp.StatusCode, applyBody)
	}
	if !s.HasFleet("squall-auto-vastai") {
		t.Fatal("fleets/apply over HTTP did not create the fleet in the shared state")
	}

	getResp, getBody := postJSON(t, srv, ValidToken, "/api/project/main/fleets/get", `{"name":"squall-auto-vastai"}`)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("fleets/get after apply: got %d, want 200: %s", getResp.StatusCode, getBody)
	}
}

// TestHTTP_Fleets_BadToken_Is403 matches the rest of the surface (D-package
// doc: auth is enforced on every route, before any existence lookup).
func TestHTTP_Fleets_BadToken_Is403(t *testing.T) {
	s := New()
	srv := httptest.NewServer(NewHTTPServer(s, ValidToken))
	t.Cleanup(srv.Close)

	resp, _ := postJSON(t, srv, "wrong-token", "/api/project/main/fleets/get", `{"name":"squall-auto-vastai"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("fleets/get with a bad token: got %d, want 403", resp.StatusCode)
	}
}

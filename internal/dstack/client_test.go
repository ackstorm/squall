// SPDX-License-Identifier: Apache-2.0

package dstack_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ackstorm/squall/internal/dstack"
	"github.com/ackstorm/squall/internal/dstack/mock"
)

func TestHTTPClient_Apply_SendsIdleDuration(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == getPlanPath {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"create"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","jobs":[],"run_spec":{"configuration":{"replicas":{"min":1,"max":1}}}}`))
	}))
	t.Cleanup(srv.Close)
	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "img", Port: 8080, IdleDuration: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := body["run_spec"].(map[string]any)["configuration"].(map[string]any)
	if cfg["idle_duration"] != "600s" {
		t.Fatalf("idle_duration = %v", cfg["idle_duration"])
	}
}

func TestHTTPClient_Apply_OmitsIdleDurationWhenZero(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == getPlanPath {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"create"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","jobs":[],"run_spec":{"configuration":{"replicas":{"min":1,"max":1}}}}`))
	}))
	t.Cleanup(srv.Close)
	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "img", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	cfg := body["run_spec"].(map[string]any)["configuration"].(map[string]any)
	if _, ok := cfg["idle_duration"]; ok {
		t.Fatalf("idle_duration unexpectedly present")
	}
}

func TestHTTPClient_Apply_SendsMaxDuration(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == getPlanPath {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"create"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","jobs":[],"run_spec":{"configuration":{"replicas":{"min":1,"max":1}}}}`))
	}))
	t.Cleanup(srv.Close)
	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "img", Port: 8080, MaxDuration: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	cfg := body["run_spec"].(map[string]any)["configuration"].(map[string]any)
	if cfg["max_duration"] != "86400s" {
		t.Fatalf("max_duration = %v", cfg["max_duration"])
	}
}

func TestHTTPClient_Apply_OmitsMaxDurationWhenZero(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == getPlanPath {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"create"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","jobs":[],"run_spec":{"configuration":{"replicas":{"min":1,"max":1}}}}`))
	}))
	t.Cleanup(srv.Close)
	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "img", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	cfg := body["run_spec"].(map[string]any)["configuration"].(map[string]any)
	if _, ok := cfg["max_duration"]; ok {
		t.Fatalf("max_duration unexpectedly present")
	}
}

const getPlanPath = "/api/project/main/runs/get_plan"
const fleetGetPath = "/api/project/main/fleets/get"

func TestHTTPClient_Get_DecodesStoredIdleDuration(t *testing.T) {
	for _, tc := range []struct {
		name, echo string
		want       time.Duration
	}{
		{"stored value", `600`, 10 * time.Minute},
		{"pre-upgrade run stores none", `null`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"abc","status":"running","jobs":[],"run_spec":{"run_name":"qwen","configuration":{"replicas":{"min":1,"max":1},"idle_duration":` + tc.echo + `}}}`))
			}))
			t.Cleanup(srv.Close)
			c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
			run, err := c.Get(context.Background(), "qwen")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if run.IdleDuration != tc.want {
				t.Fatalf("IdleDuration = %v, want %v", run.IdleDuration, tc.want)
			}
		})
	}
}

// TestHTTPClient_Apply_IsTwoStepAndNeverForces walks the MEASURED apply
// contract: get_plan first, then apply echoing the server's own normalised
// run_spec back, with force always the literal false.
func TestHTTPClient_Apply_IsTwoStepAndNeverForces(t *testing.T) {
	var sawPlan, sawApply bool
	var applyBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case getPlanPath:
			sawPlan = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_spec":{"run_name":"qwen","configuration":{"replicas":{"min":1,"max":1}}},"current_resource":null,"action":"create"}`))
		case "/api/project/main/runs/apply":
			sawApply = true
			if err := json.NewDecoder(r.Body).Decode(&applyBody); err != nil {
				t.Errorf("decode apply body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"11111111-1111-1111-1111-111111111111","submitted_at":"2026-08-27T09:00:00Z","status":"submitted","deployment_num":0,"jobs":[],"service":{"url":"/proxy/services/main/qwen/"},"run_spec":{"run_name":"qwen","configuration":{"replicas":{"min":1,"max":1}}}}`))
		default:
			t.Errorf("unexpected path %q — every run operation is POST under /api/project/{project}/runs/", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	run, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "ollama/qwen", Port: 11434})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !sawPlan || !sawApply {
		t.Fatalf("get_plan seen = %v, apply seen = %v; apply is a TWO-step flow", sawPlan, sawApply)
	}
	if force, ok := applyBody["force"]; !ok {
		t.Fatal("apply body has no `force` field: dstack requires it")
	} else if force != false {
		t.Fatalf("force = %v, want false — squall NEVER forces (AC13)", force)
	}
	if run.RunID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("RunID = %q, want dstack's own run id", run.RunID)
	}
	if run.ServiceURL != "/proxy/services/main/qwen/" {
		t.Errorf("ServiceURL = %q, want the service proxy path", run.ServiceURL)
	}
	if run.Replicas != 1 {
		t.Errorf("Replicas = %d, want 1 read from run_spec.configuration.replicas.min", run.Replicas)
	}
}

// TestHTTPClient_Apply_RoundTripsCurrentResourceVerbatim is F18's CAS:
// dstack compares the WHOLE previous Run, so anything the client drops on
// decode would corrupt the comparison and turn a legitimate flip into a
// permanent conflict.
func TestHTTPClient_Apply_RoundTripsCurrentResourceVerbatim(t *testing.T) {
	const stored = `{"id":"abc","some_field_squall_does_not_model":42,"jobs":[]}`
	var sent json.RawMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/project/main/runs/get":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(stored))
		case getPlanPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"update"}`))
		case "/api/project/main/runs/apply":
			var body struct {
				Plan struct {
					CurrentResource json.RawMessage `json:"current_resource"`
				} `json:"plan"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent = body.Plan.CurrentResource
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"abc","jobs":[],"run_spec":{"configuration":{"replicas":{"min":0,"max":0}}}}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	prev, err := c.Get(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 0, Image: "img", Port: 8080, Current: prev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(sent, &got); err != nil {
		t.Fatalf("current_resource was not sent as an object: %v (raw %q)", err, sent)
	}
	if got["some_field_squall_does_not_model"] != float64(42) {
		t.Fatalf("current_resource = %s; a field squall does not model was DROPPED, which corrupts dstack's whole-object CAS", sent)
	}
}

// TestClient_Authorize_AllMethods is the fourth load-bearing invariant: a
// reviewer's mutation commented out the c.authorize(...) call in Get, Delete
// and ListRuns and the whole suite stayed green, because the fake's
// run-management routes enforced no auth at all. This test inspects the
// wire header directly with a recording handler, so it fails on that
// mutation regardless of what the fake does or doesn't enforce.
func TestClient_Authorize_AllMethods(t *testing.T) {
	const token = "the-expected-token"
	want := "Bearer " + token
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case getPlanPath:
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"create"}`))
		case "/api/runs/list":
			_, _ = w.Write([]byte(`[]`))
		case "/api/project/main/runs/delete":
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"id":"run-x","jobs":[],"run_spec":{"run_name":"qwen","configuration":{"replicas":{"min":1,"max":1}}}}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", token, srv.Client())
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"Apply", func() error {
			_, err := c.Apply(ctx, dstack.ApplyRequest{Name: "qwen", Replicas: 1})
			return err
		}},
		{"Get", func() error {
			_, err := c.Get(ctx, "qwen")
			return err
		}},
		{"Delete", func() error {
			return c.Delete(ctx, "qwen")
		}},
		{"ListRuns", func() error {
			_, err := c.ListRuns(ctx)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAuth = ""
			if err := tt.call(); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if gotAuth != want {
				t.Fatalf("%s: Authorization header = %q, want %q", tt.name, gotAuth, want)
			}
		})
	}
}

// TestClient_Apply_Success proves the happy path: a plain Apply against the
// real fake, over real HTTP, returns the run state dstack reports.
func TestClient_Apply_Success(t *testing.T) {
	srv := httptest.NewServer(mock.NewHTTPServer(mock.New(), mock.ValidToken))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", mock.ValidToken, nil)
	run, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if run.Replicas != 1 || run.DeploymentNum != 1 || run.RunID == "" {
		t.Fatalf("Apply returned %+v, want replicas=1 deploymentNum=1 non-empty RunID", run)
	}
}

// TestClient_Apply_StaleCurrent_MapsToErrResourceChanged is the second
// load-bearing invariant: the losing side of a CAS race (F18) must surface
// as ErrResourceChanged, never retried with force (§5.2, AC13).
func TestClient_Apply_StaleCurrent_MapsToErrResourceChanged(t *testing.T) {
	srv := httptest.NewServer(mock.NewHTTPServer(mock.New(), mock.ValidToken))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", mock.ValidToken, nil)
	ctx := context.Background()

	run, err := c.Apply(ctx, dstack.ApplyRequest{Name: "qwen", Replicas: 1})
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	// Two concurrent flips computed against the same base — AC13's drill.
	stale := *run
	if _, err := c.Apply(ctx, dstack.ApplyRequest{Name: "qwen", Replicas: 0, Current: run}); err != nil {
		t.Fatalf("winning flip must succeed: %v", err)
	}
	_, err = c.Apply(ctx, dstack.ApplyRequest{Name: "qwen", Replicas: 1, Current: &stale})
	if !errors.Is(err, dstack.ErrResourceChanged) {
		t.Fatalf("F18: the loser must get ErrResourceChanged, got %v", err)
	}
}

// TestClient_Get_UnknownRun_MapsToErrNotFound covers the never-registered
// half of ErrNotFound.
func TestClient_Get_UnknownRun_MapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(mock.NewHTTPServer(mock.New(), mock.ValidToken))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", mock.ValidToken, nil)
	_, err := c.Get(context.Background(), "no-such-service")
	if !errors.Is(err, dstack.ErrNotFound) {
		t.Fatalf("Get(unknown): got %v, want ErrNotFound", err)
	}
}

// TestClient_Get_TerminalRun_ReportsStatusNotErrNotFound is F20 as MEASURED
// (§9.4): a terminated run still answers Get successfully — it is NOT
// ErrNotFound, unlike this client's earlier, invented assumption. Status is
// what tells dead apart from asleep.
func TestClient_Get_TerminalRun_ReportsStatusNotErrNotFound(t *testing.T) {
	fake := mock.New()
	srv := httptest.NewServer(mock.NewHTTPServer(fake, mock.ValidToken))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", mock.ValidToken, nil)
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fake.Terminate("qwen")

	run, err := c.Get(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("F20: Get on a terminal run must succeed (measured §9.4), got error %v", err)
	}
	if !run.IsTerminal() {
		t.Fatalf("F20: Get on a terminal run returned %+v, want IsTerminal() true", run)
	}
}

func TestClient_Get_DecodesTerminalProvisioningFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"failed-run","status":"failed","deployment_num":2,
			"status_message":"Failed to start job",
			"termination_reason":"job_failed",
			"latest_job_submission":{
				"deployment_num":2,"status":"failed",
				"termination_reason":"failed_to_start_due_to_no_capacity",
				"termination_reason_message":"{\"error\":\"insufficient_credit\"}: 429 Too Many Requests"
			},
			"jobs":[],
			"run_spec":{"run_name":"qwen","configuration":{"replicas":{"min":0,"max":0}}}
		}`))
	}))
	t.Cleanup(srv.Close)

	run, err := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client()).Get(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if run.ProvisioningFailure == nil {
		t.Fatal("ProvisioningFailure = nil, want latest terminal job failure")
	}
	if run.ProvisioningFailure.RunID != "failed-run" ||
		run.ProvisioningFailure.Reason != "failed_to_start_due_to_no_capacity" ||
		run.ProvisioningFailure.Message != `{"error":"insufficient_credit"}: 429 Too Many Requests` {
		t.Fatalf("ProvisioningFailure = %+v", run.ProvisioningFailure)
	}
}

// TestClient_Get_Success proves Get reflects live state over the wire.
// TestClient_Get_ReadsReplicasFromMin covers an asymmetric {min,max} that
// squall's own fixed-replica client never produces but a real server's
// response shape allows in general. decodeRun must read Replicas from Min,
// not Max — the two only ever coincide in this suite's other fixtures
// (squall always submits a fixed count), which would otherwise leave that
// choice unfalsified.
func TestClient_Get_ReadsReplicasFromMin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","jobs":[],"run_spec":{"run_name":"qwen","configuration":{"replicas":{"min":0,"max":3}}}}`))
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	run, err := c.Get(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.Replicas != 0 {
		t.Fatalf("Replicas = %d, want 0 read from replicas.min (not max=3)", run.Replicas)
	}
}

func TestClient_Get_Success(t *testing.T) {
	srv := httptest.NewServer(mock.NewHTTPServer(mock.New(), mock.ValidToken))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", mock.ValidToken, nil)
	ctx := context.Background()
	applied, err := c.Apply(ctx, dstack.ApplyRequest{Name: "qwen", Replicas: 1})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := c.Get(ctx, "qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RunID != applied.RunID || got.DeploymentNum != applied.DeploymentNum || got.Replicas != applied.Replicas {
		t.Fatalf("Get returned %+v, want %+v", got, applied)
	}
}

// TestClient_Delete_Success proves the happy path removes the run: a Get
// afterward must be ErrNotFound.
func TestClient_Delete_Success(t *testing.T) {
	srv := httptest.NewServer(mock.NewHTTPServer(mock.New(), mock.ValidToken))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", mock.ValidToken, nil)
	ctx := context.Background()
	if _, err := c.Apply(ctx, dstack.ApplyRequest{Name: "qwen", Replicas: 1}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// D56: real dstack refuses to delete a run that is not terminal, so the
	// happy path is stop-then-delete. Delete alone answers 400.
	if err := c.Stop(ctx, "qwen"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Delete(ctx, "qwen"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Get(ctx, "qwen"); !errors.Is(err, dstack.ErrNotFound) {
		t.Fatalf("Get after Delete: got %v, want ErrNotFound", err)
	}
}

// TestClient_Delete_AlreadyAbsent_MapsToErrNotFound documents the chosen
// idempotency contract: Delete on a run the server has never seen surfaces
// ErrNotFound rather than being swallowed into a nil "success".
func TestClient_Delete_AlreadyAbsent_MapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(mock.NewHTTPServer(mock.New(), mock.ValidToken))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", mock.ValidToken, nil)
	err := c.Delete(context.Background(), "no-such-service")
	if !errors.Is(err, dstack.ErrNotFound) {
		t.Fatalf("Delete(absent): got %v, want ErrNotFound", err)
	}
}

// TestClient_ListRuns_ReturnsAllKnownRuns backs the reconcile loop's orphan
// diff (§5.2). Unlike this client's earlier, invented assumption, a
// terminal run is NOT excluded — dstack keeps its record; Status is how a
// caller tells it apart from a live one (F20, measured §9.4).
func TestClient_ListRuns_ReturnsAllKnownRuns(t *testing.T) {
	fake := mock.New()
	srv := httptest.NewServer(mock.NewHTTPServer(fake, mock.ValidToken))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", mock.ValidToken, nil)
	ctx := context.Background()
	if _, err := c.Apply(ctx, dstack.ApplyRequest{Name: "qwen", Replicas: 1}); err != nil {
		t.Fatalf("Apply(qwen): %v", err)
	}
	if _, err := c.Apply(ctx, dstack.ApplyRequest{Name: "llama", Replicas: 0}); err != nil {
		t.Fatalf("Apply(llama): %v", err)
	}
	if _, err := c.Apply(ctx, dstack.ApplyRequest{Name: "dead", Replicas: 1}); err != nil {
		t.Fatalf("Apply(dead): %v", err)
	}
	fake.Terminate("dead")

	runs, err := c.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("ListRuns: got %d runs, want 3 (dstack keeps terminal runs too): %+v", len(runs), runs)
	}
	byName := map[string]dstack.Run{}
	for _, r := range runs {
		byName[r.Name] = r
	}
	if !byName["dead"].IsTerminal() {
		t.Fatalf("ListRuns: %+v, want the terminal run reported with a terminal Status, not excluded", byName["dead"])
	}
}

// TestClient_UnexpectedStatus_ReturnsError covers a status the fake never
// returns but a real server or intermediary might.
func TestClient_UnexpectedStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "test-token", nil)
	_, err := c.Get(context.Background(), "qwen")
	if err == nil {
		t.Fatal("Get against a 500: got nil error, want non-nil")
	}
	if errors.Is(err, dstack.ErrNotFound) || errors.Is(err, dstack.ErrResourceChanged) {
		t.Fatalf("a 500 must not be mistaken for a mapped sentinel, got %v", err)
	}
}

// TestClient_MalformedResponseBody_ReturnsError covers the decode-error path
// when the server answers 200 with a body that isn't valid JSON.
func TestClient_MalformedResponseBody_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not json"))
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "test-token", nil)
	if _, err := c.Get(context.Background(), "qwen"); err == nil {
		t.Fatal("Get with a malformed 200 body: got nil error, want non-nil")
	}
}

// TestClient_InvalidBaseURL_ReturnsBuildError covers the request-construction
// error path shared by every method: a baseURL that cannot form a valid
// request (a raw control character in the URL) must surface as an error
// rather than panic or hang.
func TestClient_InvalidBaseURL_ReturnsBuildError(t *testing.T) {
	c := dstack.NewHTTPClient("http://\x7f", "main", "test-token", nil)
	ctx := context.Background()

	if _, err := c.Apply(ctx, dstack.ApplyRequest{Name: "qwen", Replicas: 1}); err == nil {
		t.Error("Apply with an invalid base URL: got nil error")
	}
	if _, err := c.Get(ctx, "qwen"); err == nil {
		t.Error("Get with an invalid base URL: got nil error")
	}
	if err := c.Delete(ctx, "qwen"); err == nil {
		t.Error("Delete with an invalid base URL: got nil error")
	}
	if _, err := c.ListRuns(ctx); err == nil {
		t.Error("ListRuns with an invalid base URL: got nil error")
	}
}

// erroringRoundTripper always fails the request, simulating a transport
// failure (closed connection, DNS error) rather than a server-side status.
type erroringRoundTripper struct{}

func (erroringRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("simulated transport failure")
}

// TestClient_TransportError_IsNotMistakenForSentinel covers the
// c.httpClient.Do(...) err != nil branch in all four methods: a transport
// failure must be returned as an error, and MUST NOT be confused with
// ErrNotFound or ErrResourceChanged. Phase 7's sleep path treats an
// unreachable answer as "stay awake" — never as "assume idle" — so a
// network failure being misread as a mapped sentinel would be a
// wake-vs-sleep safety bug, not just a wrong error message.
func TestClient_TransportError_IsNotMistakenForSentinel(t *testing.T) {
	c := dstack.NewHTTPClient("http://example.invalid", "main", "test-token", &http.Client{Transport: erroringRoundTripper{}})
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"Apply", func() error {
			_, err := c.Apply(ctx, dstack.ApplyRequest{Name: "qwen", Replicas: 1})
			return err
		}},
		{"Get", func() error {
			_, err := c.Get(ctx, "qwen")
			return err
		}},
		{"Delete", func() error {
			return c.Delete(ctx, "qwen")
		}},
		{"ListRuns", func() error {
			_, err := c.ListRuns(ctx)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatal("got nil error, want a transport error")
			}
			if errors.Is(err, dstack.ErrNotFound) || errors.Is(err, dstack.ErrResourceChanged) {
				t.Fatalf("a transport failure must never be mistaken for a mapped sentinel, got %v", err)
			}
		})
	}
}

// TestClient_ImplementsInterface is a compile-time-flavored check that
// HTTPClient satisfies Client — the interface the controller depends on.
func TestClient_ImplementsInterface(t *testing.T) {
	var _ dstack.Client = dstack.NewHTTPClient("http://example.invalid", "main", "token", nil)
}

// TestHTTPClient_BackendConfigured walks D67's MEASURED contract: config_info
// returns 200 when the backend is configured, 400 when it is not, and any
// other status is OUR ignorance, not a verdict on the backend.
func TestHTTPClient_BackendConfigured(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    bool
		wantErr bool
	}{
		{name: "200 is configured", status: http.StatusOK, body: `{}`, want: true},
		{name: "400 is not configured (D67)", status: http.StatusBadRequest, body: `{"detail":[{"msg":"no such backend","code":"error"}]}`, want: false},
		{name: "500 is not a verdict, it's an unknown", status: http.StatusInternalServerError, body: `boom`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if want := "/api/project/main/backends/vastai/config_info"; r.URL.Path != want {
					t.Errorf("path = %q, want %q", r.URL.Path, want)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
			got, err := c.BackendConfigured(context.Background(), "vastai")
			if tc.wantErr {
				if err == nil {
					t.Fatal("BackendConfigured: got nil error, want one")
				}
				return
			}
			if err != nil {
				t.Fatalf("BackendConfigured: %v", err)
			}
			if got != tc.want {
				t.Fatalf("BackendConfigured = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHTTPClient_HasFleetFor walks D58's MEASURED contract: fleets/list
// returns an array of fleets; only "active" ones count, and a fleet with no
// backends listed in its configuration admits any backend.
func TestHTTPClient_HasFleetFor(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		backend string
		want    bool
	}{
		{
			name:    "active fleet with no backend restriction admits anything",
			body:    `[{"status":"active","spec":{"configuration":{"backends":[]}}}]`,
			backend: "vastai",
			want:    true,
		},
		{
			name:    "active fleet scoped to a different backend does not admit vastai",
			body:    `[{"status":"active","spec":{"configuration":{"backends":["aws"]}}}]`,
			backend: "vastai",
			want:    false,
		},
		{
			name:    "active fleet scoped to the requested backend admits it",
			body:    `[{"status":"active","spec":{"configuration":{"backends":["vastai"]}}}]`,
			backend: "vastai",
			want:    true,
		},
		{
			name:    "terminated fleet does not count even if it would otherwise admit",
			body:    `[{"status":"terminated","spec":{"configuration":{"backends":[]}}}]`,
			backend: "vastai",
			want:    false,
		},
		{
			name:    "empty fleet list",
			body:    `[]`,
			backend: "vastai",
			want:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if want := "/api/project/main/fleets/list"; r.URL.Path != want {
					t.Errorf("path = %q, want %q", r.URL.Path, want)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
			got, err := c.HasFleetFor(context.Background(), tc.backend)
			if err != nil {
				t.Fatalf("HasFleetFor: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasFleetFor(%q) = %v, want %v", tc.backend, got, tc.want)
			}
		})
	}
}

// TestHTTPClient_HasFleetFor_MalformedBody proves a decode failure surfaces
// as an error (fail-open happens at the preflight layer, not by silently
// treating "can't parse this" as "no fleet").
func TestHTTPClient_HasFleetFor_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if _, err := c.HasFleetFor(context.Background(), "vastai"); err == nil {
		t.Fatal("HasFleetFor: got nil error for a malformed body, want one")
	}
}

// TestFleetName is a pure function: two callers naming the same backend must
// land on the identical fleet (docs/references/dstack-real-api.md §9.8).
func TestFleetName(t *testing.T) {
	if got, want := dstack.FleetName("vastai"), "squall-auto-vastai"; got != want {
		t.Fatalf("FleetName(%q) = %q, want %q", "vastai", got, want)
	}
	first := dstack.FleetName("vastai")
	if second := dstack.FleetName("vastai"); first != second {
		t.Fatalf("FleetName is not deterministic: %q then %q", first, second)
	}
}

// TestHTTPClient_EnsureFleet_AlreadyExists proves the create-only, level-
// triggered contract (LIVE-7/D83): if fleets/get already answers the fleet,
// EnsureFleet must not touch get_plan or apply at all.
func TestHTTPClient_EnsureFleet_AlreadyExists(t *testing.T) {
	var sawGet bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fleetGetPath:
			sawGet = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"squall-auto-vastai","status":"active"}`))
		default:
			t.Errorf("unexpected path %q — an existing fleet must not be re-planned or re-applied", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if err := c.EnsureFleet(context.Background(), dstack.FleetSpec{
		Name:     "squall-auto-vastai",
		Backends: []string{"vastai"},
	}); err != nil {
		t.Fatalf("EnsureFleet: %v", err)
	}
	if !sawGet {
		t.Fatal("fleets/get was never called")
	}
}

// TestHTTPClient_EnsureFleet_CreatesWhenMissing walks the create path: a
// fleets/get resource_not_exists (D67-style body, measured live against a
// running dstack server) must fall through to get_plan then apply, echoing
// the server's effective_spec back and never setting force.
func TestHTTPClient_EnsureFleet_CreatesWhenMissing(t *testing.T) {
	var sawGet, sawPlan, sawApply bool
	var applyBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fleetGetPath:
			sawGet = true
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":[{"msg":"Resource not found","code":"resource_not_exists"}]}`))
		case "/api/project/main/fleets/get_plan":
			sawPlan = true
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode get_plan body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"spec":{"configuration":{"type":"fleet","name":"squall-auto-vastai","nodes":"0..","backends":["vastai"]}},"effective_spec":{"configuration":{"type":"fleet","name":"squall-auto-vastai","nodes":"0..2","backends":["vastai"]}}}`))
		case "/api/project/main/fleets/apply":
			sawApply = true
			if err := json.NewDecoder(r.Body).Decode(&applyBody); err != nil {
				t.Errorf("decode apply body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"squall-auto-vastai","status":"active"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if err := c.EnsureFleet(context.Background(), dstack.FleetSpec{
		Name:     "squall-auto-vastai",
		Backends: []string{"vastai"},
	}); err != nil {
		t.Fatalf("EnsureFleet: %v", err)
	}
	if !sawGet || !sawPlan || !sawApply {
		t.Fatalf("saw get=%v plan=%v apply=%v, want all three", sawGet, sawPlan, sawApply)
	}
	if force, ok := applyBody["force"].(bool); !ok || force {
		t.Fatalf("apply body force = %v, want literal false — squall never sends force (AC13)", applyBody["force"])
	}
	plan, _ := applyBody["plan"].(map[string]any)
	spec, _ := plan["spec"].(map[string]any)
	config, _ := spec["configuration"].(map[string]any)
	if nodes, _ := config["nodes"].(string); nodes != "0..2" {
		t.Fatalf("apply echoed nodes = %q, want the server's own effective_spec (\"0..2\"), not the request's", nodes)
	}
}

// TestHTTPClient_EnsureFleet_GetErrorPropagates proves a transport failure
// on fleets/get is not mistaken for "does not exist": only ErrNotFound falls
// through to create.
func TestHTTPClient_EnsureFleet_GetErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fleetGetPath:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`boom`))
		default:
			t.Errorf("unexpected path %q — a non-not-found error must not fall through to create", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if err := c.EnsureFleet(context.Background(), dstack.FleetSpec{
		Name:     "squall-auto-vastai",
		Backends: []string{"vastai"},
	}); err == nil {
		t.Fatal("EnsureFleet: got nil error, want the fleets/get failure propagated")
	}
}

// SPDX-License-Identifier: Apache-2.0

// promtool is not available in this environment (no network-installed
// binary), so this test is the substitute check for rules.yaml: it parses
// the PrometheusRule and asserts every rule group's alerts are
// well-formed (non-empty expr, alert, for, and the "observed > declared"
// shape spec §10/AC19 calls for), rather than a full PromQL semantic
// validation.
package prometheus

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type prometheusRule struct {
	Kind string `json:"kind"`
	Spec struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Alert string `json:"alert"`
				Expr  string `json:"expr"`
				For   string `json:"for"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"spec"`
}

func TestRules_ObservedExceedsDeclaredShape(t *testing.T) {
	raw, err := os.ReadFile("rules.yaml")
	if err != nil {
		t.Fatalf("read rules.yaml: %v", err)
	}

	var rule prometheusRule
	if err := yaml.Unmarshal(raw, &rule); err != nil {
		t.Fatalf("unmarshal rules.yaml: %v", err)
	}

	if rule.Kind != "PrometheusRule" {
		t.Fatalf("kind = %q, want PrometheusRule", rule.Kind)
	}
	if len(rule.Spec.Groups) != 1 {
		t.Fatalf("groups = %d, want exactly 1", len(rule.Spec.Groups))
	}

	rules := rule.Spec.Groups[0].Rules
	if len(rules) != 8 {
		t.Fatalf("rules = %d, want exactly 8 (age, price, provisioning loop, uncontrolled, "+
			"capacity-while-not-ready, idle-but-awake, controller absent, hard stop)", len(rules))
	}

	for _, r := range rules {
		if r.Alert == "" || r.For == "" {
			t.Errorf("rule %+v: alert and for must both be set", r)
		}
		if !strings.Contains(r.Expr, " > ") {
			t.Errorf("rule %q: expr %q does not follow the observed > declared shape", r.Alert, r.Expr)
		}
	}

	// Confirm the two AC19 pairs are the ones actually wired, by name.
	wantExprs := map[string]bool{
		"squall_model_age_seconds > squall_model_max_lifetime_seconds":  false,
		"squall_model_price_per_hour > squall_model_max_price_per_hour": false,
		// spec §5.2 makes provisioningTimeout destroy each failed wake, but a
		// caller that keeps retrying re-arms it forever: every attempt rents a
		// machine until the deadline. Nothing in the controller bounds that, so
		// it has to be visible.
		"increase(squall_model_provisioning_attempts_total[1h]) > 6": false,
		// the proxy stopped reporting: squall is flying blind on a live replica
		"squall_model_uncontrolled_seconds > 0.5 * squall_model_uncontrolled_timeout_seconds": false,
		// capacity is up while the Model is not Ready — paying for nothing
		"squall_model_run_active > max by (namespace, name) (squall_model_phase{phase=\"Ready\"})": false,
		"squall_model_idle_seconds > 3 * squall_model_scale_down_delay_seconds":                    false,
		"absent(controller_runtime_active_workers{controller=\"squall-model\"}) > 0":               false,
		"increase(squall_model_provisioning_outcomes_total{reason=\"hard_stop\"}[1h]) > 0":         false,
	}
	for _, r := range rules {
		if _, ok := wantExprs[r.Expr]; ok {
			wantExprs[r.Expr] = true
		}
	}
	for expr, found := range wantExprs {
		if !found {
			t.Errorf("expected an alert with expr %q, not found", expr)
		}
	}
}

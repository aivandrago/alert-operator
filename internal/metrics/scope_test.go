/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
)

func f(t alertsv1alpha1.FindingType) alertsv1alpha1.Finding {
	return alertsv1alpha1.Finding{Type: t, Severity: alertsv1alpha1.SeverityOf(t), Message: "free text never becomes a label"}
}

func fixture() ScopeSnapshot {
	return ScopeSnapshot{
		EffectiveWindow: 15 * 24 * time.Hour,
		LastSync:        time.Unix(1763460000, 0),
		Rules: []RuleSample{
			{Namespace: "team-payments", Team: "payments", Alert: "PaymentsHighLatency", Loaded: true, Measured: true,
				Activations: 212, FiringFraction: 0.06,
				Findings: []alertsv1alpha1.Finding{f(alertsv1alpha1.FindingNoisy), f(alertsv1alpha1.FindingNoRunbook)}},
			{Namespace: "team-payments", Team: "payments", Alert: "PaymentsDown", Loaded: true, Measured: true},
			{Namespace: "team-web", Team: "<none>", Alert: "WebDown", Loaded: false,
				Findings: []alertsv1alpha1.Finding{f(alertsv1alpha1.FindingNotLoaded), f(alertsv1alpha1.FindingUnowned)}},
		},
	}
}

func TestGolden(t *testing.T) {
	c := NewCollector()
	c.Set("main", fixture())
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	golden := "testdata/metrics.golden"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		mfs, _ := reg.Gather()
		var b strings.Builder
		for _, mf := range mfs {
			_ = writeText(&b, mf)
		}
		_ = os.WriteFile(golden, []byte(b.String()), 0o600)
	}
	want, err := os.Open(golden)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = want.Close() }()
	if err := testutil.GatherAndCompare(reg, want); err != nil {
		t.Error(err)
	}
}

func TestNoStaleSeries(t *testing.T) {
	c := NewCollector()
	c.Set("main", fixture())
	s := fixture()
	s.Rules = s.Rules[:1]
	s.Rules[0].Findings = s.Rules[0].Findings[:1] // NoRunbook fixed
	c.Set("main", s)
	if n := testutil.CollectAndCount(c, "hs_alert_rule_findings"); n != 1 {
		t.Errorf("findings series after the fix: %d, want 1", n)
	}
	if n := testutil.CollectAndCount(c, "hs_alert_rules"); n != 1 {
		t.Errorf("rules series: %d", n)
	}
	c.Delete("main")
	if n := testutil.CollectAndCount(c); n != 0 {
		t.Errorf("series after the scope was deleted: %d", n)
	}
}

func TestCardinalityGuard(t *testing.T) {
	c := NewCollector()
	s := fixture()
	// The same finding twice on one rule is one series.
	s.Rules[0].Findings = append(s.Rules[0].Findings, f(alertsv1alpha1.FindingNoisy))
	c.Set("main", s)
	if n := testutil.CollectAndCount(c, "hs_alert_rule_findings"); n != 4 {
		t.Errorf("findings series: %d, want 4", n)
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "expr" || l.GetName() == "message" || strings.Contains(l.GetValue(), "free text") {
					t.Errorf("%s carries a free-text label %s=%q", mf.GetName(), l.GetName(), l.GetValue())
				}
			}
		}
	}
}

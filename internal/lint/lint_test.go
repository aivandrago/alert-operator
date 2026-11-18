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

package lint

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

var good = map[string]string{"severity": "warning", "team": "payments"}
var runbook = map[string]string{"runbook_url": "https://runbooks/x"}

func object(labels map[string]string, rules ...monitoringv1.Rule) *monitoringv1.PrometheusRule {
	return &monitoringv1.PrometheusRule{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "api", Labels: labels},
		Spec:       monitoringv1.PrometheusRuleSpec{Groups: []monitoringv1.RuleGroup{{Name: "api", Rules: rules}}},
	}
}

func alert(name, expr string, labels, ann map[string]string) monitoringv1.Rule {
	return monitoringv1.Rule{Alert: name, Expr: intstr.FromString(expr), Labels: labels, Annotations: ann}
}

func checks(r Result) string {
	parts := make([]string, 0, len(r.Problems))
	for _, p := range r.Problems {
		parts = append(parts, p.Rule+":"+string(p.Check))
	}
	return strings.Join(parts, ",")
}

func defaults() alertsv1alpha1.Settings { return (&alertsv1alpha1.AlertScopeSpec{}).Settings() }

type fakeQuerier struct {
	live  []string
	err   error
	asked int
}

func (f *fakeQuerier) Query(_ context.Context, q string, _ time.Time) ([]promapi.Sample, error) {
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	for _, l := range f.live {
		if strings.Contains(q, l) {
			return []promapi.Sample{{Value: 1}}, nil
		}
	}
	return nil, nil
}

func TestChecks(t *testing.T) {
	prom := &monitoringv1.Prometheus{ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "k8s"}}
	prom.Spec.RuleSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"release": "kps"}}
	prom.Spec.RuleNamespaceSelector = &metav1.LabelSelector{}
	kps := map[string]string{"release": "kps"}
	cases := []struct {
		name     string
		obj      *monitoringv1.PrometheusRule
		prom     *monitoringv1.Prometheus
		querier  *fakeQuerier
		want     string
		blocking bool
	}{
		{"clean", object(kps, alert("A", "up == 0", good, runbook)), prom, &fakeQuerier{live: []string{"up"}}, "", false},
		{"parse error", object(kps, alert("A", "rate(up[5m]", good, runbook)), prom, nil, "A:LoadError", true},
		{"recording rule parse error", object(kps, monitoringv1.Rule{Record: "job:up:sum", Expr: intstr.FromString("sum(")}), nil, nil,
			"job:up:sum:LoadError", true},
		{"recording rule fine, no ownership checks", object(kps, monitoringv1.Rule{Record: "job:up:sum", Expr: intstr.FromString("sum(up)")}),
			nil, nil, "", false},
		{"not loaded", object(nil, alert("A", "up == 0", good, runbook)), prom, nil, ":NotLoaded", true},
		{"hygiene", object(kps, alert("A", "up == 0", nil, nil)), prom, nil, "A:Unowned,A:NoRunbook,A:NoSeverity", true},
		{"dead query only warns", object(kps, alert("A", "renamed_metric > 0", good, runbook)), prom, &fakeQuerier{}, "A:DeadQuery", false},
		{"several problems", object(nil, alert("A", "up == 0", map[string]string{"team": "x"}, runbook), alert("B", "sum(", good, runbook)),
			prom, nil, ":NotLoaded,A:NoSeverity,B:LoadError", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Rule: tc.obj, Settings: defaults(), Prometheus: tc.prom, Now: time.Now()}
			if tc.querier != nil {
				in.Querier = tc.querier
			}
			r := Check(context.Background(), in)
			if got := checks(r); got != tc.want {
				t.Errorf("problems %q, want %q", got, tc.want)
			}
			if r.Blocking() != tc.blocking {
				t.Errorf("blocking %v, want %v", r.Blocking(), tc.blocking)
			}
		})
	}
}

func TestTeamFromNamespaceLabel(t *testing.T) {
	obj := object(nil, alert("A", "up == 0", map[string]string{"severity": "info"}, runbook))
	r := Check(context.Background(), Input{Rule: obj, Settings: defaults(), NamespaceLabels: map[string]string{"hs/team": "a"}})
	if checks(r) != "" {
		t.Errorf("namespace label should own the rule: %s", checks(r))
	}
}

func TestSkipped(t *testing.T) {
	obj := object(nil, alert("A", "up == 0", good, runbook), alert("B", "up == 1", good, runbook))
	r := Check(context.Background(), Input{Rule: obj, Settings: defaults()})
	if len(r.Skipped) != 2 || !strings.HasPrefix(r.Skipped[0], "NotLoaded") || !strings.HasPrefix(r.Skipped[1], "DeadQuery") {
		t.Errorf("skipped: %v", r.Skipped)
	}
	// A Prometheus that times out: DeadQuery skipped once, after the first attempt, never a problem.
	q := &fakeQuerier{err: context.DeadlineExceeded}
	r = Check(context.Background(), Input{Rule: obj, Settings: defaults(), Querier: q})
	if len(r.Problems) != 0 || q.asked != 1 || len(r.Skipped) != 2 || !strings.Contains(r.Skipped[1], "did not answer in time") {
		t.Errorf("slow Prometheus: %+v asked %d", r, q.asked)
	}
	q = &fakeQuerier{err: errors.New("boom")}
	if r = Check(context.Background(), Input{Rule: obj, Settings: defaults(), Querier: q}); !strings.Contains(r.Skipped[1], "boom") {
		t.Errorf("error: %v", r.Skipped)
	}
}

func TestProblemString(t *testing.T) {
	p := Problem{Group: "g", Rule: "A", Check: alertsv1alpha1.FindingNoRunbook, Message: "annotation runbook_url is missing"}
	if p.String() != "g/A: NoRunbook: annotation runbook_url is missing" {
		t.Error(p.String())
	}
}

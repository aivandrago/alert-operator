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

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/inventory"
)

func fnd(t alertsv1alpha1.FindingType, msg string) alertsv1alpha1.Finding {
	return alertsv1alpha1.Finding{Type: t, Severity: alertsv1alpha1.SeverityOf(t), Message: msg}
}

func TestRuleStatusCarriesOverAnalysis(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	evaluated := metav1.NewTime(now.Add(-10 * time.Minute))
	prev := alertsv1alpha1.AlertRuleStatus{
		FirstSeen: &metav1.Time{Time: now.Add(-48 * time.Hour)}, LastEvaluated: &evaluated,
		Quality: alertsv1alpha1.RuleQuality{Activations: 80},
		Findings: []alertsv1alpha1.Finding{fnd(alertsv1alpha1.FindingNoisy, "80"), fnd(alertsv1alpha1.FindingNoRunbook, "x"),
			fnd(alertsv1alpha1.FindingAlwaysSilenced, "90%")},
	}
	e := &inventory.Entry{Identity: "abc", Loaded: true, Team: "a",
		Findings: []alertsv1alpha1.Finding{fnd(alertsv1alpha1.FindingNoRunbook, "x")}}

	// No analysis this sync: the analysis findings and the quality stay.
	st := ruleStatus(prev, e, nil, "30d", now)
	if types_(st.Findings) != "Noisy,AlwaysSilenced,NoRunbook" || st.Quality.Activations != 80 ||
		!st.LastEvaluated.Equal(&evaluated) || !st.FirstSeen.Equal(prev.FirstSeen) {
		t.Errorf("carry over: %s %+v", types_(st.Findings), st)
	}
	if again := ruleStatus(st, e, nil, "30d", now); statusChanged(st, again, now) {
		t.Error("nothing changed, but a write is due")
	}

	// Alertmanager down: quality measured fresh, AlwaysSilenced kept, Noisy gone.
	a := &analysis{measured: true, quality: alertsv1alpha1.RuleQuality{Activations: 3},
		kept: []alertsv1alpha1.FindingType{alertsv1alpha1.FindingAlwaysSilenced}}
	st = ruleStatus(prev, e, a, "30d", now)
	if types_(st.Findings) != "AlwaysSilenced,NoRunbook" || st.Quality.Activations != 3 || !st.LastEvaluated.Time.Equal(now) {
		t.Errorf("partial analysis: %s %+v", types_(st.Findings), st)
	}

	// Not loaded any more: no analysis findings.
	e2 := *e
	e2.Loaded = false
	st = ruleStatus(prev, &e2, nil, "30d", now)
	if types_(st.Findings) != "NoRunbook" {
		t.Errorf("not loaded: %s", types_(st.Findings))
	}
	// Only info findings: Healthy.
	if c := st.Conditions[0]; c.Status != metav1.ConditionTrue {
		t.Errorf("info findings only should be Healthy: %+v", c)
	}
}

func TestStatusHeartbeat(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-2 * time.Hour))
	recent := metav1.NewTime(now.Add(-10 * time.Minute))
	fresh := metav1.NewTime(now)
	if !statusChanged(alertsv1alpha1.AlertRuleStatus{LastEvaluated: &old}, alertsv1alpha1.AlertRuleStatus{LastEvaluated: &fresh}, now) {
		t.Error("an hour-old lastEvaluated must be refreshed")
	}
	if statusChanged(alertsv1alpha1.AlertRuleStatus{LastEvaluated: &recent}, alertsv1alpha1.AlertRuleStatus{LastEvaluated: &fresh}, now) {
		t.Error("a recent lastEvaluated alone is no reason to write")
	}
	if !statusChanged(alertsv1alpha1.AlertRuleStatus{Team: "a"}, alertsv1alpha1.AlertRuleStatus{Team: "b"}, now) {
		t.Error("a changed team must be written")
	}
}

func TestTransitions(t *testing.T) {
	gained, lost := transitions(
		[]alertsv1alpha1.Finding{fnd(alertsv1alpha1.FindingNoisy, "a"), fnd(alertsv1alpha1.FindingNoRunbook, "")},
		[]alertsv1alpha1.Finding{fnd(alertsv1alpha1.FindingNoisy, "b"), fnd(alertsv1alpha1.FindingFlapping, "")})
	if len(gained) != 1 || gained[0].Type != alertsv1alpha1.FindingFlapping || len(lost) != 1 ||
		lost[0].Type != alertsv1alpha1.FindingNoRunbook {
		t.Errorf("gained %v lost %v", gained, lost)
	}
}

func TestScopeOfMirror(t *testing.T) {
	mirror := &alertsv1alpha1.AlertRule{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		alertsv1alpha1.LabelManagedBy: alertsv1alpha1.ManagedByValue, alertsv1alpha1.LabelScope: "main"}}}
	if reqs := scopeOfMirror(context.Background(), mirror); len(reqs) != 1 || reqs[0].Name != "main" {
		t.Errorf("own mirror: %v", reqs)
	}
	if reqs := scopeOfMirror(context.Background(), &alertsv1alpha1.AlertRule{}); len(reqs) != 0 {
		t.Errorf("foreign object: %v", reqs)
	}
}

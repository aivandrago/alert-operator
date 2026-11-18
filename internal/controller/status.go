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
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/inventory"
)

// StatusHeartbeat: an AlertRule's status is rewritten when something in it changed, or, when
// nothing did, once its lastEvaluated is this old. Without changes there is no write every
// resync, and lastEvaluated still shows a verdict is current to within this much.
const StatusHeartbeat = time.Hour

// parseErrorPrefix marks a LoadError that the DeadQuery check found (the expression does not
// parse) rather than Prometheus.
const parseErrorPrefix = "the expression does not parse"

// analysisFinding reports whether a finding comes from the analysis of Prometheus' history,
// which runs once per resync interval, rather than from the inventory, which runs every sync.
func analysisFinding(f alertsv1alpha1.Finding) bool {
	switch f.Type {
	case alertsv1alpha1.FindingDeadQuery, alertsv1alpha1.FindingNoisy, alertsv1alpha1.FindingFlapping,
		alertsv1alpha1.FindingNeverFired, alertsv1alpha1.FindingAlwaysSilenced:
		return true
	case alertsv1alpha1.FindingLoadError:
		return strings.HasPrefix(f.Message, parseErrorPrefix)
	}
	return false
}

// analysis is the result of analysing one rule in this sync.
type analysis struct {
	// measured: the quality was measured (both queries answered).
	measured bool
	quality  alertsv1alpha1.RuleQuality
	// complete: every check ran, so findings replace the previous analysis findings. When
	// false, the previous ones of the checks that did not run are kept.
	complete bool
	findings []alertsv1alpha1.Finding
	// kept lists finding types whose previous verdict stands because their check could not
	// run this time (budget, an unreachable Alertmanager).
	kept []alertsv1alpha1.FindingType
}

// ruleStatus computes the AlertRule status of an entry. prev is the current status (zero for a
// new mirror), a the analysis of this sync (nil when no analysis ran for it).
func ruleStatus(prev alertsv1alpha1.AlertRuleStatus, e *inventory.Entry, a *analysis,
	window alertsv1alpha1.Duration, now time.Time) alertsv1alpha1.AlertRuleStatus {
	st := alertsv1alpha1.AlertRuleStatus{
		Identity:        e.Identity,
		Loaded:          e.Loaded,
		NotLoadedReason: e.NotLoadedReason,
		Team:            e.Team,
		Window:          prev.Window,
		FirstSeen:       prev.FirstSeen,
		LastEvaluated:   prev.LastEvaluated,
		Quality:         prev.Quality,
		Conditions:      slices.Clone(prev.Conditions),
	}
	if st.FirstSeen == nil {
		st.FirstSeen = &metav1.Time{Time: now}
	}
	findings := slices.Clone(e.Findings)
	if e.Loaded {
		if a == nil {
			for _, f := range prev.Findings {
				if analysisFinding(f) {
					findings = append(findings, f)
				}
			}
		} else {
			findings = append(findings, a.findings...)
			if !a.complete {
				for _, f := range prev.Findings {
					if slices.Contains(a.kept, f.Type) && !hasType(findings, f.Type) {
						findings = append(findings, f)
					}
				}
			}
			if a.measured {
				st.Quality = a.quality
				st.Window = window
				st.LastEvaluated = &metav1.Time{Time: now}
			}
		}
	}
	findings = dedupe(findings)
	inventory.SortFindings(findings)
	st.Findings = findings
	types := make([]string, 0, len(findings))
	for _, f := range findings {
		types = append(types, string(f.Type))
	}
	st.FindingsSummary = strings.Join(types, ",")

	cond := metav1.Condition{Type: alertsv1alpha1.ConditionHealthy, Status: metav1.ConditionTrue,
		Reason: "NoProblems", Message: "no critical or warning finding"}
	for _, f := range findings {
		if f.Severity != alertsv1alpha1.SeverityInfo {
			cond = metav1.Condition{Type: alertsv1alpha1.ConditionHealthy, Status: metav1.ConditionFalse,
				Reason: string(f.Type), Message: f.Message}
			break
		}
	}
	meta.SetStatusCondition(&st.Conditions, cond)
	return st
}

func hasType(fs []alertsv1alpha1.Finding, t alertsv1alpha1.FindingType) bool {
	return slices.ContainsFunc(fs, func(f alertsv1alpha1.Finding) bool { return f.Type == t })
}

func dedupe(fs []alertsv1alpha1.Finding) []alertsv1alpha1.Finding {
	out := fs[:0]
	seen := map[alertsv1alpha1.FindingType]bool{}
	for _, f := range fs {
		if !seen[f.Type] {
			seen[f.Type] = true
			out = append(out, f)
		}
	}
	return out
}

// statusChanged reports whether the new status must be written: anything but lastEvaluated
// differs, or lastEvaluated moved and the written one is older than StatusHeartbeat.
func statusChanged(prev, next alertsv1alpha1.AlertRuleStatus, now time.Time) bool {
	a, b := prev.DeepCopy(), next.DeepCopy()
	a.LastEvaluated, b.LastEvaluated = nil, nil
	if !equality.Semantic.DeepEqual(a, b) {
		return true
	}
	if next.LastEvaluated == nil {
		return false
	}
	return prev.LastEvaluated == nil || now.Sub(prev.LastEvaluated.Time) >= StatusHeartbeat
}

// transitions returns the finding types gained and lost between two statuses.
func transitions(prev, next []alertsv1alpha1.Finding) (gained, lost []alertsv1alpha1.Finding) {
	for _, f := range next {
		if !hasType(prev, f.Type) {
			gained = append(gained, f)
		}
	}
	for _, f := range prev {
		if !hasType(next, f.Type) {
			lost = append(lost, f)
		}
	}
	return gained, lost
}

// aggregate computes the counts of the scope status from its rules' statuses.
func aggregate(statuses []alertsv1alpha1.AlertRuleStatus) (alertsv1alpha1.RuleCounts, map[alertsv1alpha1.FindingType]int32,
	int32, []alertsv1alpha1.TeamSummary) {
	var counts alertsv1alpha1.RuleCounts
	byType := map[alertsv1alpha1.FindingType]int32{}
	var total int32
	teams := map[string]*alertsv1alpha1.TeamSummary{}
	for _, st := range statuses {
		counts.Total++
		if st.Loaded {
			counts.Loaded++
		} else {
			counts.NotLoaded++
		}
		t := teams[st.Team]
		if t == nil {
			t = &alertsv1alpha1.TeamSummary{Team: st.Team}
			teams[st.Team] = t
		}
		t.Rules++
		for _, f := range st.Findings {
			byType[f.Type]++
			total++
			t.Findings++
		}
	}
	out := make([]alertsv1alpha1.TeamSummary, 0, len(teams))
	for _, t := range teams {
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b alertsv1alpha1.TeamSummary) int { return strings.Compare(a.Team, b.Team) })
	if len(byType) == 0 {
		byType = nil
	}
	return counts, byType, total, out
}

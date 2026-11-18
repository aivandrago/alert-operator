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

package inventory

import (
	"fmt"
	"slices"
	"strings"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
)

func metav1LabelSelector(s *metav1.LabelSelector) (labels.Selector, error) {
	return metav1.LabelSelectorAsSelector(s)
}

// ResolveTeam returns the first team the sources name (design §4.1 ownership.teamFrom), and
// which source named it. obj is nil for a rule loaded from a plain file; then only rule labels
// can name a team.
func ResolveTeam(sources []alertsv1alpha1.TeamSource, ruleLabels map[string]string,
	obj *monitoringv1.PrometheusRule, namespaces map[string]map[string]string) (team, from string) {
	for _, s := range sources {
		switch {
		case s.RuleLabel != "":
			if v := strings.TrimSpace(ruleLabels[s.RuleLabel]); v != "" {
				return v, "rule label " + s.RuleLabel
			}
		case s.ObjectLabel != "" && obj != nil:
			if v := strings.TrimSpace(obj.Labels[s.ObjectLabel]); v != "" {
				return v, "PrometheusRule label " + s.ObjectLabel
			}
		case s.NamespaceLabel != "" && obj != nil:
			if v := strings.TrimSpace(namespaces[obj.Namespace][s.NamespaceLabel]); v != "" {
				return v, "namespace label " + s.NamespaceLabel
			}
		}
	}
	return "", ""
}

// applyOwnership sets the entry's team and adds Unowned, NoRunbook and NoSeverity.
func applyOwnership(e *Entry, obj *monitoringv1.PrometheusRule, namespaces map[string]map[string]string,
	s alertsv1alpha1.Settings) {
	team, _ := ResolveTeam(s.TeamFrom, e.Labels, obj, namespaces)
	if team == "" {
		e.Team = alertsv1alpha1.NoTeam
		e.Findings = append(e.Findings, finding(alertsv1alpha1.FindingUnowned,
			"no team from "+describeSources(s.TeamFrom)))
	} else {
		e.Team = team
	}
	if s.RequireRunbook && strings.TrimSpace(e.Annotations[alertsv1alpha1.AnnotationRunbook]) == "" {
		e.Findings = append(e.Findings, finding(alertsv1alpha1.FindingNoRunbook,
			"annotation runbook_url is missing"))
	}
	sev, has := e.Labels[alertsv1alpha1.LabelSeverityOnRule]
	switch {
	case !has || strings.TrimSpace(sev) == "":
		e.Findings = append(e.Findings, finding(alertsv1alpha1.FindingNoSeverity,
			"label severity is missing; allowed: "+strings.Join(s.RequireSeverity, ", ")))
	case !slices.Contains(s.RequireSeverity, sev):
		e.Findings = append(e.Findings, finding(alertsv1alpha1.FindingNoSeverity,
			fmt.Sprintf("severity %q is not one of %s", sev, strings.Join(s.RequireSeverity, ", "))))
	}
}

func describeSources(sources []alertsv1alpha1.TeamSource) string {
	parts := make([]string, 0, len(sources))
	for _, s := range sources {
		switch {
		case s.RuleLabel != "":
			parts = append(parts, "rule label "+s.RuleLabel)
		case s.ObjectLabel != "":
			parts = append(parts, "PrometheusRule label "+s.ObjectLabel)
		case s.NamespaceLabel != "":
			parts = append(parts, "namespace label "+s.NamespaceLabel)
		}
	}
	return strings.Join(parts, ", ")
}

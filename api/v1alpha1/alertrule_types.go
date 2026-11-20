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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Labels the operator puts on every AlertRule it writes. It updates or deletes only objects
// that carry both.
const (
	LabelManagedBy      = "alerts.hypersurgery.dev/managed-by"
	LabelScope          = "alerts.hypersurgery.dev/scope"
	LabelTeam           = "hs/team"
	ManagedByValue      = "alert-operator"
	ConditionReady      = "Ready"
	ConditionHealthy    = "Healthy"
	NoTeam              = "<none>"
	SourceKindRule      = "PrometheusRule"
	SourceKindFile      = "File"
	AnnotationRunbook   = "runbook_url"
	LabelSeverityOnRule = "severity"
)

// FindingType is a kind of problem with an alerting rule.
// +kubebuilder:validation:Enum=NotLoaded;LoadError;DeadQuery;NeverFired;Noisy;Flapping;AlwaysSilenced;Unowned;NoRunbook;NoSeverity
type FindingType string

// The findings of v0.1 (docs/findings.md).
const (
	FindingNotLoaded      FindingType = "NotLoaded"
	FindingLoadError      FindingType = "LoadError"
	FindingDeadQuery      FindingType = "DeadQuery"
	FindingNeverFired     FindingType = "NeverFired"
	FindingNoisy          FindingType = "Noisy"
	FindingFlapping       FindingType = "Flapping"
	FindingAlwaysSilenced FindingType = "AlwaysSilenced"
	FindingUnowned        FindingType = "Unowned"
	FindingNoRunbook      FindingType = "NoRunbook"
	FindingNoSeverity     FindingType = "NoSeverity"
)

// FindingTypes lists every finding type, in the order they are reported.
var FindingTypes = []FindingType{
	FindingNotLoaded, FindingLoadError, FindingDeadQuery, FindingNeverFired, FindingNoisy,
	FindingFlapping, FindingAlwaysSilenced, FindingUnowned, FindingNoRunbook, FindingNoSeverity,
}

// Severity of a finding.
// +kubebuilder:validation:Enum=critical;warning;info
type Severity string

// Finding severities, most severe first.
const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// SeverityOf returns the severity of a finding type (design §4.3).
func SeverityOf(t FindingType) Severity {
	switch t {
	case FindingNotLoaded, FindingLoadError:
		return SeverityCritical
	case FindingNeverFired, FindingNoRunbook, FindingNoSeverity:
		return SeverityInfo
	default:
		return SeverityWarning
	}
}

// RuleSource says where an alerting rule is defined.
type RuleSource struct {
	// kind: PrometheusRule, or File for a rule Prometheus loaded from a plain rule file.
	// +kubebuilder:validation:Enum=PrometheusRule;File
	Kind string `json:"kind"`

	// namespace of the PrometheusRule.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// name of the PrometheusRule.
	// +optional
	Name string `json:"name,omitempty"`

	// file Prometheus loaded the rule from, as the rules API reports it.
	// +optional
	File string `json:"file,omitempty"`

	// group is the rule group's name.
	Group string `json:"group"`

	// alert is the alert name.
	Alert string `json:"alert"`
}

// AlertRuleSpec is a copy of one alerting rule. It is written by the operator only.
type AlertRuleSpec struct {
	// scopeRef is the name of the AlertScope this rule was found in.
	ScopeRef string `json:"scopeRef"`

	// source says where the rule is defined.
	Source RuleSource `json:"source"`

	// expr is the PromQL expression.
	Expr string `json:"expr"`

	// for is how long the expression must hold before the alert fires.
	// +optional
	For string `json:"for,omitempty"`

	// keepFiringFor is how long the alert keeps firing after the expression stops holding.
	// +optional
	KeepFiringFor string `json:"keepFiringFor,omitempty"`

	// labels of the rule.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// annotations of the rule.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// RuleQuality is the rule's measured behaviour over the window.
type RuleQuality struct {
	// activations: how many times the alert started firing (or pending) in the window.
	// +optional
	Activations int32 `json:"activations"`

	// activationsPerDay: activations divided by the window in days.
	// +optional
	ActivationsPerDay float64 `json:"activationsPerDay"`

	// medianFiringDuration: the median time one activation stayed firing.
	// +optional
	MedianFiringDuration Duration `json:"medianFiringDuration,omitempty"`

	// firingFraction: the share of the window the alert was firing.
	// +optional
	FiringFraction float64 `json:"firingFraction"`

	// silencedFraction: the share of the firing time covered by a matching silence.
	// +optional
	SilencedFraction float64 `json:"silencedFraction"`

	// lastFired: the last time the alert was seen firing.
	// +optional
	LastFired *metav1.Time `json:"lastFired,omitempty"`
}

// Finding is one problem with the rule.
type Finding struct {
	// type of the finding.
	Type FindingType `json:"type"`

	// severity of the finding.
	Severity Severity `json:"severity"`

	// message explains the finding in one sentence.
	Message string `json:"message"`
}

// AlertRuleStatus is the rule's verdict.
type AlertRuleStatus struct {
	// identity is the rule's stable id: scope, source, group and alert name. It survives
	// edits of the expression, duration and labels.
	// +optional
	Identity string `json:"identity,omitempty"`

	// loaded: Prometheus has loaded the rule.
	// +optional
	Loaded bool `json:"loaded"`

	// notLoadedReason explains why a defined rule is not loaded, when it is known.
	// +optional
	NotLoadedReason string `json:"notLoadedReason,omitempty"`

	// team owning the rule; "<none>" when no source names one.
	// +optional
	Team string `json:"team,omitempty"`

	// window of history the quality was measured over.
	// +optional
	Window Duration `json:"window,omitempty"`

	// firstSeen is when the operator first found the rule.
	// +optional
	FirstSeen *metav1.Time `json:"firstSeen,omitempty"`

	// lastEvaluated is when the quality was last measured. It stops moving while Prometheus
	// cannot be read: the verdict is then stale.
	// +optional
	LastEvaluated *metav1.Time `json:"lastEvaluated,omitempty"`

	// quality is the rule's measured behaviour.
	// +optional
	Quality RuleQuality `json:"quality,omitzero"`

	// findings on the rule, most severe first.
	// +optional
	// +listType=atomic
	Findings []Finding `json:"findings,omitempty"`

	// findingsSummary lists the finding types, comma-separated, for kubectl get.
	// +optional
	FindingsSummary string `json:"findingsSummary,omitempty"`

	// conditions: Healthy (False with the most severe finding as reason).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hsar,categories=hypersurgery
// +kubebuilder:printcolumn:name="Alert",type=string,JSONPath=`.spec.source.alert`
// +kubebuilder:printcolumn:name="Team",type=string,JSONPath=`.status.team`
// +kubebuilder:printcolumn:name="Loaded",type=boolean,JSONPath=`.status.loaded`
// +kubebuilder:printcolumn:name="Fired",type=integer,JSONPath=`.status.quality.activations`
// +kubebuilder:printcolumn:name="Last Fired",type=date,JSONPath=`.status.quality.lastFired`
// +kubebuilder:printcolumn:name="Findings",type=string,JSONPath=`.status.findingsSummary`

// AlertRule is a read-only mirror of one alerting rule with its owner and quality verdict.
// The operator creates, updates and deletes these objects; nobody else writes them.
type AlertRule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is a copy of the rule
	// +required
	Spec AlertRuleSpec `json:"spec"`

	// status is the verdict
	// +optional
	Status AlertRuleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AlertRuleList contains a list of AlertRule
type AlertRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AlertRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AlertRule{}, &AlertRuleList{})
		return nil
	})
}

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

// Endpoint is an HTTP API the operator reads: a Prometheus or an Alertmanager.
type Endpoint struct {
	// url is the base URL of the API, for example http://prometheus-operated.monitoring.svc:9090.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() in ['http', 'https']",message="url must be an http or https URL"
	URL string `json:"url"`

	// authSecretRef names a Secret in the operator's namespace with the credentials for this
	// endpoint. Keys read: "token" (bearer token), "username" and "password" (basic auth),
	// "ca.crt" (CA bundle for the server certificate), "tls.crt" and "tls.key" (client
	// certificate). All keys are optional; a token and basic auth are mutually exclusive.
	// +optional
	AuthSecretRef *LocalSecretReference `json:"authSecretRef,omitempty"`
}

// LocalSecretReference names a Secret in the operator's own namespace.
type LocalSecretReference struct {
	// name of the Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ObjectReference names a namespaced object.
type ObjectReference struct {
	// namespace of the object.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`

	// name of the object.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// PrometheusEndpoint is the Prometheus the scope reads rules and history from.
type PrometheusEndpoint struct {
	Endpoint `json:",inline"`

	// resourceRef names the prometheus-operator Prometheus object that runs this Prometheus.
	// With it, a rule that is defined but not loaded comes with the reason: the
	// PrometheusRule's labels do not match ruleSelector, or its namespace is outside
	// ruleNamespaceSelector.
	// +optional
	ResourceRef *ObjectReference `json:"resourceRef,omitempty"`
}

// TeamSource is one place the owning team of a rule is read from. Exactly one field is set.
// +kubebuilder:validation:XValidation:rule="[has(self.ruleLabel), has(self.objectLabel), has(self.namespaceLabel)].filter(x, x).size() == 1",message="exactly one of ruleLabel, objectLabel and namespaceLabel must be set"
type TeamSource struct {
	// ruleLabel is a label on the alerting rule itself.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	RuleLabel string `json:"ruleLabel,omitempty"`

	// objectLabel is a label on the PrometheusRule object the rule is defined in.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	ObjectLabel string `json:"objectLabel,omitempty"`

	// namespaceLabel is a label on the namespace of the PrometheusRule object.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	NamespaceLabel string `json:"namespaceLabel,omitempty"`
}

// Ownership says where a rule's team comes from and which hygiene rules apply.
type Ownership struct {
	// teamFrom lists the sources of the team, first match wins. Default: the rule label
	// "team", then the label "hs/team" on the PrometheusRule object, then on its namespace.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	TeamFrom []TeamSource `json:"teamFrom,omitempty"`

	// requireRunbook reports NoRunbook for a rule without a runbook_url annotation.
	// +optional
	// +kubebuilder:default=true
	RequireRunbook *bool `json:"requireRunbook,omitempty"`

	// requireSeverity lists the allowed values of the severity label; a rule without one of
	// them is reported as NoSeverity. Empty means the default: critical, warning, info.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=63
	// +listType=set
	RequireSeverity []string `json:"requireSeverity,omitempty"`
}

// Duration is a whole number and one unit: s, m, h or d. Examples: "90s", "15m", "30d".
// +kubebuilder:validation:Pattern=`^[0-9]{1,6}(s|m|h|d)$`
// +kubebuilder:validation:MaxLength=7
type Duration string

// NoisyThreshold decides Noisy.
type NoisyThreshold struct {
	// firingsAbove: more activations than this in the window make a rule Noisy.
	// +optional
	// +kubebuilder:default=50
	// +kubebuilder:validation:Minimum=1
	FiringsAbove int32 `json:"firingsAbove,omitempty"`
}

// FlappingThreshold decides Flapping: both conditions must hold.
type FlappingThreshold struct {
	// activationsPerDayAbove: more activations per day than this, on average over the window.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	ActivationsPerDayAbove int32 `json:"activationsPerDayAbove,omitempty"`

	// medianDurationBelow: the median time the alert stays firing is shorter than this.
	// +optional
	// +kubebuilder:default="10m"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) > 0",message="medianDurationBelow must be greater than zero"
	MedianDurationBelow Duration `json:"medianDurationBelow,omitempty"`
}

// NeverFiredThreshold decides NeverFired.
type NeverFiredThreshold struct {
	// after: a rule is reported as NeverFired only once the operator has known it this long.
	// +optional
	// +kubebuilder:default="30d"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] >= 3600",message="after must be at least 1h"
	After Duration `json:"after,omitempty"`
}

// AlwaysSilencedThreshold decides AlwaysSilenced.
type AlwaysSilencedThreshold struct {
	// silencedFractionAbove: a larger share of the rule's firing time covered by a matching
	// Alertmanager silence makes it AlwaysSilenced. Between 0 and 1.
	// +optional
	// +kubebuilder:default=0.8
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +kubebuilder:validation:ExclusiveMinimum=true
	SilencedFractionAbove float64 `json:"silencedFractionAbove,omitempty"`
}

// Quality configures how rules are judged from Prometheus' history.
type Quality struct {
	// window is the history each verdict looks at. It is capped to the retention Prometheus
	// reports; status.effectiveWindow shows the window actually used. Between 1h and 90d.
	// +optional
	// +kubebuilder:default="30d"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] >= 3600",message="window must be at least 1h"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] <= 7776000",message="window must be at most 90d"
	Window Duration `json:"window,omitempty"`

	// resyncInterval is how often the whole scope is read and judged again. Between 1m and 24h.
	// +optional
	// +kubebuilder:default="15m"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] >= 60",message="resyncInterval must be at least 1m"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] <= 86400",message="resyncInterval must be at most 24h"
	ResyncInterval Duration `json:"resyncInterval,omitempty"`

	// noisy decides Noisy.
	// +optional
	// +kubebuilder:default={}
	Noisy NoisyThreshold `json:"noisy,omitzero"`

	// flapping decides Flapping.
	// +optional
	// +kubebuilder:default={}
	Flapping FlappingThreshold `json:"flapping,omitzero"`

	// neverFired decides NeverFired.
	// +optional
	// +kubebuilder:default={}
	NeverFired NeverFiredThreshold `json:"neverFired,omitzero"`

	// alwaysSilenced decides AlwaysSilenced.
	// +optional
	// +kubebuilder:default={}
	AlwaysSilenced AlwaysSilencedThreshold `json:"alwaysSilenced,omitzero"`
}

// QueryBudget bounds the load the operator puts on Prometheus and Alertmanager.
type QueryBudget struct {
	// timeout of a single query.
	// +optional
	// +kubebuilder:default="20s"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] >= 1",message="timeout must be at least 1s"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] <= 300",message="timeout must be at most 5m"
	Timeout Duration `json:"timeout,omitempty"`

	// maxConcurrent queries in flight at once.
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`

	// maxQueriesPerMinute across the scope. When it runs out, the rest of the work waits for
	// the next sync instead of failing this one.
	// +optional
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=6000
	MaxQueriesPerMinute int32 `json:"maxQueriesPerMinute,omitempty"`
}

// AlertScopeSpec says where to read alert rules from, which ones to inventory and how to
// judge them.
type AlertScopeSpec struct {
	// prometheus the rules and their history are read from.
	// +required
	Prometheus PrometheusEndpoint `json:"prometheus"`

	// alertmanager the silences are read from. Without it AlwaysSilenced is not evaluated.
	// +optional
	Alertmanager *Endpoint `json:"alertmanager,omitempty"`

	// namespaceSelector picks the namespaces whose PrometheusRule objects belong to this
	// scope. Empty selects all namespaces.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// ownership: where the team comes from and which hygiene rules apply.
	// +optional
	// +kubebuilder:default={}
	Ownership Ownership `json:"ownership,omitzero"`

	// quality: the history window and the thresholds of each verdict.
	// +optional
	// +kubebuilder:default={}
	Quality Quality `json:"quality,omitzero"`

	// silences is the policy for AlertSilence objects of this scope (v0.3): off by default.
	// +optional
	// +kubebuilder:default={}
	Silences SilencePolicy `json:"silences,omitzero"`

	// queryBudget bounds the load on Prometheus and Alertmanager.
	// +optional
	// +kubebuilder:default={}
	QueryBudget QueryBudget `json:"queryBudget,omitzero"`
}

// RuleCounts counts the rules of a scope.
type RuleCounts struct {
	// total number of alerting rules in the scope.
	Total int32 `json:"total"`
	// loaded by Prometheus.
	Loaded int32 `json:"loaded"`
	// notLoaded: defined in a PrometheusRule in scope but absent from Prometheus.
	NotLoaded int32 `json:"notLoaded"`
}

// TeamSummary counts one team's rules and findings.
type TeamSummary struct {
	// team name; "<none>" for rules without an owner.
	Team string `json:"team"`
	// rules owned by the team.
	Rules int32 `json:"rules"`
	// findings on those rules.
	Findings int32 `json:"findings"`
}

// AlertScopeStatus is what the operator last observed for the scope.
type AlertScopeStatus struct {
	// observedGeneration is the generation of the spec this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastSync is the time of the last complete, successful sync.
	// +optional
	LastSync *metav1.Time `json:"lastSync,omitempty"`

	// effectiveWindow is the history window actually used: spec.quality.window, capped to the
	// retention Prometheus reports.
	// +optional
	EffectiveWindow Duration `json:"effectiveWindow,omitempty"`

	// rules counts the alerting rules in the scope.
	// +optional
	Rules RuleCounts `json:"rules,omitzero"`

	// findingsTotal is the number of findings on all rules of the scope.
	// +optional
	FindingsTotal int32 `json:"findingsTotal,omitempty"`

	// findings counts the rules with each finding type.
	// +optional
	Findings map[FindingType]int32 `json:"findings,omitempty"`

	// teams lists rule and finding counts per team, sorted by team.
	// +optional
	// +listType=atomic
	Teams []TeamSummary `json:"teams,omitempty"`

	// conflicts lists AlertRule objects the operator wanted to write but did not, because an
	// object of that name exists and is not its own (namespace/name).
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=100
	Conflicts []string `json:"conflicts,omitempty"`

	// conditions: Ready (reasons Synced, PrometheusUnreachable, AlertmanagerUnreachable,
	// BudgetExhausted, Conflict, InvalidSpec).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=hsas,categories=hypersurgery
// +kubebuilder:printcolumn:name="Rules",type=integer,JSONPath=`.status.rules.total`
// +kubebuilder:printcolumn:name="Loaded",type=integer,JSONPath=`.status.rules.loaded`
// +kubebuilder:printcolumn:name="Findings",type=integer,JSONPath=`.status.findingsTotal`
// +kubebuilder:printcolumn:name="Window",type=string,JSONPath=`.status.effectiveWindow`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last Sync",type=date,JSONPath=`.status.lastSync`

// AlertScope declares where the operator reads alert rules from (one Prometheus and,
// optionally, its Alertmanager), which PrometheusRule objects belong to the inventory, and how
// each rule is judged.
type AlertScope struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AlertScope
	// +required
	Spec AlertScopeSpec `json:"spec"`

	// status defines the observed state of AlertScope
	// +optional
	Status AlertScopeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AlertScopeList contains a list of AlertScope
type AlertScopeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AlertScope `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AlertScope{}, &AlertScopeList{})
		return nil
	})
}

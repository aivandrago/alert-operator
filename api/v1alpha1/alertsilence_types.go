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

// Silence-related constants.
const (
	// SilenceCreatedBy is the createdBy of every silence the operator writes.
	SilenceCreatedBy = "alert-operator"
	// FinalizerSilence expires the silence when its AlertSilence is deleted.
	FinalizerSilence = "alerts.hypersurgery.dev/silence"
)

// SilenceMode is a scope's silence policy.
// +kubebuilder:validation:Enum=Off;Propose;Apply
type SilenceMode string

// Silence modes (docs/design.md §11.2).
const (
	SilenceModeOff     SilenceMode = "Off"
	SilenceModePropose SilenceMode = "Propose"
	SilenceModeApply   SilenceMode = "Apply"
)

// SilenceState is where an AlertSilence stands.
// +kubebuilder:validation:Enum=Disabled;Proposed;Rejected;Pending;Active;Expired;ExpiredExternally
type SilenceState string

// Silence states.
const (
	SilenceDisabled          SilenceState = "Disabled"
	SilenceProposed          SilenceState = "Proposed"
	SilenceRejected          SilenceState = "Rejected"
	SilencePending           SilenceState = "Pending"
	SilenceActive            SilenceState = "Active"
	SilenceExpired           SilenceState = "Expired"
	SilenceExpiredExternally SilenceState = "ExpiredExternally"
)

// SilencePolicy is what a scope allows AlertSilence objects to do.
type SilencePolicy struct {
	// mode: Off ignores AlertSilence objects of this scope, Propose computes the silences
	// without creating them, Apply creates and expires them (the manager also needs
	// --enable-writes).
	// +optional
	// +kubebuilder:default=Off
	Mode SilenceMode `json:"mode,omitempty"`

	// maxDuration is the longest silence an AlertSilence of this scope may ask for. Between 1m
	// and 90d.
	// +optional
	// +kubebuilder:default="7d"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] >= 60",message="maxDuration must be at least 1m"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] <= 7776000",message="maxDuration must be at most 90d"
	MaxDuration Duration `json:"maxDuration,omitempty"`

	// restrictToNamespaceRules requires an alertname matcher whose alerts are all rules of the
	// AlertSilence's namespace, and of no other namespace: a team silences only its own alerts.
	// +optional
	// +kubebuilder:default=true
	RestrictToNamespaceRules *bool `json:"restrictToNamespaceRules,omitempty"`
}

// SilenceMatcher is one matcher of a silence, as in Alertmanager.
type SilenceMatcher struct {
	// name of the label.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// value to match; a regular expression with isRegex.
	// +required
	// +kubebuilder:validation:MaxLength=1024
	Value string `json:"value"`

	// isRegex: value is a regular expression (anchored, as in Alertmanager).
	// +optional
	IsRegex bool `json:"isRegex,omitempty"`

	// isEqual: false negates the matcher (!= or !~).
	// +optional
	// +kubebuilder:default=true
	IsEqual *bool `json:"isEqual,omitempty"`
}

// AlertSilenceSpec is a silence a team asks for.
// +kubebuilder:validation:XValidation:rule="has(self.expiresAt) != has(self.duration)",message="exactly one of expiresAt and duration must be set"
type AlertSilenceSpec struct {
	// scopeRef names the AlertScope whose Alertmanager gets the silence and whose policy
	// applies.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ScopeRef string `json:"scopeRef"`

	// matchers select the alerts to silence.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	Matchers []SilenceMatcher `json:"matchers"`

	// startsAt is when the silence begins; default: when the object is created.
	// +optional
	StartsAt *metav1.Time `json:"startsAt,omitempty"`

	// expiresAt is when the silence ends.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// duration is how long the silence lasts from its start. Between 1m and 90d; the scope's
	// silences.maxDuration may allow less.
	// +optional
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] >= 60",message="duration must be at least 1m"
	// +kubebuilder:validation:XValidation:rule="int(self.substring(0, size(self)-1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[self.substring(size(self)-1)] <= 7776000",message="duration must be at most 90d"
	Duration Duration `json:"duration,omitempty"`

	// owner is the team the silence is for.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Owner string `json:"owner"`

	// reason says why, for whoever finds the silence later.
	// +required
	// +kubebuilder:validation:MinLength=10
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason"`

	// dryRun only proposes: the status shows the silence and the alerts it would match, and
	// nothing is written to Alertmanager.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

// ProposedSilence is the silence the operator would create (or has created).
type ProposedSilence struct {
	Matchers  []SilenceMatcher `json:"matchers"`
	StartsAt  metav1.Time      `json:"startsAt"`
	EndsAt    metav1.Time      `json:"endsAt"`
	CreatedBy string           `json:"createdBy"`
	Comment   string           `json:"comment"`
}

// AlertSilenceStatus is what the operator did with the AlertSilence.
type AlertSilenceStatus struct {
	// observedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// state of the silence.
	// +optional
	State SilenceState `json:"state,omitempty"`

	// message explains the state, in particular why an object was rejected.
	// +optional
	Message string `json:"message,omitempty"`

	// silenceID is the silence's ID in Alertmanager.
	// +optional
	SilenceID string `json:"silenceID,omitempty"`

	// startsAt and endsAt of the silence.
	// +optional
	StartsAt *metav1.Time `json:"startsAt,omitempty"`
	// +optional
	EndsAt *metav1.Time `json:"endsAt,omitempty"`

	// specHash identifies the spec the silence was written for.
	// +optional
	SpecHash string `json:"specHash,omitempty"`

	// matchedAlerts is how many alerts Alertmanager currently has that the matchers match.
	// +optional
	MatchedAlerts int32 `json:"matchedAlerts"`

	// proposed is the silence, as the operator would write it.
	// +optional
	Proposed *ProposedSilence `json:"proposed,omitempty"`

	// conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hsasil,categories=hypersurgery
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scopeRef`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Ends",type=date,JSONPath=`.status.endsAt`
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=`.status.matchedAlerts`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Silence",type=string,JSONPath=`.status.silenceID`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AlertSilence is an Alertmanager silence declared as code: matchers, an owner, a reason and a
// mandatory end, limited to the namespace's own alerts. The operator creates the silence, keeps
// it in step and expires it when the object is deleted (docs/silences.md).
type AlertSilence struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the silence asked for
	// +required
	Spec AlertSilenceSpec `json:"spec"`

	// status is what the operator did with it
	// +optional
	Status AlertSilenceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AlertSilenceList contains a list of AlertSilence
type AlertSilenceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AlertSilence `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AlertSilence{}, &AlertSilenceList{})
		return nil
	})
}

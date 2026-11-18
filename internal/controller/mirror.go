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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/inventory"
)

// FieldOwner is the server-side apply field manager of everything the operator writes.
const FieldOwner = "alert-operator"

// MirrorName is the AlertRule name of an inventory entry: the alert name in lower case with
// anything outside [a-z0-9-] turned into "-", then "-" and six hex digits of the identity, at
// most 63 characters.
func MirrorName(alert, identity string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(alert) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	prefix := strings.Trim(b.String(), "-")
	suffix := identity
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	if limit := validation.DNS1123LabelMaxLength - len(suffix) - 1; len(prefix) > limit {
		prefix = strings.TrimRight(prefix[:limit], "-")
	}
	if prefix == "" {
		prefix = "rule"
	}
	return prefix + "-" + suffix
}

// mirrorNamespace is where an entry's AlertRule lives: the PrometheusRule's namespace, or the
// operator's for a rule loaded from a plain file.
func mirrorNamespace(e *inventory.Entry, operatorNamespace string) string {
	if e.Source.Kind == alertsv1alpha1.SourceKindRule {
		return e.Source.Namespace
	}
	return operatorNamespace
}

// desiredMirror is the AlertRule (metadata and spec) for an entry.
func desiredMirror(scope string, e *inventory.Entry, operatorNamespace string) *alertsv1alpha1.AlertRule {
	ar := &alertsv1alpha1.AlertRule{
		TypeMeta: metav1.TypeMeta{APIVersion: alertsv1alpha1.GroupVersion.String(), Kind: "AlertRule"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      MirrorName(e.Source.Alert, e.Identity),
			Namespace: mirrorNamespace(e, operatorNamespace),
			Labels: map[string]string{
				alertsv1alpha1.LabelManagedBy: alertsv1alpha1.ManagedByValue,
				alertsv1alpha1.LabelScope:     scope,
			},
		},
		Spec: alertsv1alpha1.AlertRuleSpec{
			ScopeRef:      scope,
			Source:        e.Source,
			Expr:          e.Expr,
			For:           e.For,
			KeepFiringFor: e.KeepFiringFor,
			Labels:        e.Labels,
			Annotations:   e.Annotations,
		},
	}
	if e.Team != alertsv1alpha1.NoTeam && len(validation.IsValidLabelValue(e.Team)) == 0 {
		ar.Labels[alertsv1alpha1.LabelTeam] = e.Team
	}
	if e.Source.Kind == alertsv1alpha1.SourceKindRule && e.SourceUID != "" {
		// Garbage-collected with its source, never the other way round.
		ar.OwnerReferences = []metav1.OwnerReference{{
			APIVersion:         "monitoring.coreos.com/v1",
			Kind:               "PrometheusRule",
			Name:               e.Source.Name,
			UID:                e.SourceUID,
			Controller:         new(false),
			BlockOwnerDeletion: new(false),
		}}
	}
	return ar
}

// ours reports whether an existing AlertRule is the operator's own for this scope: the only
// objects it ever updates or deletes.
func ours(obj metav1.Object, scope string) bool {
	l := obj.GetLabels()
	return l[alertsv1alpha1.LabelManagedBy] == alertsv1alpha1.ManagedByValue && l[alertsv1alpha1.LabelScope] == scope
}

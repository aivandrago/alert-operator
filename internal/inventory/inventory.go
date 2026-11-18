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

// Package inventory joins what should run (alerting rules in PrometheusRule objects) with what
// does run (the rules Prometheus reports as loaded) into one entry per rule with a stable
// identity (docs/design.md §5 steps 1-2). It is pure: no I/O, so the join is tested on tables.
package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

// LoadGrace is how long a PrometheusRule object may exist before its rules missing from
// Prometheus count as NotLoaded: prometheus-operator and Prometheus need a moment to reload.
const LoadGrace = 5 * time.Minute

// Input is everything the join needs.
type Input struct {
	// Scope is the AlertScope's name.
	Scope string
	// Rules are all PrometheusRule objects the operator can see, in scope or not.
	Rules []monitoringv1.PrometheusRule
	// Namespaces maps every namespace to its labels.
	Namespaces map[string]map[string]string
	// InScope says whether a namespace belongs to the scope (spec.namespaceSelector).
	InScope func(namespace string) bool
	// Loaded are the alerting rule groups Prometheus reports.
	Loaded []promapi.RuleGroup
	// Prometheus is the object named by spec.prometheus.resourceRef, if any.
	Prometheus *monitoringv1.Prometheus
	// Settings are the scope's ownership rules.
	Settings alertsv1alpha1.Settings
	// Now is the time of the sync.
	Now time.Time
}

// Entry is one alerting rule of the inventory.
type Entry struct {
	Identity string
	Source   alertsv1alpha1.RuleSource
	// SourceUID is the PrometheusRule's UID, for the mirror's ownerReference.
	SourceUID     types.UID
	Expr          string
	For           string
	KeepFiringFor string
	Labels        map[string]string
	Annotations   map[string]string

	Loaded bool
	// Pending is true for a defined rule that is not loaded yet but whose object is younger
	// than LoadGrace: no NotLoaded finding yet.
	Pending         bool
	NotLoadedReason string
	// Group interval Prometheus evaluates the rule at, when loaded.
	Interval time.Duration

	Team     string
	Findings []alertsv1alpha1.Finding
}

// Result is the inventory of one scope.
type Result struct {
	Entries []Entry
}

// Identity is the stable id of a rule: it depends on where the rule is defined and on its
// name, never on its expression, duration or labels, so editing a rule keeps its history.
// ordinal tells apart rules with the same alert name in the same group (0 for the first).
func Identity(scope string, src alertsv1alpha1.RuleSource, ordinal int) string {
	where := src.Namespace + "/" + src.Name
	if src.Kind == alertsv1alpha1.SourceKindFile {
		where = "file:" + src.File
	}
	key := strings.Join([]string{scope, where, src.Group, src.Alert, strconv.Itoa(ordinal)}, "\x00")
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:8]
}

type defKey struct{ namespace, name, group, alert string }

type definition struct {
	obj     *monitoringv1.PrometheusRule
	rule    monitoringv1.Rule
	group   string
	ordinal int
}

// Build joins the defined rules with the loaded ones.
func Build(in Input) Result {
	byFile := map[string]*monitoringv1.PrometheusRule{} // "<ns>-<name>-<uid>.yaml" -> object
	defs := map[defKey][]*definition{}
	var order []defKey
	for i := range in.Rules {
		obj := &in.Rules[i]
		byFile[fmt.Sprintf("%s-%s-%s.yaml", obj.Namespace, obj.Name, obj.UID)] = obj
		if !in.InScope(obj.Namespace) {
			continue
		}
		for _, g := range obj.Spec.Groups {
			for _, r := range g.Rules {
				if r.Alert == "" {
					continue
				}
				k := defKey{obj.Namespace, obj.Name, g.Name, r.Alert}
				if _, seen := defs[k]; !seen {
					order = append(order, k)
				}
				defs[k] = append(defs[k], &definition{obj: obj, rule: r, group: g.Name, ordinal: len(defs[k])})
			}
		}
	}

	matched := map[*definition]*promapi.Rule{}
	matchedInterval := map[*definition]time.Duration{}
	var fileOnly []Entry
	fileOrdinals := map[string]int{}
	for gi := range in.Loaded {
		g := &in.Loaded[gi]
		obj := byFile[path.Base(g.File)]
		if obj != nil && !in.InScope(obj.Namespace) {
			continue // defined in a namespace outside the scope
		}
		used := map[string]int{}
		for ri := range g.Rules {
			r := &g.Rules[ri]
			var d *definition
			if obj != nil {
				list := defs[defKey{obj.Namespace, obj.Name, g.Name, r.Name}]
				if n := used[r.Name]; n < len(list) {
					d = list[n]
				}
				used[r.Name]++
			} else {
				d = matchByName(defs, g.Name, r.Name, matched)
			}
			if d != nil {
				matched[d] = r
				matchedInterval[d] = time.Duration(g.Interval * float64(time.Second))
				continue
			}
			if obj != nil {
				// A stale rule of an object whose new version Prometheus has not reloaded yet:
				// the definition that replaced it is reported, this one goes with the reload.
				continue
			}
			src := alertsv1alpha1.RuleSource{
				Kind: alertsv1alpha1.SourceKindFile, File: g.File, Group: g.Name, Alert: r.Name,
			}
			ordKey := g.File + "\x00" + g.Name + "\x00" + r.Name
			e := Entry{
				Identity:      Identity(in.Scope, src, fileOrdinals[ordKey]),
				Source:        src,
				Expr:          r.Query,
				For:           secondsToDuration(r.Duration),
				KeepFiringFor: secondsToDuration(r.KeepFiringFor),
				Labels:        r.Labels,
				Annotations:   r.Annotations,
				Loaded:        true,
				Interval:      time.Duration(g.Interval * float64(time.Second)),
			}
			fileOrdinals[ordKey]++
			loadError(&e, r)
			fileOnly = append(fileOnly, e)
		}
	}

	out := Result{}
	for _, k := range order {
		for _, d := range defs[k] {
			src := alertsv1alpha1.RuleSource{
				Kind: alertsv1alpha1.SourceKindRule, Namespace: d.obj.Namespace, Name: d.obj.Name,
				Group: d.group, Alert: d.rule.Alert,
			}
			e := Entry{
				Identity:    Identity(in.Scope, src, d.ordinal),
				Source:      src,
				SourceUID:   d.obj.UID,
				Expr:        d.rule.Expr.String(),
				Labels:      d.rule.Labels,
				Annotations: d.rule.Annotations,
			}
			if d.rule.For != nil {
				e.For = string(*d.rule.For)
			}
			if d.rule.KeepFiringFor != nil {
				e.KeepFiringFor = string(*d.rule.KeepFiringFor)
			}
			if r, ok := matched[d]; ok {
				e.Loaded = true
				e.Interval = matchedInterval[d]
				loadError(&e, r)
			} else {
				notLoaded(&e, d.obj, in)
			}
			out.Entries = append(out.Entries, e)
		}
	}
	out.Entries = append(out.Entries, fileOnly...)
	for i := range out.Entries {
		e := &out.Entries[i]
		var obj *monitoringv1.PrometheusRule
		if e.Source.Kind == alertsv1alpha1.SourceKindRule {
			for j := range in.Rules {
				if in.Rules[j].UID == e.SourceUID {
					obj = &in.Rules[j]
					break
				}
			}
		}
		applyOwnership(e, obj, in.Namespaces, in.Settings)
		SortFindings(e.Findings)
	}
	return out
}

// matchByName finds the definition of a loaded rule whose file names no object (a Prometheus
// not run by prometheus-operator, or a renamed file) by group and alert name, when that is
// unambiguous.
func matchByName(defs map[defKey][]*definition, group, alert string, matched map[*definition]*promapi.Rule) *definition {
	var found *definition
	for k, list := range defs {
		if k.group != group || k.alert != alert {
			continue
		}
		for _, d := range list {
			if _, taken := matched[d]; taken {
				continue
			}
			if found != nil {
				return nil // ambiguous
			}
			found = d
		}
	}
	return found
}

func loadError(e *Entry, r *promapi.Rule) {
	if r.Health == "err" || r.LastError != "" {
		msg := r.LastError
		if msg == "" {
			msg = "Prometheus reports the rule unhealthy"
		}
		e.Findings = append(e.Findings, finding(alertsv1alpha1.FindingLoadError,
			"last evaluation failed: "+msg))
	}
}

func notLoaded(e *Entry, obj *monitoringv1.PrometheusRule, in Input) {
	reason := explainNotLoaded(obj, in)
	e.NotLoadedReason = reason
	if age := in.Now.Sub(obj.CreationTimestamp.Time); age < LoadGrace && reason == reasonUnknown {
		e.Pending = true
		e.NotLoadedReason = "waiting for Prometheus to load the rule (object created " +
			age.Round(time.Second).String() + " ago)"
		return
	}
	e.Findings = append(e.Findings, finding(alertsv1alpha1.FindingNotLoaded, reason))
}

const reasonUnknown = "defined in the PrometheusRule but absent from the Prometheus rules API"

func explainNotLoaded(obj *monitoringv1.PrometheusRule, in Input) string {
	p := in.Prometheus
	if p == nil {
		return reasonUnknown
	}
	promName := p.Namespace + "/" + p.Name
	// ruleSelector: null selects no PrometheusRule at all, empty selects all.
	if p.Spec.RuleSelector == nil {
		return fmt.Sprintf("Prometheus %s has no ruleSelector, so it loads no PrometheusRule", promName)
	}
	sel, err := metav1LabelSelector(p.Spec.RuleSelector)
	if err == nil && !sel.Matches(labels.Set(obj.Labels)) {
		return fmt.Sprintf("the labels of PrometheusRule %s/%s do not match ruleSelector of Prometheus %s (%s)",
			obj.Namespace, obj.Name, promName, sel.String())
	}
	// ruleNamespaceSelector: null means the Prometheus object's own namespace only.
	if p.Spec.RuleNamespaceSelector == nil {
		if obj.Namespace != p.Namespace {
			return fmt.Sprintf("namespace %s is outside ruleNamespaceSelector of Prometheus %s "+
				"(unset: only its own namespace %s)", obj.Namespace, promName, p.Namespace)
		}
	} else if nsSel, err := metav1LabelSelector(p.Spec.RuleNamespaceSelector); err == nil &&
		!nsSel.Matches(labels.Set(in.Namespaces[obj.Namespace])) {
		return fmt.Sprintf("namespace %s is outside ruleNamespaceSelector of Prometheus %s (%s)",
			obj.Namespace, promName, nsSel.String())
	}
	return reasonUnknown + "; the selectors of Prometheus " + promName +
		" match it, so prometheus-operator may have rejected it (see its logs) or not reloaded yet"
}

func finding(t alertsv1alpha1.FindingType, msg string) alertsv1alpha1.Finding {
	return alertsv1alpha1.Finding{Type: t, Severity: alertsv1alpha1.SeverityOf(t), Message: msg}
}

// SortFindings orders findings most severe first, then in the order of FindingTypes.
func SortFindings(fs []alertsv1alpha1.Finding) {
	rank := func(s alertsv1alpha1.Severity) int {
		switch s {
		case alertsv1alpha1.SeverityCritical:
			return 0
		case alertsv1alpha1.SeverityWarning:
			return 1
		default:
			return 2
		}
	}
	slices.SortStableFunc(fs, func(a, b alertsv1alpha1.Finding) int {
		if d := rank(a.Severity) - rank(b.Severity); d != 0 {
			return d
		}
		return slices.Index(alertsv1alpha1.FindingTypes, a.Type) - slices.Index(alertsv1alpha1.FindingTypes, b.Type)
	})
}

func secondsToDuration(s float64) string {
	if s <= 0 {
		return ""
	}
	return string(alertsv1alpha1.FormatDuration(time.Duration(s * float64(time.Second))))
}

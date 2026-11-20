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

// Package lint checks PrometheusRule objects before they are applied (docs/design.md §10). The
// validating webhook and the hsalert CLI both call Check; neither implements a check of its own.
package lint

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/analyze"
	"hypersurgery.dev/alert-operator/internal/inventory"
)

// Problem is one thing wrong with a rule, or with the object as a whole (Group and Rule empty).
type Problem struct {
	Group string `json:"group,omitempty"`
	// Rule is the alert or record name.
	Rule    string                     `json:"rule,omitempty"`
	Check   alertsv1alpha1.FindingType `json:"check"`
	Message string                     `json:"message"`
	// Blocking problems deny the object in enforce mode and fail the CLI. DeadQuery never
	// blocks: a rule may legitimately arrive before the metric it watches.
	Blocking bool `json:"blocking"`
}

// String writes the problem on one line.
func (p Problem) String() string {
	where := ""
	if p.Group != "" {
		where = p.Group + "/" + p.Rule + ": "
	}
	return fmt.Sprintf("%s%s: %s", where, p.Check, p.Message)
}

// Input is what Check needs. Only Rule is required.
type Input struct {
	Rule *monitoringv1.PrometheusRule
	// NamespaceLabels are the labels of the object's namespace, for team resolution and the
	// ruleNamespaceSelector.
	NamespaceLabels map[string]string
	// Settings are the ownership rules (an AlertScope's, or the defaults).
	Settings alertsv1alpha1.Settings
	// Prometheus is the Prometheus object whose selectors decide NotLoaded; nil skips the check.
	Prometheus *monitoringv1.Prometheus
	// Querier answers DeadQuery's count queries; nil skips the check.
	Querier analyze.Querier
	Now     time.Time
}

// Result is what Check found, and the checks it could not run and why.
type Result struct {
	Problems []Problem `json:"problems"`
	Skipped  []string  `json:"skipped,omitempty"`
}

// Blocking reports whether any problem blocks.
func (r Result) Blocking() bool {
	return slices.ContainsFunc(r.Problems, func(p Problem) bool { return p.Blocking })
}

var parser = analyze.NewParser()

// Check runs every check on the object. It returns problems in the order of the object's
// groups and rules; object-wide problems come first.
func Check(ctx context.Context, in Input) Result {
	var res Result
	pr := in.Rule
	if in.Prometheus != nil {
		if ok, reason := inventory.WouldLoad(in.Prometheus, pr.Namespace, pr.Name, pr.Labels, in.NamespaceLabels); !ok {
			res.Problems = append(res.Problems, Problem{Check: alertsv1alpha1.FindingNotLoaded, Message: reason, Blocking: true})
		}
	} else {
		res.Skipped = append(res.Skipped, "NotLoaded: no Prometheus object to check the selectors against")
	}
	var dq *analyze.DeadQueryChecker
	if in.Querier != nil {
		dq = analyze.NewDeadQueryChecker(in.Querier, in.Now)
	} else {
		res.Skipped = append(res.Skipped, "DeadQuery: no Prometheus to ask")
	}
	namespaces := map[string]map[string]string{pr.Namespace: in.NamespaceLabels}
	deadQuerySkipped := ""
	for _, g := range pr.Spec.Groups {
		for _, r := range g.Rules {
			name := r.Alert
			if name == "" {
				name = r.Record
			}
			add := func(t alertsv1alpha1.FindingType, msg string, blocking bool) {
				res.Problems = append(res.Problems, Problem{Group: g.Name, Rule: name, Check: t, Message: msg, Blocking: blocking})
			}
			expr := r.Expr.String()
			if _, err := parser.ParseExpr(expr); err != nil {
				add(alertsv1alpha1.FindingLoadError, "the expression does not parse: "+err.Error(), true)
				continue
			}
			if r.Alert == "" {
				continue // recording rule: parsing is all that applies
			}
			_, fs := inventory.Ownership(r.Labels, r.Annotations, pr, namespaces, in.Settings)
			for _, f := range fs {
				add(f.Type, f.Message, true)
			}
			if dq == nil || deadQuerySkipped != "" {
				continue
			}
			f, err := dq.Check(ctx, expr)
			switch {
			case err != nil:
				deadQuerySkipped = "DeadQuery: " + describe(err)
			case f != nil:
				add(f.Type, f.Message, false)
			}
		}
	}
	if deadQuerySkipped != "" {
		res.Skipped = append(res.Skipped, deadQuerySkipped)
	}
	return res
}

func describe(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "Prometheus did not answer in time"
	default:
		return strings.TrimSpace(err.Error())
	}
}

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

// Package analyze judges alerting rules from Prometheus' own data: whether their selectors
// match any series (DeadQuery) and how they behaved over the window (Noisy, Flapping,
// NeverFired, AlwaysSilenced).
package analyze

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

// Querier runs instant queries; *promapi.Prometheus is one.
type Querier interface {
	Query(ctx context.Context, q string, ts time.Time) ([]promapi.Sample, error)
}

// DeadQueryLookback is how far back a selector may last have had a series and still count as
// alive. A rule on a series that comes and goes (a failed job, a pending pod) is not dead.
const DeadQueryLookback = time.Hour

var promqlParser = parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

// ParseError is an expression Prometheus' parser refuses. It is reported as LoadError.
type ParseError struct{ Err error }

func (e *ParseError) Error() string { return "the expression does not parse: " + e.Err.Error() }

// DeadQueryChecker checks the selectors of rule expressions with cheap count queries. It
// caches each selector's answer, so a selector shared by many rules is asked once per sync:
// make a new checker for every sync.
type DeadQueryChecker struct {
	q     Querier
	now   time.Time
	cache map[string]bool // selector query -> has series
}

// NewDeadQueryChecker returns a checker for one sync at now.
func NewDeadQueryChecker(q Querier, now time.Time) *DeadQueryChecker {
	return &DeadQueryChecker{q: q, now: now, cache: map[string]bool{}}
}

// Check returns a DeadQuery finding when the expression cannot return anything because of
// selectors that match no series, or nil when it can. A parse error comes back as a
// *ParseError. promapi.ErrBudgetExhausted means the answer is not known yet: keep the previous
// verdict.
func (c *DeadQueryChecker) Check(ctx context.Context, expr string) (*alertsv1alpha1.Finding, error) {
	root, err := promqlParser.ParseExpr(expr)
	if err != nil {
		return nil, &ParseError{Err: err}
	}
	empty := map[parser.Node]string{} // selector node -> its text, for those with no series
	var budgetErr error
	var walk func(n parser.Node, exempt bool, rng time.Duration) error
	walk = func(n parser.Node, exempt bool, rng time.Duration) error {
		switch n := n.(type) {
		case *parser.Call:
			if n.Func.Name == "absent" || n.Func.Name == "absent_over_time" {
				exempt = true // empty is the point
			}
		case *parser.SubqueryExpr:
			rng = max(rng, n.Range)
		case *parser.MatrixSelector:
			if !exempt {
				q := countQuery(n.VectorSelector.(*parser.VectorSelector), max(n.Range, rng))
				ok, err := c.hasSeries(ctx, q)
				if err != nil {
					return err
				}
				if !ok {
					empty[n] = n.String()
				}
			}
			return nil // the inner vector selector was just checked
		case *parser.VectorSelector:
			if !exempt {
				ok, err := c.hasSeries(ctx, countQuery(n, rng))
				if err != nil {
					return err
				}
				if !ok {
					empty[n] = n.String()
				}
			}
			return nil
		}
		for child := range parser.ChildrenIter(n) {
			if err := walk(child, exempt, rng); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, false, 0); err != nil {
		if errors.Is(err, promapi.ErrBudgetExhausted) {
			budgetErr = err
		} else {
			return nil, err
		}
	}
	if budgetErr != nil {
		return nil, budgetErr
	}
	if len(empty) == 0 || !alwaysEmpty(root, empty) {
		return nil, nil
	}
	var names []string
	seen := map[string]bool{}
	parser.Inspect(root, func(n parser.Node, _ []parser.Node) error {
		if s, ok := empty[n]; ok && !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
		return nil
	})
	return &alertsv1alpha1.Finding{
		Type:     alertsv1alpha1.FindingDeadQuery,
		Severity: alertsv1alpha1.SeverityOf(alertsv1alpha1.FindingDeadQuery),
		Message: fmt.Sprintf("selects no series in the last %s, so the rule cannot fire: %s",
			alertsv1alpha1.FormatDuration(DeadQueryLookback), strings.Join(names, ", ")),
	}, nil
}

// countQuery asks whether the selector had any series in the lookback (or the selector's own
// range, if longer): count(last_over_time(<selector>[range])), offset and @ kept.
func countQuery(vs *parser.VectorSelector, rng time.Duration) string {
	m := &parser.MatrixSelector{VectorSelector: vs, Range: max(rng, DeadQueryLookback)}
	return "count(last_over_time(" + m.String() + "))"
}

func (c *DeadQueryChecker) hasSeries(ctx context.Context, q string) (bool, error) {
	if v, ok := c.cache[q]; ok {
		return v, nil
	}
	res, err := c.q.Query(ctx, q, c.now)
	if err != nil {
		return false, err
	}
	ok := len(res) > 0 && res[0].Value > 0
	c.cache[q] = ok
	return ok, nil
}

// alwaysEmpty reports whether the expression returns nothing, given that the selectors in
// empty match no series. It follows PromQL's semantics: "a or b" needs both sides empty,
// "a unless b" only a, "a and b" and arithmetic between vectors either side; functions and
// aggregations of an empty vector are empty, except absent(), absent_over_time() and those
// returning a scalar.
func alwaysEmpty(n parser.Node, empty map[parser.Node]string) bool {
	switch n := n.(type) {
	case *parser.VectorSelector, *parser.MatrixSelector:
		_, ok := empty[n]
		return ok
	case *parser.ParenExpr:
		return alwaysEmpty(n.Expr, empty)
	case *parser.UnaryExpr:
		return alwaysEmpty(n.Expr, empty)
	case *parser.SubqueryExpr:
		return alwaysEmpty(n.Expr, empty)
	case *parser.StepInvariantExpr:
		return alwaysEmpty(n.Expr, empty)
	case *parser.AggregateExpr:
		return alwaysEmpty(n.Expr, empty)
	case *parser.Call:
		if n.Func.Name == "absent" || n.Func.Name == "absent_over_time" || n.Func.ReturnType == parser.ValueTypeScalar {
			return false
		}
		for _, a := range n.Args {
			if (a.Type() == parser.ValueTypeVector || a.Type() == parser.ValueTypeMatrix) && alwaysEmpty(a, empty) {
				return true
			}
		}
		return false
	case *parser.BinaryExpr:
		l, r := alwaysEmpty(n.LHS, empty), alwaysEmpty(n.RHS, empty)
		switch n.Op {
		case parser.LOR:
			return l && r
		case parser.LUNLESS:
			return l
		default:
			lv, rv := n.LHS.Type() == parser.ValueTypeVector, n.RHS.Type() == parser.ValueTypeVector
			switch {
			case lv && rv:
				return l || r
			case lv:
				return l
			case rv:
				return r
			default:
				return false
			}
		}
	default:
		return false
	}
}

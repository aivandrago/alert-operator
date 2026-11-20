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

package analyze

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

// fakeSeries answers count(last_over_time(<selector>[...])) queries: a selector has series
// when its text contains one of the live metric names with a matching label.
type fakeSeries struct {
	live    []string // substrings of selectors that have series
	asked   []string
	budget  int // queries allowed; <0 unlimited
	failure error
}

func (f *fakeSeries) Query(_ context.Context, q string, _ time.Time) ([]promapi.Sample, error) {
	if f.failure != nil {
		return nil, f.failure
	}
	if f.budget == 0 {
		return nil, promapi.ErrBudgetExhausted
	}
	f.budget--
	f.asked = append(f.asked, q)
	for _, l := range f.live {
		if strings.Contains(q, l) {
			return []promapi.Sample{{Value: 3}}, nil
		}
	}
	return nil, nil
}

func check(t *testing.T, f *fakeSeries, expr string) (string, error) {
	t.Helper()
	fd, err := NewDeadQueryChecker(f, time.Now()).Check(context.Background(), expr)
	if fd == nil {
		return "", err
	}
	return fd.Message, err
}

func TestDeadQuery(t *testing.T) {
	live := []string{"http_server_requests_seconds_count", `up{job="payments"}`, "kube_pod_info", "node_load1"}
	cases := []struct {
		name, expr string
		dead       []string // selectors the message must name; nil means not dead
	}{
		{"alive", `rate(http_server_requests_seconds_count[5m]) > 1`, nil},
		{"renamed metric", `rate(http_requests_total[5m]) > 1`, []string{"http_requests_total[5m]"}},
		{"wrong label value", `up{job="payment"} == 0`, []string{`up{job="payment"}`}},
		{"right label value", `up{job="payments"} == 0`, nil},
		{"absent is exempt", `absent(up{job="gone"})`, nil},
		{"absent_over_time is exempt", `absent_over_time(up{job="gone"}[10m])`, nil},
		{"and: one dead side kills it", `node_load1 > 4 and on() missing_metric`, []string{"missing_metric"}},
		{"arithmetic: one dead side kills it", `node_load1 / missing_cpus > 2`, []string{"missing_cpus"}},
		{"or: one dead side is fine", `node_load1 > 4 or missing_metric > 1`, nil},
		{"or: both dead", `missing_a > 1 or missing_b > 1`, []string{"missing_a", "missing_b"}},
		{"unless: dead right side is fine", `node_load1 > 4 unless on() maintenance_mode`, nil},
		{"unless: dead left side", `missing_a > 1 unless on() node_load1`, []string{"missing_a"}},
		{"aggregation of dead", `sum by (ns) (missing_a) > 0`, []string{"missing_a"}},
		{"scalar function is not dead", `node_load1 > scalar(missing_a)`, nil},
		{"subquery", `max_over_time(rate(missing_a[5m])[1h:1m]) > 0`, []string{"missing_a[5m]"}},
		{"offset", `node_load1 offset 1h > 2`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := check(t, &fakeSeries{live: live, budget: -1}, tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			if tc.dead == nil {
				if msg != "" {
					t.Errorf("reported dead: %s", msg)
				}
				return
			}
			if msg == "" {
				t.Fatalf("not reported dead")
			}
			for _, d := range tc.dead {
				if !strings.Contains(msg, d) {
					t.Errorf("message %q does not name %s", msg, d)
				}
			}
		})
	}
}

func TestDeadQueryQueriesAreCheapAndCached(t *testing.T) {
	f := &fakeSeries{live: []string{"node_load1"}, budget: -1}
	c := NewDeadQueryChecker(f, time.Now())
	for range 3 {
		if _, err := c.Check(context.Background(), `node_load1 > 1 and node_load1 offset 1h > 1`); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.asked) != 2 {
		t.Errorf("want 2 queries (one per distinct selector, cached), got %v", f.asked)
	}
	for _, q := range f.asked {
		if !strings.HasPrefix(q, "count(last_over_time(") {
			t.Errorf("not a count query: %s", q)
		}
	}
	if !strings.Contains(f.asked[1], "node_load1[1h] offset 1h") {
		t.Errorf("offset lost: %s", f.asked[1])
	}
	if strings.Contains(strings.Join(f.asked, " "), "absent") {
		t.Error("absent() arguments must not be queried")
	}
}

func TestDeadQueryParseError(t *testing.T) {
	_, err := check(t, &fakeSeries{budget: -1}, `rate(up[5m]`)
	if _, ok := errors.AsType[*ParseError](err); !ok {
		t.Errorf("want ParseError, got %v", err)
	}
}

func TestDeadQueryBudgetAndErrors(t *testing.T) {
	_, err := check(t, &fakeSeries{budget: 1}, `missing_a > 1 and missing_b > 1`)
	if !errors.Is(err, promapi.ErrBudgetExhausted) {
		t.Errorf("want budget exhausted, got %v", err)
	}
	down := &promapi.UnreachableError{Target: "prometheus", Err: errors.New("connection refused")}
	if _, err := check(t, &fakeSeries{failure: down}, `up == 0`); !promapi.IsUnreachable(err) {
		t.Errorf("want unreachable, got %v", err)
	}
}

func TestStepAndIntervals(t *testing.T) {
	if s := Step(30*24*time.Hour, 30*time.Second); s != 5*time.Minute {
		t.Errorf("30d step: %v", s)
	}
	if s := Step(24*time.Hour, 2*time.Minute); s != 2*time.Minute {
		t.Errorf("interval above a minute: %v", s)
	}
	t0 := time.Unix(0, 0)
	pts := []promapi.Point{{Time: t0.Add(time.Minute), Value: 1}, {Time: t0.Add(2 * time.Minute), Value: 2},
		{Time: t0.Add(5 * time.Minute), Value: 1}, {Time: t0.Add(6 * time.Minute), Value: 0}}
	runs := runsOf(pts, time.Minute)
	if len(runs) != 2 || runs[0].end.Sub(runs[0].start) != 2*time.Minute || runs[1].end.Sub(runs[1].start) != time.Minute {
		t.Errorf("runs: %+v", runs)
	}
	if u := unionLength([]interval{{t0, t0.Add(10 * time.Minute)}, {t0.Add(5 * time.Minute), t0.Add(12 * time.Minute)},
		{t0.Add(20 * time.Minute), t0.Add(21 * time.Minute)}}); u != 13*time.Minute {
		t.Errorf("union: %v", u)
	}
	if median([]time.Duration{1, 3, 2, 4}) != 2 || median(nil) != 0 {
		t.Error("median")
	}
	if EffectiveWindow(30*24*time.Hour, 0, false) != 30*24*time.Hour || EffectiveWindow(time.Hour, 48*time.Hour, true) != time.Hour {
		t.Error("effective window")
	}
}

func TestFlappingNeedsSeveralActivations(t *testing.T) {
	s := (&alertsv1alpha1.AlertScopeSpec{}).Settings()
	in := VerdictInput{Settings: s, Window: time.Hour}
	// One short activation in an hour is 24 a day, but it is not flapping.
	one := Measured{Activations: 1, PerDay: 24, Runs: []time.Duration{time.Minute}, Median: time.Minute}
	if len(Verdicts(one, in)) != 0 {
		t.Errorf("one activation judged: %v", Verdicts(one, in))
	}
	three := Measured{Activations: 3, PerDay: 72, Runs: []time.Duration{time.Minute}, Median: 20 * time.Second}
	if v := Verdicts(three, in); len(v) != 1 || v[0].Type != alertsv1alpha1.FindingFlapping {
		t.Errorf("three short activations: %v", v)
	}
}

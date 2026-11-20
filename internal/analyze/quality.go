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
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

// RangeQuerier runs instant and range queries; *promapi.Prometheus is one.
type RangeQuerier interface {
	Querier
	QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]promapi.Series, error)
}

// RuleRef is one rule to measure.
type RuleRef struct {
	// Key identifies the rule in the result (its identity).
	Key string
	// Alert is the alert name.
	Alert string
	// Labels are the rule's labels. Those without a template ({{ ... }}) are on every alert
	// of the rule and tell apart rules with the same alert name.
	Labels map[string]string
	// Interval is the rule group's evaluation interval.
	Interval time.Duration
}

// Measured is a rule's behaviour over the window.
type Measured struct {
	// Activations counts the times the alert became active (pending or firing).
	Activations int
	// PerDay is Activations per day of the window.
	PerDay float64
	// Runs are the lengths of the firing periods seen.
	Runs []time.Duration
	// Median is the typical firing duration: the median of Runs, or, when there were more
	// activations than the query's resolution could tell apart (several activations inside one
	// step), the firing time divided by the activations. Zero without runs.
	Median time.Duration
	// Firing is the time any alert of the rule was firing.
	Firing time.Duration
	// FiringFraction is Firing divided by the window.
	FiringFraction float64
	// SilencedFraction is the share of the firing time (summed over the rule's alerts) covered
	// by a matching silence.
	SilencedFraction float64
	// LastFired is the last time the alert was seen firing; zero if never in the window.
	LastFired time.Time
}

// Quality measures rules from Prometheus' history: ALERTS_FOR_STATE for activations, ALERTS
// for firing periods, and the silences Alertmanager holds.
type Quality struct {
	P        RangeQuerier
	Silences []promapi.Silence
	Now      time.Time
	// Window is the effective window: already capped to the retention.
	Window time.Duration
}

// MaxPointsPerSeries keeps range queries under Prometheus' limit of 11,000 points per series.
const MaxPointsPerSeries = 10000

// Step returns the resolution of the firing-period query for a window and an evaluation
// interval: at least a minute and the interval, enough to stay under MaxPointsPerSeries,
// rounded up to whole minutes.
func Step(window, interval time.Duration) time.Duration {
	step := max(time.Minute, interval, window/MaxPointsPerSeries)
	return ((step + time.Minute - 1) / time.Minute) * time.Minute
}

// MeasureGroup measures the rules of one rule group with two queries: the activations of all
// its alert names and their firing periods. Series are assigned to rules by alert name and the
// rule's untemplated labels.
func (q *Quality) MeasureGroup(ctx context.Context, rules []RuleRef) (map[string]Measured, error) {
	out := map[string]Measured{}
	if len(rules) == 0 {
		return out, nil
	}
	names := make([]string, 0, len(rules))
	var evalInterval time.Duration
	for _, r := range rules {
		if !slices.Contains(names, r.Alert) {
			names = append(names, r.Alert)
		}
		evalInterval = max(evalInterval, r.Interval)
	}
	slices.Sort(names)
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	match := strconv.Quote(strings.Join(quoted, "|"))
	win := promDuration(q.Window)

	activations, err := q.P.Query(ctx,
		fmt.Sprintf("changes(ALERTS_FOR_STATE{alertname=~%s}[%s])", match, win), q.Now)
	if err != nil {
		return nil, err
	}
	step := Step(q.Window, evalInterval)
	firing, err := q.P.QueryRange(ctx,
		fmt.Sprintf(`count_over_time(ALERTS{alertstate="firing",alertname=~%s}[%s])`, match, promDuration(step)),
		q.Now.Add(-q.Window).Add(step), q.Now, step)
	if err != nil {
		return nil, err
	}

	days := q.Window.Hours() / 24
	for _, r := range rules {
		m := Measured{}
		for _, s := range activations {
			if belongs(s.Metric, r) {
				// changes() counts the activations after the first sample in the window; the
				// first one is the series itself.
				m.Activations += int(s.Value) + 1
			}
		}
		var union []interval
		var silenced, firingSum time.Duration
		for _, s := range firing {
			if !belongs(s.Metric, r) {
				continue
			}
			runs := runsOf(s.Points, step)
			for _, run := range runs {
				m.Runs = append(m.Runs, run.end.Sub(run.start))
				firingSum += run.end.Sub(run.start)
				silenced += q.silencedDuring(s.Metric, run)
				if run.end.After(m.LastFired) {
					m.LastFired = run.end
				}
			}
			union = append(union, runs...)
		}
		m.Firing = unionLength(union)
		if days > 0 {
			m.PerDay = float64(m.Activations) / days
		}
		if q.Window > 0 {
			m.FiringFraction = min(1, m.Firing.Seconds()/q.Window.Seconds())
		}
		if firingSum > 0 {
			m.SilencedFraction = min(1, silenced.Seconds()/firingSum.Seconds())
		}
		m.Median = median(m.Runs)
		if m.Activations > len(m.Runs) && m.Activations > 0 && m.Firing > 0 {
			m.Median = m.Firing / time.Duration(m.Activations)
		}
		if m.LastFired.After(q.Now) {
			m.LastFired = q.Now
		}
		out[r.Key] = m
	}
	return out, nil
}

// belongs reports whether an alert series is one of the rule's: same alert name, and every
// untemplated rule label equal.
func belongs(metric map[string]string, r RuleRef) bool {
	if metric["alertname"] != r.Alert {
		return false
	}
	for k, v := range r.Labels {
		if strings.Contains(v, "{{") {
			continue
		}
		if metric[k] != v {
			return false
		}
	}
	return true
}

type interval struct{ start, end time.Time }

// runsOf turns count_over_time points (one per step, present while firing) into firing
// periods: consecutive points one step apart are one period, which started a step before its
// first point.
func runsOf(points []promapi.Point, step time.Duration) []interval {
	var out []interval
	for _, p := range points {
		if p.Value <= 0 {
			continue
		}
		if n := len(out); n > 0 && p.Time.Sub(out[n-1].end) <= step {
			out[n-1].end = p.Time
			continue
		}
		out = append(out, interval{start: p.Time.Add(-step), end: p.Time})
	}
	return out
}

func unionLength(in []interval) time.Duration {
	if len(in) == 0 {
		return 0
	}
	slices.SortFunc(in, func(a, b interval) int { return a.start.Compare(b.start) })
	var total time.Duration
	cur := in[0]
	for _, iv := range in[1:] {
		if !iv.start.After(cur.end) {
			if iv.end.After(cur.end) {
				cur.end = iv.end
			}
			continue
		}
		total += cur.end.Sub(cur.start)
		cur = iv
	}
	return total + cur.end.Sub(cur.start)
}

// silencedDuring returns how much of a firing period of one alert matching silences covered.
func (q *Quality) silencedDuring(labels map[string]string, run interval) time.Duration {
	var cover []interval
	for i := range q.Silences {
		s := &q.Silences[i]
		if !s.Matches(labels) {
			continue
		}
		start, end := maxTime(s.StartsAt, run.start), minTime(s.EndsAt, run.end)
		if end.After(start) {
			cover = append(cover, interval{start, end})
		}
	}
	return unionLength(cover)
}

func median(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// promDuration writes a duration the way PromQL reads it: whole seconds at least.
func promDuration(d time.Duration) string {
	return string(alertsv1alpha1.FormatDuration(max(d.Round(time.Second), time.Second)))
}

// MinFlappingActivations: fewer activations than this are never Flapping, whatever the rate.
// A single short activation in a short window would otherwise extrapolate to many per day.
const MinFlappingActivations = 3

// VerdictInput is what the quality verdicts depend on besides the measurement.
type VerdictInput struct {
	Settings alertsv1alpha1.Settings
	// Known is how long the operator has known the rule (now minus status.firstSeen).
	Known time.Duration
	// Silences says whether an Alertmanager is configured, so AlwaysSilenced can be judged.
	Silences bool
	Window   time.Duration
}

// Verdicts returns the quality findings of a measured rule: Noisy, Flapping, NeverFired,
// AlwaysSilenced.
func Verdicts(m Measured, in VerdictInput) []alertsv1alpha1.Finding {
	var out []alertsv1alpha1.Finding
	add := func(t alertsv1alpha1.FindingType, format string, args ...any) {
		out = append(out, alertsv1alpha1.Finding{Type: t, Severity: alertsv1alpha1.SeverityOf(t),
			Message: fmt.Sprintf(format, args...)})
	}
	win := alertsv1alpha1.FormatDuration(in.Window)
	s := in.Settings
	if m.Activations > s.NoisyFiringsAbove {
		add(alertsv1alpha1.FindingNoisy, "%d activations in %s (threshold %d); median firing time %s",
			m.Activations, win, s.NoisyFiringsAbove, alertsv1alpha1.FormatDuration(m.Median))
	}
	if m.Activations >= MinFlappingActivations && m.PerDay > s.FlappingPerDayAbove && len(m.Runs) > 0 &&
		m.Median < s.FlappingMedianBelow {
		add(alertsv1alpha1.FindingFlapping, "%.1f activations per day (threshold %g) with a median firing time "+
			"of %s (threshold %s)", m.PerDay, s.FlappingPerDayAbove, alertsv1alpha1.FormatDuration(m.Median),
			alertsv1alpha1.FormatDuration(s.FlappingMedianBelow))
	}
	if m.Activations == 0 && len(m.Runs) == 0 && in.Known >= s.NeverFiredAfter {
		add(alertsv1alpha1.FindingNeverFired, "no activation in %s", win)
	}
	if in.Silences && m.Firing > 0 && m.SilencedFraction > s.SilencedFractionAbove {
		add(alertsv1alpha1.FindingAlwaysSilenced, "silenced for %.0f%% of its firing time in %s (threshold %.0f%%)",
			m.SilencedFraction*100, win, s.SilencedFractionAbove*100)
	}
	return out
}

// EffectiveWindow is the configured window capped to the retention, when it is known.
func EffectiveWindow(window, retention time.Duration, known bool) time.Duration {
	if known && retention > 0 && retention < window {
		return retention
	}
	return window
}

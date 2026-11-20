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

package metrics

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
)

// RuleSample is what the metrics say about one rule.
type RuleSample struct {
	Namespace string
	Team      string
	Alert     string
	Loaded    bool
	Findings  []alertsv1alpha1.Finding
	// Measured is false until the rule's quality has been measured once.
	Measured       bool
	Activations    int
	FiringFraction float64
}

// ScopeSnapshot is the state of one scope after a sync.
type ScopeSnapshot struct {
	Rules           []RuleSample
	EffectiveWindow time.Duration
	LastSync        time.Time
}

var (
	descRules = prometheus.NewDesc("hs_alert_rules",
		"Alerting rules in the scope, by team, namespace and whether Prometheus loaded them.",
		[]string{lScope, lTeam, lNamespace, "loaded"}, nil)
	descFindings = prometheus.NewDesc("hs_alert_rule_findings",
		"Active findings: 1 per rule and finding (more only when rules share namespace, team and alert name).",
		[]string{lScope, lNamespace, lTeam, lAlert, "finding", "severity"}, nil)
	descActivations = prometheus.NewDesc("hs_alert_rule_activations",
		"Activations of the rule's alert in the effective window.",
		[]string{lScope, lNamespace, lTeam, lAlert}, nil)
	descFiringFraction = prometheus.NewDesc("hs_alert_rule_firing_fraction",
		"Share of the effective window the rule's alert was firing.",
		[]string{lScope, lNamespace, lTeam, lAlert}, nil)
	descWindow = prometheus.NewDesc("hs_alert_scope_effective_window_seconds",
		"History window the verdicts are based on: the configured window capped to the retention.",
		[]string{lScope}, nil)
	descLastSync = prometheus.NewDesc("hs_alert_scope_last_sync_timestamp_seconds",
		"Time of the scope's last complete sync.",
		[]string{lScope}, nil)
)

// SyncDuration is the time a scope sync takes.
var SyncDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "hs_alert_scope_sync_duration_seconds",
	Help:    "Duration of a scope sync: reading Prometheus and Alertmanager, judging and mirroring the rules.",
	Buckets: []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
}, []string{lScope})

// Collector serves the per-rule and per-scope gauges from the last snapshot of each scope. A
// scrape sees either the previous sync or the new one, never a mix, and a rule or finding that
// is gone is gone from the next scrape: there are no stale series to clean up.
type Collector struct {
	mu     sync.RWMutex
	scopes map[string]ScopeSnapshot
}

// NewCollector returns an empty collector.
func NewCollector() *Collector { return &Collector{scopes: map[string]ScopeSnapshot{}} }

// Scopes is the collector registered with the manager's registry.
var Scopes = NewCollector()

func init() {
	registry().MustRegister(Scopes, SyncDuration)
}

// Set replaces the snapshot of a scope.
func (c *Collector) Set(scope string, s ScopeSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scopes[scope] = s
}

// Delete forgets a scope, for when the AlertScope is deleted.
func (c *Collector) Delete(scope string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.scopes, scope)
	SyncDuration.DeleteLabelValues(scope)
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descRules, descFindings, descActivations, descFiringFraction, descWindow, descLastSync} {
		ch <- d
	}
}

type ruleKey struct{ namespace, team, alert string }

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for scope, s := range c.scopes {
		type countKey struct{ team, namespace, loaded string }
		counts := map[countKey]int{}
		type findingKey struct {
			ruleKey
			finding, severity string
		}
		findings := map[findingKey]int{}
		activations := map[ruleKey]int{}
		firing := map[ruleKey]float64{}
		for _, r := range s.Rules {
			counts[countKey{r.Team, r.Namespace, strconv.FormatBool(r.Loaded)}]++
			k := ruleKey{r.Namespace, r.Team, r.Alert}
			seen := map[alertsv1alpha1.FindingType]bool{}
			for _, f := range r.Findings {
				if seen[f.Type] {
					continue
				}
				seen[f.Type] = true
				findings[findingKey{k, string(f.Type), string(f.Severity)}]++
			}
			if r.Measured {
				activations[k] += r.Activations
				firing[k] = max(firing[k], r.FiringFraction)
			}
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(descRules, prometheus.GaugeValue, float64(n), scope, k.team, k.namespace, k.loaded)
		}
		for k, n := range findings {
			ch <- prometheus.MustNewConstMetric(descFindings, prometheus.GaugeValue, float64(n),
				scope, k.namespace, k.team, k.alert, k.finding, k.severity)
		}
		for k, n := range activations {
			ch <- prometheus.MustNewConstMetric(descActivations, prometheus.GaugeValue, float64(n), scope, k.namespace, k.team, k.alert)
		}
		for k, v := range firing {
			ch <- prometheus.MustNewConstMetric(descFiringFraction, prometheus.GaugeValue, v, scope, k.namespace, k.team, k.alert)
		}
		if s.EffectiveWindow > 0 {
			ch <- prometheus.MustNewConstMetric(descWindow, prometheus.GaugeValue, s.EffectiveWindow.Seconds(), scope)
		}
		if !s.LastSync.IsZero() {
			ch <- prometheus.MustNewConstMetric(descLastSync, prometheus.GaugeValue,
				float64(s.LastSync.UnixMilli())/1000, scope)
		}
	}
}

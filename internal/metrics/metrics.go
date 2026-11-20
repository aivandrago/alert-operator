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

// Package metrics defines the operator's Prometheus metrics, all prefixed hs_alert_
// (docs/metrics.md), on the controller-runtime registry the manager serves.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Label names shared by several metrics.
const (
	lScope     = "scope"
	lNamespace = "namespace"
	lTeam      = "team"
	lAlert     = "alertname"
)

var (
	// QueryTotal counts queries to Prometheus and Alertmanager by result: ok, error (a query
	// refused), unreachable, unauthorized, budget_exhausted.
	QueryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hs_alert_query_total",
		Help: "Queries sent to Prometheus and Alertmanager, by target and result.",
	}, []string{lScope, "target", "result"})

	// BudgetExhaustedTotal counts the times work was deferred to the next sync because the
	// scope's query budget ran out.
	BudgetExhaustedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hs_alert_query_budget_exhausted_total",
		Help: "Times a query was deferred to the next sync because the scope's query budget ran out.",
	}, []string{lScope})
)

func registry() prometheus.Registerer { return ctrlmetrics.Registry }

func init() {
	registry().MustRegister(QueryTotal, BudgetExhaustedTotal)
}

// ScopeRecorder records the queries of one scope.
type ScopeRecorder string

// Query counts one query.
func (s ScopeRecorder) Query(target, result string) {
	QueryTotal.WithLabelValues(string(s), target, result).Inc()
}

// BudgetExhausted counts one deferral.
func (s ScopeRecorder) BudgetExhausted() {
	BudgetExhaustedTotal.WithLabelValues(string(s)).Inc()
}

// SilenceWritesTotal counts the operator's writes to Alertmanager (v0.3).
var SilenceWritesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hs_alert_silence_writes_total",
	Help: "Writes to Alertmanager for AlertSilence objects, by action (create, update, expire) and result.",
}, []string{lScope, "action", "result"})

func init() {
	registry().MustRegister(SilenceWritesTotal, Silences)
}

// SilenceWrite counts one write.
func SilenceWrite(scope, action string, ok bool) {
	result := "ok"
	if !ok {
		result = "error"
	}
	SilenceWritesTotal.WithLabelValues(scope, action, result).Inc()
}

// SilenceCollector serves hs_alert_silences{scope,state} from the last state of each AlertSilence.
type SilenceCollector struct {
	mu    sync.Mutex
	state map[string][2]string // namespace/name -> scope, state
}

// Silences is the collector of AlertSilence states.
var Silences = &SilenceCollector{state: map[string][2]string{}}

var descSilences = prometheus.NewDesc("hs_alert_silences", "AlertSilence objects by scope and state.",
	[]string{lScope, "state"}, nil)

// Set records the state of one AlertSilence.
func (c *SilenceCollector) Set(key, scope, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state[key] = [2]string{scope, state}
}

// Delete forgets one AlertSilence.
func (c *SilenceCollector) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.state, key)
}

// Describe implements prometheus.Collector.
func (c *SilenceCollector) Describe(ch chan<- *prometheus.Desc) { ch <- descSilences }

// Collect implements prometheus.Collector.
func (c *SilenceCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts := map[[2]string]int{}
	for _, v := range c.state {
		counts[v]++
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(descSilences, prometheus.GaugeValue, float64(n), k[0], k[1])
	}
}

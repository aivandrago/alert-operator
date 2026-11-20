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

// Package webhook is the validating admission webhook for PrometheusRule objects
// (docs/design.md §10): the checks of internal/lint at the door, in warn or enforce mode per
// namespace. It answers admission reviews and writes nothing.
package webhook

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/analyze"
	"hypersurgery.dev/alert-operator/internal/lint"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

// Path is where the webhook is served.
const Path = "/validate-monitoring-coreos-com-v1-prometheusrule"

// LabelMode is the namespace label that picks the mode.
const LabelMode = "alerts.hypersurgery.dev/validation"

// Modes.
const (
	ModeEnforce = "enforce"
	ModeWarn    = "warn"
	ModeOff     = "off"
)

// Defaults of the review budget (docs/design.md §10.4).
const (
	DefaultQueryBudget = 2 * time.Second
	DefaultCacheTTL    = 5 * time.Minute
	// maxWarning keeps each admission warning short; the API server drops very long ones.
	maxWarning = 250
)

var (
	reviews = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hs_alert_webhook_reviews_total",
		Help: "PrometheusRule admission reviews, by mode and result (allowed, warned, denied).",
	}, []string{"mode", "result"})
	reviewDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "hs_alert_webhook_review_duration_seconds",
		Help:    "Time to answer a PrometheusRule admission review.",
		Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5},
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(reviews, reviewDuration)
}

// Validator handles PrometheusRule admission reviews.
type Validator struct {
	// Client reads namespaces, AlertScopes and Prometheus objects, from the manager's cache.
	Client client.Reader
	// APIReader reads the auth Secrets of a scope's Prometheus, uncached.
	APIReader client.Reader
	Decoder   admission.Decoder
	// OperatorNamespace holds the auth Secrets.
	OperatorNamespace string
	// DefaultMode applies to namespaces without the mode label: warn or enforce.
	DefaultMode string
	// QueryBudget bounds the time a review spends asking Prometheus; zero means the default.
	QueryBudget time.Duration
	// CacheTTL is how long a selector's answer is reused; zero means the default.
	CacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	samples []promapi.Sample
	at      time.Time
}

// Handle answers one review.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	start := time.Now()
	defer func() { reviewDuration.Observe(time.Since(start).Seconds()) }()
	log := logf.FromContext(ctx).WithValues("prometheusrule", req.Namespace+"/"+req.Name)

	pr := &monitoringv1.PrometheusRule{}
	if err := v.Decoder.Decode(req, pr); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if pr.Namespace == "" {
		pr.Namespace = req.Namespace
	}
	ns := &corev1.Namespace{}
	if err := v.Client.Get(ctx, types.NamespacedName{Name: pr.Namespace}, ns); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to read Namespace; admitting the object unchecked")
		reviews.WithLabelValues("unknown", "allowed").Inc()
		return admission.Allowed("").WithWarnings("alert-operator: not checked: " + err.Error())
	}
	mode := ns.Labels[LabelMode]
	if mode != ModeEnforce && mode != ModeWarn && mode != ModeOff {
		mode = v.DefaultMode
	}
	if mode == ModeOff {
		reviews.WithLabelValues(mode, "allowed").Inc()
		return admission.Allowed("")
	}

	in, notes, err := v.input(ctx, pr, ns.Labels)
	if err != nil {
		log.Error(err, "Failed to read the AlertScopes; admitting the object unchecked")
		reviews.WithLabelValues(mode, "allowed").Inc()
		return admission.Allowed("").WithWarnings("alert-operator: not checked: " + err.Error())
	}
	budget := v.QueryBudget
	if budget <= 0 {
		budget = DefaultQueryBudget
	}
	checkCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	res := lint.Check(checkCtx, in)

	var warnings, blocking []string
	for _, p := range res.Problems {
		line := clip("alert-operator: " + p.String())
		if p.Blocking && mode == ModeEnforce {
			blocking = append(blocking, p.String())
			continue
		}
		warnings = append(warnings, line)
	}
	for _, s := range append(notes, res.Skipped...) {
		warnings = append(warnings, clip("alert-operator: not checked: "+s))
	}
	if len(blocking) > 0 {
		reviews.WithLabelValues(mode, "denied").Inc()
		msg := fmt.Sprintf("alert-operator (namespace %s enforces %s): %s", pr.Namespace, LabelMode, strings.Join(blocking, "; "))
		return admission.Denied(msg).WithWarnings(warnings...)
	}
	result := "allowed"
	if len(res.Problems) > 0 {
		result = "warned"
	}
	reviews.WithLabelValues(mode, result).Inc()
	return admission.Allowed("").WithWarnings(warnings...)
}

// input gathers the governing scope's settings, its Prometheus object and its Prometheus.
func (v *Validator) input(ctx context.Context, pr *monitoringv1.PrometheusRule,
	nsLabels map[string]string) (lint.Input, []string, error) {
	in := lint.Input{Rule: pr, NamespaceLabels: nsLabels, Now: time.Now()}
	scope, err := v.scopeFor(ctx, nsLabels)
	if err != nil {
		return in, nil, err
	}
	if scope == nil {
		in.Settings = (&alertsv1alpha1.AlertScopeSpec{}).Settings()
		return in, []string{"no AlertScope selects this namespace; default ownership rules, no Prometheus checks"}, nil
	}
	in.Settings = scope.Spec.Settings()
	var notes []string
	if ref := scope.Spec.Prometheus.ResourceRef; ref != nil {
		p := &monitoringv1.Prometheus{}
		switch err := v.Client.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, p); {
		case err == nil:
			in.Prometheus = p
		case apierrors.IsNotFound(err):
			notes = append(notes, fmt.Sprintf("NotLoaded: Prometheus %s/%s named by AlertScope %s not found",
				ref.Namespace, ref.Name, scope.Name))
		default:
			return in, nil, err
		}
	}
	q, err := v.querier(ctx, scope)
	if err != nil {
		notes = append(notes, "DeadQuery: "+err.Error())
	} else {
		in.Querier = q
	}
	return in, notes, nil
}

// scopeFor returns the first AlertScope, by name, whose namespaceSelector selects the namespace.
func (v *Validator) scopeFor(ctx context.Context, nsLabels map[string]string) (*alertsv1alpha1.AlertScope, error) {
	list := &alertsv1alpha1.AlertScopeList{}
	if err := v.Client.List(ctx, list); err != nil {
		return nil, err
	}
	slices.SortFunc(list.Items, func(a, b alertsv1alpha1.AlertScope) int { return strings.Compare(a.Name, b.Name) })
	for i := range list.Items {
		s := &list.Items[i]
		sel := labels.Everything()
		if s.Spec.NamespaceSelector != nil {
			var err error
			if sel, err = metav1.LabelSelectorAsSelector(s.Spec.NamespaceSelector); err != nil {
				continue
			}
		}
		if sel.Matches(labels.Set(nsLabels)) {
			return s, nil
		}
	}
	return nil, nil
}

func (v *Validator) querier(ctx context.Context, scope *alertsv1alpha1.AlertScope) (analyze.Querier, error) {
	auth := promapi.Auth{}
	if ref := scope.Spec.Prometheus.AuthSecretRef; ref != nil {
		s := &corev1.Secret{}
		if err := v.APIReader.Get(ctx, types.NamespacedName{Namespace: v.OperatorNamespace, Name: ref.Name}, s); err != nil {
			return nil, fmt.Errorf("reading Secret %s/%s: %w", v.OperatorNamespace, ref.Name, err)
		}
		a, err := promapi.AuthFromSecret(s)
		if err != nil {
			return nil, err
		}
		auth = a
	}
	settings := scope.Spec.Settings()
	p, err := promapi.NewPrometheus(scope.Spec.Prometheus.URL, auth,
		promapi.NewBudget(settings.QueryTimeout, settings.MaxConcurrent, settings.MaxQueriesPerMinute, nil))
	if err != nil {
		return nil, err
	}
	return &cachingQuerier{v: v, key: scope.Spec.Prometheus.URL, next: p}, nil
}

// cachingQuerier reuses a selector's answer for CacheTTL: a rollout applying the same rules to
// many namespaces asks Prometheus once.
type cachingQuerier struct {
	v    *Validator
	key  string
	next analyze.Querier
}

func (c *cachingQuerier) Query(ctx context.Context, q string, ts time.Time) ([]promapi.Sample, error) {
	ttl := c.v.CacheTTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	key := c.key + "\x00" + q
	c.v.mu.Lock()
	if e, ok := c.v.cache[key]; ok && time.Since(e.at) < ttl {
		c.v.mu.Unlock()
		return e.samples, nil
	}
	c.v.mu.Unlock()
	res, err := c.next.Query(ctx, q, ts)
	if err != nil {
		return nil, err
	}
	c.v.mu.Lock()
	defer c.v.mu.Unlock()
	if c.v.cache == nil {
		c.v.cache = map[string]cached{}
	}
	if len(c.v.cache) > 10000 {
		for k, e := range c.v.cache {
			if time.Since(e.at) >= ttl {
				delete(c.v.cache, k)
			}
		}
	}
	c.v.cache[key] = cached{samples: res, at: time.Now()}
	return res, nil
}

func clip(s string) string {
	if len(s) <= maxWarning {
		return s
	}
	return s[:maxWarning-3] + "..."
}

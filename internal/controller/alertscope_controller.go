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

// Package controller holds the AlertScope reconciler: it reads Prometheus, Alertmanager and the
// PrometheusRule objects of a scope, judges every alerting rule and mirrors the result into
// AlertRule objects (docs/design.md §5).
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/analyze"
	"hypersurgery.dev/alert-operator/internal/inventory"
	"hypersurgery.dev/alert-operator/internal/metrics"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

// Ready condition reasons of an AlertScope.
const (
	ReasonSynced                  = "Synced"
	ReasonPrometheusUnreachable   = "PrometheusUnreachable"
	ReasonAlertmanagerUnreachable = "AlertmanagerUnreachable"
	ReasonBudgetExhausted         = "BudgetExhausted"
	ReasonConflict                = "Conflict"
	ReasonInvalidSpec             = "InvalidSpec"
)

// RetryInterval is how soon a scope is synced again after a failure or while rules wait to be
// loaded.
const RetryInterval = time.Minute

// AlertScopeReconciler reconciles AlertScope objects.
type AlertScopeReconciler struct {
	client.Client
	// APIReader reads Secrets without a cache (and so without a cluster-wide watch on them).
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	// OperatorNamespace holds the auth Secrets and the mirrors of file-loaded rules.
	OperatorNamespace string
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu    sync.Mutex
	state map[string]*scopeState
}

// scopeState is what the reconciler keeps between syncs of one scope.
type scopeState struct {
	budget       *promapi.Budget
	lastAnalysis time.Time
	generation   int64
}

// +kubebuilder:rbac:groups=alerts.hypersurgery.dev,resources=alertscopes,verbs=get;list;watch
// +kubebuilder:rbac:groups=alerts.hypersurgery.dev,resources=alertscopes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=alerts.hypersurgery.dev,resources=alertrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=alerts.hypersurgery.dev,resources=alertrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules;prometheuses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",namespace=alert-operator-system,resources=secrets,verbs=get

func (r *AlertScopeReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *AlertScopeReconciler) scopeState(name string) *scopeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		r.state = map[string]*scopeState{}
	}
	s := r.state[name]
	if s == nil {
		s = &scopeState{}
		r.state[name] = s
	}
	return s
}

func (r *AlertScopeReconciler) forget(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.state, name)
}

// Reconcile syncs one scope.
func (r *AlertScopeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	scope := &alertsv1alpha1.AlertScope{}
	if err := r.Get(ctx, req.NamespacedName, scope); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.scopeDeleted(ctx, req.Name)
		}
		return ctrl.Result{}, err
	}
	start := r.now()
	res, err := r.sync(ctx, scope, start)
	metrics.SyncDuration.WithLabelValues(scope.Name).Observe(time.Since(start).Seconds())
	if err != nil {
		log.Error(err, "Failed to sync AlertScope", "scope", scope.Name)
	}
	return res, err
}

// scopeDeleted removes the mirrors of a deleted scope: objects carrying its scope label and the
// operator's managed-by label, nothing else.
func (r *AlertScopeReconciler) scopeDeleted(ctx context.Context, name string) error {
	r.forget(name)
	metrics.Scopes.Delete(name)
	list := &alertsv1alpha1.AlertRuleList{}
	if err := r.List(ctx, list, client.MatchingLabels{
		alertsv1alpha1.LabelManagedBy: alertsv1alpha1.ManagedByValue, alertsv1alpha1.LabelScope: name,
	}); err != nil {
		return err
	}
	for i := range list.Items {
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	if len(list.Items) > 0 {
		logf.FromContext(ctx).Info("Deleted AlertRules of a deleted AlertScope", "scope", name, "count", len(list.Items))
	}
	return nil
}

// syncResult collects what goes into the scope status.
type syncResult struct {
	conflicts    []string
	window       alertsv1alpha1.Duration
	budgetHit    bool
	amDown       error
	pending      bool
	analysisDone bool
}

func readyFalse(reason, msg string) metav1.Condition {
	return metav1.Condition{Type: alertsv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: msg}
}

func (r *AlertScopeReconciler) sync(ctx context.Context, scope *alertsv1alpha1.AlertScope, now time.Time) (ctrl.Result, error) {
	settings := scope.Spec.Settings()
	st := r.scopeState(scope.Name)
	if st.budget == nil {
		st.budget = promapi.NewBudget(settings.QueryTimeout, settings.MaxConcurrent, settings.MaxQueriesPerMinute,
			metrics.ScopeRecorder(scope.Name))
	} else {
		st.budget.Update(settings.QueryTimeout, settings.MaxConcurrent, settings.MaxQueriesPerMinute)
	}
	// The work of one sync must end well before the next: what the budget cannot afford by
	// then waits for the next sync.
	syncCtx, cancel := context.WithTimeout(ctx, min(settings.ResyncInterval/2, 10*time.Minute))
	defer cancel()

	prom, am, err := r.clients(ctx, scope, st.budget)
	if err != nil {
		return r.failed(ctx, scope, readyFalse(ReasonInvalidSpec, err.Error()))
	}
	groups, err := prom.AlertingRules(syncCtx)
	if err != nil {
		reason := ReasonPrometheusUnreachable
		if errors.Is(err, promapi.ErrBudgetExhausted) {
			reason = ReasonBudgetExhausted
		}
		// Nothing is deleted or rewritten: the verdicts stay as they were and go stale.
		return r.failed(ctx, scope, readyFalse(reason, "reading /api/v1/rules: "+err.Error()))
	}

	in, err := r.inventoryInput(ctx, scope, groups, settings, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	inv := inventory.Build(in)

	existing := &alertsv1alpha1.AlertRuleList{}
	if err := r.List(ctx, existing, client.MatchingLabels{
		alertsv1alpha1.LabelManagedBy: alertsv1alpha1.ManagedByValue, alertsv1alpha1.LabelScope: scope.Name,
	}); err != nil {
		return ctrl.Result{}, err
	}
	prev := map[string]*alertsv1alpha1.AlertRule{}
	for i := range existing.Items {
		prev[existing.Items[i].Namespace+"/"+existing.Items[i].Name] = &existing.Items[i]
	}

	res := syncResult{window: scope.Status.EffectiveWindow}
	var analyses map[string]*analysis
	if now.Sub(st.lastAnalysis) >= settings.ResyncInterval || st.generation != scope.Generation {
		analyses, err = r.analyse(syncCtx, prom, am, inv.Entries, prev, settings, now, &res)
		if err != nil {
			reason := ReasonPrometheusUnreachable
			return r.failed(ctx, scope, readyFalse(reason, err.Error()))
		}
		st.lastAnalysis, st.generation = now, scope.Generation
		res.analysisDone = true
	}

	statuses, wanted, err := r.mirror(ctx, scope, inv.Entries, prev, analyses, now, &res)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Deletion only now: Prometheus answered and every list succeeded, so a rule missing from
	// the inventory is really gone.
	for key, ar := range prev {
		if wanted[key] || !ours(ar, scope.Name) {
			continue
		}
		if err := r.Delete(ctx, ar); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
	}

	r.publish(scope.Name, inv.Entries, statuses, res, now)
	if err := r.writeScopeStatus(ctx, scope, statuses, res, now); err != nil {
		return ctrl.Result{}, err
	}
	next := settings.ResyncInterval - now.Sub(st.lastAnalysis)
	if res.pending || res.budgetHit || res.amDown != nil {
		next = min(next, RetryInterval)
	}
	return ctrl.Result{RequeueAfter: max(next, time.Second)}, nil
}

// failed records a failed sync on the scope and retries soon. Mirrors are left as they are.
func (r *AlertScopeReconciler) failed(ctx context.Context, scope *alertsv1alpha1.AlertScope, cond metav1.Condition) (ctrl.Result, error) {
	patch := client.MergeFrom(scope.DeepCopy())
	cond.ObservedGeneration = scope.Generation
	meta.SetStatusCondition(&scope.Status.Conditions, cond)
	scope.Status.ObservedGeneration = scope.Generation
	if err := r.Status().Patch(ctx, scope, patch); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("AlertScope not synced", "scope", scope.Name, "reason", cond.Reason, "message", cond.Message)
	return ctrl.Result{RequeueAfter: RetryInterval}, nil
}

func (r *AlertScopeReconciler) clients(ctx context.Context, scope *alertsv1alpha1.AlertScope,
	budget *promapi.Budget) (*promapi.Prometheus, *promapi.Alertmanager, error) {
	auth := func(ref *alertsv1alpha1.LocalSecretReference) (promapi.Auth, error) {
		if ref == nil {
			return promapi.Auth{}, nil
		}
		s := &corev1.Secret{}
		if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: r.OperatorNamespace, Name: ref.Name}, s); err != nil {
			return promapi.Auth{}, fmt.Errorf("reading Secret %s/%s: %w", r.OperatorNamespace, ref.Name, err)
		}
		return promapi.AuthFromSecret(s)
	}
	pa, err := auth(scope.Spec.Prometheus.AuthSecretRef)
	if err != nil {
		return nil, nil, err
	}
	prom, err := promapi.NewPrometheus(scope.Spec.Prometheus.URL, pa, budget)
	if err != nil {
		return nil, nil, err
	}
	if scope.Spec.Alertmanager == nil {
		return prom, nil, nil
	}
	aa, err := auth(scope.Spec.Alertmanager.AuthSecretRef)
	if err != nil {
		return nil, nil, err
	}
	am, err := promapi.NewAlertmanager(scope.Spec.Alertmanager.URL, aa, budget)
	if err != nil {
		return nil, nil, err
	}
	return prom, am, nil
}

func (r *AlertScopeReconciler) inventoryInput(ctx context.Context, scope *alertsv1alpha1.AlertScope,
	groups []promapi.RuleGroup, settings alertsv1alpha1.Settings, now time.Time) (inventory.Input, error) {
	rules := &monitoringv1.PrometheusRuleList{}
	if err := r.List(ctx, rules); err != nil {
		return inventory.Input{}, fmt.Errorf("listing PrometheusRules: %w", err)
	}
	nsList := &corev1.NamespaceList{}
	if err := r.List(ctx, nsList); err != nil {
		return inventory.Input{}, fmt.Errorf("listing namespaces: %w", err)
	}
	namespaces := map[string]map[string]string{}
	for _, ns := range nsList.Items {
		namespaces[ns.Name] = ns.Labels
	}
	sel := labels.Everything()
	if scope.Spec.NamespaceSelector != nil {
		s, err := metav1.LabelSelectorAsSelector(scope.Spec.NamespaceSelector)
		if err != nil {
			return inventory.Input{}, fmt.Errorf("spec.namespaceSelector: %w", err)
		}
		sel = s
	}
	in := inventory.Input{
		Scope:      scope.Name,
		Rules:      rules.Items,
		Namespaces: namespaces,
		InScope: func(ns string) bool {
			l, ok := namespaces[ns]
			return ok && sel.Matches(labels.Set(l))
		},
		Loaded:   groups,
		Settings: settings,
		Now:      now,
	}
	if ref := scope.Spec.Prometheus.ResourceRef; ref != nil {
		p := &monitoringv1.Prometheus{}
		err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, p)
		switch {
		case err == nil:
			in.Prometheus = p
		case apierrors.IsNotFound(err):
			logf.FromContext(ctx).Info("Prometheus named by resourceRef not found", "scope", scope.Name,
				"prometheus", ref.Namespace+"/"+ref.Name)
		default:
			return inventory.Input{}, err
		}
	}
	return in, nil
}

// groupKey groups entries the way Prometheus evaluates them: by rule group.
func groupKey(e *inventory.Entry) string {
	if e.Source.Kind == alertsv1alpha1.SourceKindFile {
		return "file:" + e.Source.File + "/" + e.Source.Group
	}
	return e.Source.Namespace + "/" + e.Source.Name + "/" + e.Source.Group
}

// analyse runs DeadQuery and the quality measurement for the loaded rules. A query the budget
// cannot afford keeps that rule's previous verdict; an unreachable Prometheus fails the sync.
func (r *AlertScopeReconciler) analyse(ctx context.Context,
	prom *promapi.Prometheus, am *promapi.Alertmanager, entries []inventory.Entry,
	prev map[string]*alertsv1alpha1.AlertRule, settings alertsv1alpha1.Settings, now time.Time,
	res *syncResult) (map[string]*analysis, error) {
	retention, known, err := prom.Retention(ctx)
	if err != nil {
		if promapi.IsUnreachable(err) {
			return nil, fmt.Errorf("reading retention: %w", err)
		}
		known = false
	}
	window := analyze.EffectiveWindow(settings.Window, retention, known)
	res.window = alertsv1alpha1.FormatDuration(window)

	var silences []promapi.Silence
	haveSilences := false
	if am != nil {
		silences, err = am.Silences(ctx)
		if err != nil {
			res.amDown = err
			if errors.Is(err, promapi.ErrBudgetExhausted) {
				res.budgetHit = true
			}
		} else {
			haveSilences = true
		}
	}

	out := map[string]*analysis{}
	dq := analyze.NewDeadQueryChecker(prom, now)
	groups := map[string][]*inventory.Entry{}
	var order []string
	for i := range entries {
		e := &entries[i]
		if !e.Loaded {
			continue
		}
		a := &analysis{complete: true}
		out[e.Identity] = a
		if !haveSilences && am != nil {
			a.complete = false
			a.kept = append(a.kept, alertsv1alpha1.FindingAlwaysSilenced)
		}
		f, err := dq.Check(ctx, e.Expr)
		parseErr, isParseErr := errors.AsType[*analyze.ParseError](err)
		switch {
		case isParseErr:
			a.findings = append(a.findings, alertsv1alpha1.Finding{Type: alertsv1alpha1.FindingLoadError,
				Severity: alertsv1alpha1.SeverityCritical, Message: parseErr.Error()})
		case errors.Is(err, promapi.ErrBudgetExhausted):
			res.budgetHit = true
			a.complete = false
			a.kept = append(a.kept, alertsv1alpha1.FindingDeadQuery, alertsv1alpha1.FindingLoadError)
		case err != nil && promapi.IsUnreachable(err):
			return nil, err
		case err != nil:
			// A selector query Prometheus refused: no verdict on this rule this time.
			a.complete = false
			a.kept = append(a.kept, alertsv1alpha1.FindingDeadQuery)
		case f != nil:
			a.findings = append(a.findings, *f)
		}
		k := groupKey(e)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}

	q := &analyze.Quality{P: prom, Silences: silences, Now: now, Window: window}
	qualityKinds := []alertsv1alpha1.FindingType{alertsv1alpha1.FindingNoisy, alertsv1alpha1.FindingFlapping,
		alertsv1alpha1.FindingNeverFired, alertsv1alpha1.FindingAlwaysSilenced}
	for _, k := range order {
		refs := make([]analyze.RuleRef, 0, len(groups[k]))
		for _, e := range groups[k] {
			refs = append(refs, analyze.RuleRef{Key: e.Identity, Alert: e.Source.Alert, Labels: e.Labels, Interval: e.Interval})
		}
		measured, err := q.MeasureGroup(ctx, refs)
		if err != nil {
			if promapi.IsUnreachable(err) {
				return nil, err
			}
			if errors.Is(err, promapi.ErrBudgetExhausted) {
				res.budgetHit = true
			}
			for _, e := range groups[k] {
				a := out[e.Identity]
				a.complete = false
				a.kept = append(a.kept, qualityKinds...)
			}
			continue
		}
		for _, e := range groups[k] {
			m := measured[e.Identity]
			a := out[e.Identity]
			firstSeen := now
			if p := prev[mirrorKey(e, r.OperatorNamespace)]; p != nil && p.Status.FirstSeen != nil {
				firstSeen = p.Status.FirstSeen.Time
			}
			a.measured = true
			a.quality = toQuality(m)
			a.findings = append(a.findings, analyze.Verdicts(m, analyze.VerdictInput{
				Settings: settings, Known: now.Sub(firstSeen), Silences: haveSilences, Window: window,
			})...)
		}
	}
	return out, nil
}

func toQuality(m analyze.Measured) alertsv1alpha1.RuleQuality {
	q := alertsv1alpha1.RuleQuality{
		Activations:       int32(min(m.Activations, 1<<31-1)),
		ActivationsPerDay: round(m.PerDay, 2),
		FiringFraction:    round(m.FiringFraction, 4),
		SilencedFraction:  round(m.SilencedFraction, 4),
	}
	if m.Median > 0 {
		r := time.Minute
		if m.Median < 10*time.Minute {
			r = time.Second
		}
		q.MedianFiringDuration = alertsv1alpha1.FormatDuration(m.Median.Round(r))
	}
	if !m.LastFired.IsZero() {
		q.LastFired = &metav1.Time{Time: m.LastFired.Truncate(time.Second)}
	}
	return q
}

func round(v float64, digits int) float64 {
	p := 1.0
	for range digits {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

func mirrorKey(e *inventory.Entry, operatorNamespace string) string {
	return mirrorNamespace(e, operatorNamespace) + "/" + MirrorName(e.Source.Alert, e.Identity)
}

// mirror creates or updates the AlertRule of every entry and writes its status. It returns the
// statuses (for the scope summary) and the set of mirrors that should exist.
func (r *AlertScopeReconciler) mirror(ctx context.Context, scope *alertsv1alpha1.AlertScope, entries []inventory.Entry,
	prev map[string]*alertsv1alpha1.AlertRule, analyses map[string]*analysis, now time.Time,
	res *syncResult) ([]alertsv1alpha1.AlertRuleStatus, map[string]bool, error) {
	log := logf.FromContext(ctx)
	wanted := map[string]bool{}
	current := map[string]bool{}
	for i := range entries {
		current[entries[i].Identity] = true
	}
	statuses := make([]alertsv1alpha1.AlertRuleStatus, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Pending {
			res.pending = true
		}
		desired := desiredMirror(scope.Name, e, r.OperatorNamespace)
		key := desired.Namespace + "/" + desired.Name
		old := prev[key]
		if old == nil {
			// Not one of ours under that name; anything else there is somebody else's.
			current := &alertsv1alpha1.AlertRule{}
			err := r.Get(ctx, client.ObjectKeyFromObject(desired), current)
			switch {
			case err == nil:
				res.conflicts = append(res.conflicts, key)
				continue
			case !apierrors.IsNotFound(err):
				return nil, nil, err
			}
		} else if old.Status.Identity != "" && old.Status.Identity != e.Identity && current[old.Status.Identity] {
			// Two rules whose names and identity prefixes collide: the first one keeps the name.
			res.conflicts = append(res.conflicts, key)
			continue
		}
		wanted[key] = true
		if err := r.applySpec(ctx, desired); err != nil {
			if apierrors.IsNotFound(err) || apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
				// The namespace is going away; the rule goes with it.
				continue
			}
			return nil, nil, fmt.Errorf("applying AlertRule %s: %w", key, err)
		}
		var prevStatus alertsv1alpha1.AlertRuleStatus
		if old != nil {
			prevStatus = old.Status
		}
		next := ruleStatus(prevStatus, e, analyses[e.Identity], res.window, now)
		statuses = append(statuses, next)
		if statusChanged(prevStatus, next, now) {
			obj := desired.DeepCopy()
			obj.Status = next
			if err := r.applyStatus(ctx, obj); err != nil {
				return nil, nil, fmt.Errorf("writing status of AlertRule %s: %w", key, err)
			}
		}
		if old != nil {
			r.recordTransitions(desired, e.Source.Alert, prevStatus.Findings, next.Findings)
		}
	}
	if len(res.conflicts) > 0 {
		log.Info("AlertRule names taken by objects the operator does not own", "scope", scope.Name, "conflicts", res.conflicts)
	}
	return statuses, wanted, nil
}

func toApply(obj *alertsv1alpha1.AlertRule, withStatus bool) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "metadata", "resourceVersion")
	if withStatus {
		unstructured.RemoveNestedField(u.Object, "spec")
		u.SetLabels(nil)
		u.SetOwnerReferences(nil)
	} else {
		unstructured.RemoveNestedField(u.Object, "status")
	}
	return u, nil
}

func (r *AlertScopeReconciler) applySpec(ctx context.Context, obj *alertsv1alpha1.AlertRule) error {
	u, err := toApply(obj, false)
	if err != nil {
		return err
	}
	return r.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner), client.ForceOwnership)
}

func (r *AlertScopeReconciler) applyStatus(ctx context.Context, obj *alertsv1alpha1.AlertRule) error {
	u, err := toApply(obj, true)
	if err != nil {
		return err
	}
	return r.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner),
		client.ForceOwnership)
}

// recordTransitions emits one Event per finding gained or lost.
func (r *AlertScopeReconciler) recordTransitions(obj *alertsv1alpha1.AlertRule, alert string, prev, next []alertsv1alpha1.Finding) {
	if r.Recorder == nil {
		return
	}
	gained, lost := transitions(prev, next)
	for _, f := range gained {
		kind := corev1.EventTypeNormal
		if f.Severity != alertsv1alpha1.SeverityInfo {
			kind = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(obj, nil, kind, "FindingAdded", "Judge", "%s became %s: %s", alert, f.Type, f.Message)
	}
	for _, f := range lost {
		r.Recorder.Eventf(obj, nil, corev1.EventTypeNormal, "FindingCleared", "Judge", "%s is no longer %s", alert, f.Type)
	}
}

func (r *AlertScopeReconciler) publish(scope string, entries []inventory.Entry, statuses []alertsv1alpha1.AlertRuleStatus,
	res syncResult, now time.Time) {
	byID := map[string]*inventory.Entry{}
	for i := range entries {
		byID[entries[i].Identity] = &entries[i]
	}
	snap := metrics.ScopeSnapshot{LastSync: now}
	if w, err := res.window.Parse(); err == nil {
		snap.EffectiveWindow = w
	}
	for _, st := range statuses {
		e := byID[st.Identity]
		if e == nil {
			continue
		}
		snap.Rules = append(snap.Rules, metrics.RuleSample{
			Namespace: e.Source.Namespace, Team: st.Team, Alert: e.Source.Alert, Loaded: st.Loaded,
			Findings: st.Findings, Measured: st.LastEvaluated != nil,
			Activations: int(st.Quality.Activations), FiringFraction: st.Quality.FiringFraction,
		})
	}
	metrics.Scopes.Set(scope, snap)
}

func (r *AlertScopeReconciler) writeScopeStatus(ctx context.Context, scope *alertsv1alpha1.AlertScope,
	statuses []alertsv1alpha1.AlertRuleStatus, res syncResult, now time.Time) error {
	patch := client.MergeFrom(scope.DeepCopy())
	counts, byType, total, teams := aggregate(statuses)
	s := &scope.Status
	s.ObservedGeneration = scope.Generation
	s.LastSync = &metav1.Time{Time: now}
	s.EffectiveWindow = res.window
	s.Rules = counts
	s.Findings = byType
	s.FindingsTotal = total
	s.Teams = teams
	slices.Sort(res.conflicts)
	if len(res.conflicts) > 100 {
		res.conflicts = res.conflicts[:100]
	}
	s.Conflicts = res.conflicts
	cond := metav1.Condition{Type: alertsv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonSynced,
		Message: fmt.Sprintf("%d rules, %d findings", counts.Total, total)}
	switch {
	case res.amDown != nil && !errors.Is(res.amDown, promapi.ErrBudgetExhausted):
		cond = readyFalse(ReasonAlertmanagerUnreachable, "reading silences: "+res.amDown.Error()+
			"; AlwaysSilenced keeps its previous verdicts")
	case res.budgetHit:
		cond = readyFalse(ReasonBudgetExhausted, "the query budget ran out; the rest is measured in the next sync")
	case len(res.conflicts) > 0:
		cond = readyFalse(ReasonConflict, fmt.Sprintf("%d AlertRule names are taken by objects the operator does not own",
			len(res.conflicts)))
	}
	cond.ObservedGeneration = scope.Generation
	meta.SetStatusCondition(&s.Conditions, cond)
	return r.Status().Patch(ctx, scope, patch)
}

// scopeOfMirror maps an AlertRule the operator owns to its scope.
func scopeOfMirror(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if l[alertsv1alpha1.LabelManagedBy] != alertsv1alpha1.ManagedByValue || l[alertsv1alpha1.LabelScope] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: l[alertsv1alpha1.LabelScope]}}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *AlertScopeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	allScopes := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		list := &alertsv1alpha1.AlertScopeList{}
		if err := mgr.GetClient().List(ctx, list); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for _, s := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: s.Name}})
		}
		return reqs
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&alertsv1alpha1.AlertScope{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&monitoringv1.PrometheusRule{}, allScopes, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{}))).
		Watches(&monitoringv1.Prometheus{}, allScopes, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Namespace{}, allScopes, builder.WithPredicates(predicate.LabelChangedPredicate{})).
		// A scope deleted while no replica was leader is never reconciled from its own events:
		// the AlertRules that carry its label bring it back to the queue, at start-up (the
		// informer's first list arrives as creates) and whenever one is created.
		Watches(&alertsv1alpha1.AlertRule{}, handler.EnqueueRequestsFromMapFunc(scopeOfMirror),
			builder.WithPredicates(predicate.Funcs{
				UpdateFunc:  func(event.UpdateEvent) bool { return false },
				DeleteFunc:  func(event.DeleteEvent) bool { return false },
				GenericFunc: func(event.GenericEvent) bool { return false },
			})).
		Named("alertscope").
		Complete(r)
}

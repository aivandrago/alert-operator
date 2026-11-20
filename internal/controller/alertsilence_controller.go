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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/metrics"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

// Timings of the silence controller (docs/design.md §11).
const (
	// DeleteRetry is how long deleting an AlertSilence waits for Alertmanager before it lets go
	// of the object; the silence still ends at its expiry.
	DeleteRetry = 10 * time.Minute
	// silenceRefresh is how often an Active or Pending AlertSilence is checked again.
	silenceRefresh = 5 * time.Minute
)

// AlertSilenceReconciler reconciles AlertSilence objects: it creates, updates and expires the
// silence in the scope's Alertmanager, only for scopes in Apply mode and only with EnableWrites.
type AlertSilenceReconciler struct {
	client.Client
	APIReader         client.Reader
	Scheme            *runtime.Scheme
	Recorder          events.EventRecorder
	OperatorNamespace string
	// EnableWrites is --enable-writes. Without it every AlertSilence is only proposed.
	EnableWrites bool
	Now          func() time.Time
	// Audit receives one line per write; logf's "audit" logger when unset.
	Audit *logr.Logger
}

// +kubebuilder:rbac:groups=alerts.hypersurgery.dev,resources=alertsilences,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=alerts.hypersurgery.dev,resources=alertsilences/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=alerts.hypersurgery.dev,resources=alertsilences/finalizers,verbs=update

func (r *AlertSilenceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *AlertSilenceReconciler) audit() logr.Logger {
	if r.Audit != nil {
		return *r.Audit
	}
	return logf.Log.WithName("audit")
}

// Reconcile brings one AlertSilence and its silence in step.
func (r *AlertSilenceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := &alertsv1alpha1.AlertSilence{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.Silences.Delete(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !obj.DeletionTimestamp.IsZero() {
		return r.deleted(ctx, obj)
	}
	before := obj.Status.DeepCopy()
	res, err := r.sync(ctx, obj)
	if err != nil {
		return res, err
	}
	obj.Status.ObservedGeneration = obj.Generation
	metrics.Silences.Set(req.String(), obj.Spec.ScopeRef, string(obj.Status.State))
	if !equality.Semantic.DeepEqual(before, &obj.Status) {
		if err := r.Status().Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}
	return res, nil
}

func (r *AlertSilenceReconciler) setState(obj *alertsv1alpha1.AlertSilence, state alertsv1alpha1.SilenceState, msg string) {
	obj.Status.State = state
	obj.Status.Message = msg
	cond := metav1.Condition{Type: alertsv1alpha1.ConditionReady, Reason: string(state), Message: msg,
		ObservedGeneration: obj.Generation, Status: metav1.ConditionFalse}
	if state == alertsv1alpha1.SilenceActive || state == alertsv1alpha1.SilencePending ||
		state == alertsv1alpha1.SilenceProposed || state == alertsv1alpha1.SilenceExpired {
		cond.Status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&obj.Status.Conditions, cond)
}

func (r *AlertSilenceReconciler) reject(obj *alertsv1alpha1.AlertSilence, msg string) (ctrl.Result, error) {
	if obj.Status.State != alertsv1alpha1.SilenceRejected || obj.Status.Message != msg {
		r.event(obj, corev1.EventTypeWarning, "SilenceRejected", "Reject", "%s", msg)
	}
	r.setState(obj, alertsv1alpha1.SilenceRejected, msg)
	return ctrl.Result{}, nil
}

func (r *AlertSilenceReconciler) event(obj *alertsv1alpha1.AlertSilence, kind, reason, action, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, kind, reason, action, format, args...)
	}
}

// window returns when the silence starts and ends.
func window(obj *alertsv1alpha1.AlertSilence) (time.Time, time.Time, error) {
	start := obj.CreationTimestamp.Time
	if obj.Spec.StartsAt != nil {
		start = obj.Spec.StartsAt.Time
	}
	if obj.Spec.ExpiresAt != nil {
		return start, obj.Spec.ExpiresAt.Time, nil
	}
	d, err := obj.Spec.Duration.Parse()
	if err != nil {
		return start, start, err
	}
	return start, start.Add(d), nil
}

func toMatchers(in []alertsv1alpha1.SilenceMatcher) []promapi.Matcher {
	out := make([]promapi.Matcher, 0, len(in))
	for _, m := range in {
		eq := m.IsEqual == nil || *m.IsEqual
		out = append(out, promapi.Matcher{Name: m.Name, Value: m.Value, IsRegex: m.IsRegex, IsEqual: &eq})
	}
	return out
}

var alertName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// validate checks the object against the scope's policy (docs/design.md §11.2) and returns
// why it is refused, or "".
func (r *AlertSilenceReconciler) validate(ctx context.Context, obj *alertsv1alpha1.AlertSilence,
	s alertsv1alpha1.Settings, start, end time.Time) (string, error) {
	if !end.After(start) {
		return "the silence ends before it starts", nil
	}
	if d := end.Sub(start); d > s.SilenceMaxDuration {
		return fmt.Sprintf("the silence lasts %s, longer than silences.maxDuration of AlertScope %s (%s)",
			alertsv1alpha1.FormatDuration(d.Round(time.Minute)), obj.Spec.ScopeRef,
			alertsv1alpha1.FormatDuration(s.SilenceMaxDuration)), nil
	}
	selective := false
	var names []string
	for _, m := range obj.Spec.Matchers {
		eq := m.IsEqual == nil || *m.IsEqual
		var re *regexp.Regexp
		if m.IsRegex {
			var err error
			if re, err = regexp.Compile("^(?:" + m.Value + ")$"); err != nil {
				return fmt.Sprintf("matcher %s: not a valid regular expression: %v", m.Name, err), nil
			}
		}
		matchesAll := m.Value == "" || (re != nil && re.MatchString("") && re.MatchString("zz-any-9"))
		if eq && !matchesAll {
			selective = true
		}
	}
	if !selective {
		return "the matchers match every alert; at least one positive matcher must select something", nil
	}
	for _, m := range obj.Spec.Matchers {
		if m.Name == "alertname" && (m.IsEqual == nil || *m.IsEqual) {
			if !m.IsRegex {
				names = append(names, m.Value)
				continue
			}
			for alt := range strings.SplitSeq(m.Value, "|") {
				if !alertName.MatchString(alt) {
					return "the alertname regex must be a list of alert names (A|B|C) so the alerts it silences are known", nil
				}
				names = append(names, alt)
			}
		}
	}
	if !s.RestrictSilences {
		return "", nil
	}
	if len(names) == 0 {
		return "an alertname matcher is required: silences.restrictToNamespaceRules allows only alerts of this namespace's rules", nil
	}
	rules := &alertsv1alpha1.AlertRuleList{}
	if err := r.List(ctx, rules, client.MatchingLabels{
		alertsv1alpha1.LabelManagedBy: alertsv1alpha1.ManagedByValue, alertsv1alpha1.LabelScope: obj.Spec.ScopeRef,
	}); err != nil {
		return "", err
	}
	where := map[string][]string{}
	for _, ar := range rules.Items {
		ns := ar.Namespace
		if !slices.Contains(where[ar.Spec.Source.Alert], ns) {
			where[ar.Spec.Source.Alert] = append(where[ar.Spec.Source.Alert], ns)
		}
	}
	for _, n := range names {
		nss := where[n]
		switch {
		case !slices.Contains(nss, obj.Namespace):
			return fmt.Sprintf("alert %s is not a rule of namespace %s (silences.restrictToNamespaceRules)", n, obj.Namespace), nil
		case len(nss) > 1:
			slices.Sort(nss)
			return fmt.Sprintf("alert %s is a rule of several namespaces (%s); a silence on it would silence them all",
				n, strings.Join(nss, ", ")), nil
		}
	}
	return "", nil
}

func specHash(p *alertsv1alpha1.ProposedSilence) string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

type silenceClients struct {
	reader *promapi.Alertmanager
	writer *promapi.SilenceWriter
}

func (r *AlertSilenceReconciler) clients(ctx context.Context, scope *alertsv1alpha1.AlertScope, write bool) (silenceClients, error) {
	var c silenceClients
	if scope.Spec.Alertmanager == nil {
		return c, fmt.Errorf("AlertScope %s has no spec.alertmanager", scope.Name)
	}
	auth := promapi.Auth{}
	if ref := scope.Spec.Alertmanager.AuthSecretRef; ref != nil {
		sec := &corev1.Secret{}
		if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: r.OperatorNamespace, Name: ref.Name}, sec); err != nil {
			return c, fmt.Errorf("reading Secret %s/%s: %w", r.OperatorNamespace, ref.Name, err)
		}
		a, err := promapi.AuthFromSecret(sec)
		if err != nil {
			return c, err
		}
		auth = a
	}
	settings := scope.Spec.Settings()
	budget := promapi.NewBudget(settings.QueryTimeout, 2, 600, metrics.ScopeRecorder(scope.Name))
	var err error
	if c.reader, err = promapi.NewAlertmanager(scope.Spec.Alertmanager.URL, auth, budget); err != nil {
		return c, err
	}
	if write {
		if c.writer, err = promapi.NewSilenceWriter(scope.Spec.Alertmanager.URL, auth, budget); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (r *AlertSilenceReconciler) sync(ctx context.Context, obj *alertsv1alpha1.AlertSilence) (ctrl.Result, error) {
	scope := &alertsv1alpha1.AlertScope{}
	if err := r.Get(ctx, types.NamespacedName{Name: obj.Spec.ScopeRef}, scope); err != nil {
		if apierrors.IsNotFound(err) {
			return r.reject(obj, fmt.Sprintf("AlertScope %s not found", obj.Spec.ScopeRef))
		}
		return ctrl.Result{}, err
	}
	s := scope.Spec.Settings()
	if s.SilenceMode == alertsv1alpha1.SilenceModeOff {
		r.setState(obj, alertsv1alpha1.SilenceDisabled, fmt.Sprintf("silences.mode of AlertScope %s is Off", scope.Name))
		return ctrl.Result{}, nil
	}
	start, end, err := window(obj)
	if err != nil {
		return r.reject(obj, err.Error())
	}
	why, err := r.validate(ctx, obj, s, start, end)
	if err != nil {
		return ctrl.Result{}, err
	}
	if why != "" {
		return r.reject(obj, why)
	}

	marker := promapi.Marker(obj.Namespace, obj.Name, string(obj.UID))
	proposed := &alertsv1alpha1.ProposedSilence{
		Matchers: obj.Spec.Matchers, StartsAt: metav1.NewTime(start.UTC()), EndsAt: metav1.NewTime(end.UTC()),
		CreatedBy: promapi.CreatedBy,
		Comment:   fmt.Sprintf("%s owner: %s; reason: %s", marker, obj.Spec.Owner, obj.Spec.Reason),
	}
	obj.Status.Proposed = proposed
	obj.Status.StartsAt, obj.Status.EndsAt = &proposed.StartsAt, &proposed.EndsAt

	propose := ""
	switch {
	case s.SilenceMode == alertsv1alpha1.SilenceModePropose:
		propose = fmt.Sprintf("silences.mode of AlertScope %s is Propose", scope.Name)
	case obj.Spec.DryRun:
		propose = "spec.dryRun is set"
	case !r.EnableWrites:
		propose = "the operator runs without --enable-writes"
	}
	c, err := r.clients(ctx, scope, propose == "")
	if err != nil {
		return r.reject(obj, err.Error())
	}
	if alerts, err := c.reader.Alerts(ctx); err == nil {
		n := 0
		matchers := toMatchers(obj.Spec.Matchers)
		for _, a := range alerts {
			if promapi.Matching(matchers, a.Labels) {
				n++
			}
		}
		obj.Status.MatchedAlerts = int32(n)
	}
	if propose != "" {
		r.setState(obj, alertsv1alpha1.SilenceProposed, "only proposed: "+propose+"; nothing is written to Alertmanager")
		return ctrl.Result{RequeueAfter: silenceRefresh}, nil
	}
	return r.apply(ctx, obj, scope, c, proposed, marker, start, end)
}

func (r *AlertSilenceReconciler) apply(ctx context.Context, obj *alertsv1alpha1.AlertSilence, scope *alertsv1alpha1.AlertScope,
	c silenceClients, proposed *alertsv1alpha1.ProposedSilence, marker string, start, end time.Time) (ctrl.Result, error) {
	now := r.now()
	hash := specHash(proposed)
	if !controllerutil.ContainsFinalizer(obj, alertsv1alpha1.FinalizerSilence) {
		// The patch returns the object as stored, status included: keep what this sync computed.
		status := obj.Status.DeepCopy()
		patch := client.MergeFrom(obj.DeepCopy())
		controllerutil.AddFinalizer(obj, alertsv1alpha1.FinalizerSilence)
		if err := r.Patch(ctx, obj, patch); err != nil {
			return ctrl.Result{}, err
		}
		obj.Status = *status
	}

	if obj.Status.SilenceID != "" {
		current, err := c.reader.Silence(ctx, obj.Status.SilenceID)
		switch {
		case err != nil && !errors.Is(err, promapi.ErrNotFound):
			return r.unreachable(obj, err)
		case errors.Is(err, promapi.ErrNotFound):
			// Alertmanager lost it (a restart without storage): that is no one's decision, so
			// the silence is created again below.
		case obj.Status.SpecHash == hash && current.Status.State == "expired":
			if !end.After(now) {
				r.setState(obj, alertsv1alpha1.SilenceExpired, "the silence has ended")
				return ctrl.Result{}, nil
			}
			if obj.Status.State != alertsv1alpha1.SilenceExpiredExternally {
				r.event(obj, corev1.EventTypeWarning, "SilenceExpiredExternally", "Observe",
					"Silence %s was ended in Alertmanager before %s; it is not recreated. Change the spec to ask for a new one.",
					obj.Status.SilenceID, end.UTC().Format(time.RFC3339))
			}
			r.setState(obj, alertsv1alpha1.SilenceExpiredExternally,
				"the silence was ended in Alertmanager before its expiry; change the spec to ask for a new one")
			return ctrl.Result{}, nil
		case obj.Status.SpecHash == hash:
			return r.settle(obj, now, start, end)
		}
		// The spec changed: update the silence, or create a new one if the old one is gone.
		if err == nil && current.Status.State != "expired" && obj.Status.SpecHash != hash {
			return r.write(ctx, obj, scope, c, proposed, marker, hash, obj.Status.SilenceID, now, start, end)
		}
	}
	if !end.After(now) {
		r.setState(obj, alertsv1alpha1.SilenceExpired, "the silence would already have ended; nothing was created")
		return ctrl.Result{}, nil
	}
	return r.write(ctx, obj, scope, c, proposed, marker, hash, "", now, start, end)
}

func (r *AlertSilenceReconciler) write(ctx context.Context, obj *alertsv1alpha1.AlertSilence, scope *alertsv1alpha1.AlertScope,
	c silenceClients, proposed *alertsv1alpha1.ProposedSilence, marker, hash, id string,
	now, start, end time.Time) (ctrl.Result, error) {
	action := "create"
	if id != "" {
		action = "update"
	}
	newID, err := c.writer.CreateOrUpdateSilence(ctx, promapi.PostableSilence{
		ID: id, Matchers: toMatchers(proposed.Matchers), StartsAt: start, EndsAt: end,
		CreatedBy: proposed.CreatedBy, Comment: proposed.Comment,
	}, marker)
	r.auditWrite(obj, scope.Name, action, newID, end, err)
	if err != nil {
		if errors.Is(err, promapi.ErrNotOurs) {
			return r.reject(obj, "silence "+id+" in Alertmanager is not this object's: it is left alone")
		}
		if promapi.IsUnreachable(err) || errors.Is(err, promapi.ErrBudgetExhausted) {
			return r.unreachable(obj, err)
		}
		return r.reject(obj, "Alertmanager refused the silence: "+err.Error())
	}
	if action == "create" {
		r.event(obj, corev1.EventTypeNormal, "SilenceCreated", "Create", "Created silence %s until %s",
			newID, end.UTC().Format(time.RFC3339))
	} else {
		r.event(obj, corev1.EventTypeNormal, "SilenceUpdated", "Update", "Updated silence %s (now %s) until %s",
			id, newID, end.UTC().Format(time.RFC3339))
	}
	obj.Status.SilenceID = newID
	obj.Status.SpecHash = hash
	return r.settle(obj, now, start, end)
}

// settle sets Pending, Active or Expired from the clock and requeues for the next change.
func (r *AlertSilenceReconciler) settle(obj *alertsv1alpha1.AlertSilence, now, start, end time.Time) (ctrl.Result, error) {
	switch {
	case !end.After(now):
		r.setState(obj, alertsv1alpha1.SilenceExpired, "the silence has ended")
		return ctrl.Result{}, nil
	case obj.Spec.StartsAt != nil && start.After(now):
		r.setState(obj, alertsv1alpha1.SilencePending, "the silence starts at "+start.UTC().Format(time.RFC3339))
		return ctrl.Result{RequeueAfter: min(start.Sub(now)+time.Second, silenceRefresh)}, nil
	default:
		r.setState(obj, alertsv1alpha1.SilenceActive, "silence "+obj.Status.SilenceID+" is active")
		return ctrl.Result{RequeueAfter: min(end.Sub(now)+time.Second, silenceRefresh)}, nil
	}
}

func (r *AlertSilenceReconciler) unreachable(obj *alertsv1alpha1.AlertSilence, err error) (ctrl.Result, error) {
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{Type: alertsv1alpha1.ConditionReady,
		Status: metav1.ConditionFalse, Reason: ReasonAlertmanagerUnreachable, Message: err.Error(),
		ObservedGeneration: obj.Generation})
	return ctrl.Result{RequeueAfter: RetryInterval}, nil
}

func (r *AlertSilenceReconciler) auditWrite(obj *alertsv1alpha1.AlertSilence, scope, action, id string, end time.Time, err error) {
	result := "ok"
	if err != nil {
		result = err.Error()
	}
	metrics.SilenceWrite(scope, action, err == nil)
	r.audit().Info("Alertmanager silence write", "action", action, "alertsilence", obj.Namespace+"/"+obj.Name,
		"uid", string(obj.UID), "scope", scope, "silenceID", id, "matchers", obj.Spec.Matchers,
		"endsAt", end.UTC().Format(time.RFC3339), "owner", obj.Spec.Owner, "result", result)
}

// deleted expires the silence of an AlertSilence being deleted, then lets go of the object.
func (r *AlertSilenceReconciler) deleted(ctx context.Context, obj *alertsv1alpha1.AlertSilence) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(obj, alertsv1alpha1.FinalizerSilence) {
		return ctrl.Result{}, nil
	}
	release := func(kind, format string, args ...any) (ctrl.Result, error) {
		if format != "" {
			r.event(obj, kind, "SilenceReleased", "Delete", format, args...)
		}
		patch := client.MergeFrom(obj.DeepCopy())
		controllerutil.RemoveFinalizer(obj, alertsv1alpha1.FinalizerSilence)
		metrics.Silences.Delete(obj.Namespace + "/" + obj.Name)
		return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, obj, patch))
	}
	id := obj.Status.SilenceID
	if id == "" || obj.Status.State == alertsv1alpha1.SilenceExpired || obj.Status.State == alertsv1alpha1.SilenceExpiredExternally {
		return release("", "")
	}
	scope := &alertsv1alpha1.AlertScope{}
	if err := r.Get(ctx, types.NamespacedName{Name: obj.Spec.ScopeRef}, scope); err != nil {
		if apierrors.IsNotFound(err) {
			return release(corev1.EventTypeWarning, "AlertScope %s is gone: silence %s is left to end at its expiry",
				obj.Spec.ScopeRef, id)
		}
		return ctrl.Result{}, err
	}
	if !r.EnableWrites {
		return release(corev1.EventTypeWarning, "the operator runs without --enable-writes: silence %s is left to end at its expiry", id)
	}
	c, err := r.clients(ctx, scope, true)
	if err != nil {
		return release(corev1.EventTypeWarning, "silence %s is left to end at its expiry: %v", id, err)
	}
	marker := promapi.Marker(obj.Namespace, obj.Name, string(obj.UID))
	_, err = c.writer.ExpireSilence(ctx, id, marker)
	end := time.Time{}
	if obj.Status.EndsAt != nil {
		end = obj.Status.EndsAt.Time
	}
	switch {
	case err == nil:
		r.auditWrite(obj, scope.Name, "expire", id, end, nil)
		return release(corev1.EventTypeNormal, "Expired silence %s", id)
	case errors.Is(err, promapi.ErrNotFound):
		return release("", "")
	case errors.Is(err, promapi.ErrNotOurs):
		r.auditWrite(obj, scope.Name, "expire", id, end, err)
		return release(corev1.EventTypeWarning, "silence %s is not this object's and is left alone", id)
	}
	r.auditWrite(obj, scope.Name, "expire", id, end, err)
	if r.now().Sub(obj.DeletionTimestamp.Time) < DeleteRetry {
		logf.FromContext(ctx).Info("Could not expire silence, retrying", "silence", id, "error", err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return release(corev1.EventTypeWarning, "Alertmanager unreachable for %s: silence %s is left to end at its expiry (%v)",
		DeleteRetry, id, err)
}

// SetupWithManager sets up the controller with the Manager.
func (r *AlertSilenceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	silencesOfScope := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		list := &alertsv1alpha1.AlertSilenceList{}
		if err := mgr.GetClient().List(ctx, list); err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, s := range list.Items {
			if s.Spec.ScopeRef == o.GetName() {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: s.Namespace, Name: s.Name}})
			}
		}
		return out
	})
	deleting := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetDeletionTimestamp().IsZero() && !e.ObjectNew.GetDeletionTimestamp().IsZero()
	}}
	return ctrl.NewControllerManagedBy(mgr).
		For(&alertsv1alpha1.AlertSilence{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, deleting))).
		Watches(&alertsv1alpha1.AlertScope{}, silencesOfScope, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("alertsilence").
		Complete(r)
}

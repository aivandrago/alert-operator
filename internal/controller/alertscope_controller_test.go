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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/inventory"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

var (
	k8s    client.Client
	scheme = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*")); len(matches) > 0 {
			abs, _ := filepath.Abs(matches[len(matches)-1])
			_ = os.Setenv("KUBEBUILDER_ASSETS", abs)
		}
	}
	_ = clientgoscheme.AddToScheme(scheme)
	_ = alertsv1alpha1.AddToScheme(scheme)
	_ = monitoringv1.AddToScheme(scheme)
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	code := func() int {
		defer func() { _ = env.Stop() }()
		k8s, err = client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			panic(err)
		}
		return m.Run()
	}()
	os.Exit(code)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func drain(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func rule(alert, expr string, labels, ann map[string]string) monitoringv1.Rule {
	return monitoringv1.Rule{Alert: alert, Expr: intstr.FromString(expr), Labels: labels, Annotations: ann}
}

func loadedFrom(pr *monitoringv1.PrometheusRule) []promapi.RuleGroup {
	out := make([]promapi.RuleGroup, 0, len(pr.Spec.Groups))
	for _, g := range pr.Spec.Groups {
		rg := promapi.RuleGroup{Name: g.Name, Interval: 30,
			File: "/etc/prometheus/rules/prometheus-k8s-rulefiles-0/" + pr.Namespace + "-" + pr.Name + "-" + string(pr.UID) + ".yaml"}
		for _, r := range g.Rules {
			rg.Rules = append(rg.Rules, promapi.Rule{Name: r.Alert, Query: r.Expr.String(), Labels: r.Labels,
				Annotations: r.Annotations, Health: "ok", Type: "alerting"})
		}
		out = append(out, rg)
	}
	return out
}

func mirrors(t *testing.T) map[string]alertsv1alpha1.AlertRule {
	t.Helper()
	list := &alertsv1alpha1.AlertRuleList{}
	mustNoErr(t, k8s.List(context.Background(), list, client.MatchingLabels{alertsv1alpha1.LabelScope: "main"}))
	out := map[string]alertsv1alpha1.AlertRule{}
	for _, ar := range list.Items {
		out[ar.Spec.Source.Alert] = ar
	}
	return out
}

func types_(fs []alertsv1alpha1.Finding) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		parts = append(parts, string(f.Type))
	}
	return strings.Join(parts, ",")
}

// TestScopeLifecycle walks one scope through create, update, a foreign object in the way,
// finding transitions, Prometheus going down and coming back, and deletion.
func TestScopeLifecycle(t *testing.T) {
	ctx := context.Background()
	prom := newFakeProm()
	defer prom.srv.Close()
	clk := &clock{t: time.Now()}
	rec := events.NewFakeRecorder(100)
	r := &AlertScopeReconciler{Client: k8s, APIReader: k8s, Scheme: scheme, Recorder: rec,
		OperatorNamespace: "alert-operator-system", Now: clk.now}

	for _, ns := range []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{"hs/team": "a"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alert-operator-system"}},
	} {
		mustNoErr(t, k8s.Create(ctx, ns))
	}
	runbook := map[string]string{"runbook_url": "https://runbooks/x"}
	prA := &monitoringv1.PrometheusRule{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "api"},
		Spec: monitoringv1.PrometheusRuleSpec{Groups: []monitoringv1.RuleGroup{{Name: "api", Rules: []monitoringv1.Rule{
			rule("ApiDown", "up == 0", map[string]string{"severity": "critical"}, runbook),
			rule("ApiSlow", "missing_latency > 1", map[string]string{"severity": "warning"}, runbook),
		}}}}}
	prB := &monitoringv1.PrometheusRule{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "web"},
		Spec: monitoringv1.PrometheusRuleSpec{Groups: []monitoringv1.RuleGroup{{Name: "web", Rules: []monitoringv1.Rule{
			rule("WebDown", "up == 0", nil, nil),
			rule("Taken", "up == 0", map[string]string{"severity": "info", "team": "b"}, runbook),
		}}}}}
	mustNoErr(t, k8s.Create(ctx, prA))
	mustNoErr(t, k8s.Create(ctx, prB))
	// Prometheus loaded team-a's rules only; team-b's object is not picked up.
	prom.set(func(f *fakeProm) { f.groups = loadedFrom(prA) })
	clk.advance(10 * time.Minute) // past the load grace of team-b's object

	// Somebody else's object already has the name the Taken rule's mirror would get.
	takenID := inventory.Identity("main", alertsv1alpha1.RuleSource{Kind: "PrometheusRule", Namespace: "team-b",
		Name: "web", Group: "web", Alert: "Taken"}, 0)
	foreign := &alertsv1alpha1.AlertRule{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: MirrorName("Taken", takenID)},
		Spec: alertsv1alpha1.AlertRuleSpec{ScopeRef: "other", Expr: "vector(1)",
			Source: alertsv1alpha1.RuleSource{Kind: "File", Group: "x", Alert: "Other"}}}
	mustNoErr(t, k8s.Create(ctx, foreign))

	scope := &alertsv1alpha1.AlertScope{ObjectMeta: metav1.ObjectMeta{Name: "main"},
		Spec: alertsv1alpha1.AlertScopeSpec{Prometheus: alertsv1alpha1.PrometheusEndpoint{
			Endpoint: alertsv1alpha1.Endpoint{URL: prom.srv.URL}},
			Alertmanager: &alertsv1alpha1.Endpoint{URL: prom.srv.URL}}}
	mustNoErr(t, k8s.Create(ctx, scope))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "main"}}
	reconcile := func() ctrl.Result {
		t.Helper()
		res, err := r.Reconcile(ctx, req)
		mustNoErr(t, err)
		return res
	}

	// 1. First sync: create.
	res := reconcile()
	if res.RequeueAfter <= 0 || res.RequeueAfter > 15*time.Minute {
		t.Errorf("requeue after %v", res.RequeueAfter)
	}
	m := mirrors(t)
	if len(m) != 3 {
		t.Fatalf("mirrors: %v", m)
	}
	down := m["ApiDown"]
	if down.Namespace != "team-a" || !down.Status.Loaded || down.Status.Team != "a" || down.Labels["hs/team"] != "a" ||
		len(down.Status.Findings) != 0 || down.Status.LastEvaluated == nil || down.Status.Window != "15d" {
		t.Errorf("ApiDown: %+v %+v", down.ObjectMeta, down.Status)
	}
	if len(down.OwnerReferences) != 1 || down.OwnerReferences[0].UID != prA.UID || *down.OwnerReferences[0].Controller {
		t.Errorf("ownerReferences: %+v", down.OwnerReferences)
	}
	if got := types_(m["ApiSlow"].Status.Findings); got != "DeadQuery" {
		t.Errorf("ApiSlow findings: %s", got)
	}
	web := m["WebDown"]
	if web.Status.Loaded || types_(web.Status.Findings) != "NotLoaded,Unowned,NoRunbook,NoSeverity" ||
		web.Status.FindingsSummary != "NotLoaded,Unowned,NoRunbook,NoSeverity" {
		t.Errorf("WebDown: %+v", web.Status)
	}
	if c := meta.FindStatusCondition(web.Status.Conditions, "Healthy"); c == nil || c.Status != "False" || c.Reason != "NotLoaded" {
		t.Errorf("WebDown Healthy: %+v", c)
	}
	if _, ok := m["Taken"]; ok {
		t.Error("the foreign object was taken over")
	}
	got := &alertsv1alpha1.AlertRule{}
	mustNoErr(t, k8s.Get(ctx, client.ObjectKeyFromObject(foreign), got))
	if got.Spec.ScopeRef != "other" || len(got.Labels) != 0 {
		t.Errorf("foreign object modified: %+v", got)
	}

	mustNoErr(t, k8s.Get(ctx, req.NamespacedName, scope))
	s := scope.Status
	if s.Rules != (alertsv1alpha1.RuleCounts{Total: 3, Loaded: 2, NotLoaded: 1}) || s.FindingsTotal != 5 ||
		s.Findings["NotLoaded"] != 1 || s.Findings["DeadQuery"] != 1 || s.EffectiveWindow != "15d" || s.LastSync == nil {
		t.Errorf("scope status: %+v", s)
	}
	if len(s.Teams) != 2 || s.Teams[0] != (alertsv1alpha1.TeamSummary{Team: "<none>", Rules: 1, Findings: 4}) ||
		s.Teams[1] != (alertsv1alpha1.TeamSummary{Team: "a", Rules: 2, Findings: 1}) {
		t.Errorf("teams: %+v", s.Teams)
	}
	if c := meta.FindStatusCondition(s.Conditions, "Ready"); c == nil || c.Reason != ReasonConflict ||
		len(s.Conflicts) != 1 || s.Conflicts[0] != "team-b/"+foreign.Name {
		t.Errorf("conflict not reported: %+v %v", c, s.Conflicts)
	}
	if ev := drain(rec); len(ev) != 0 {
		t.Errorf("no events on creation, got %v", ev)
	}

	// 2. Nothing changed: no status writes.
	clk.advance(time.Minute)
	rvBefore := down.ResourceVersion
	reconcile()
	if rv := mirrors(t)["ApiDown"].ResourceVersion; rv != rvBefore {
		t.Errorf("unchanged mirror was rewritten (%s -> %s)", rvBefore, rv)
	}

	// 3. Update: the expression changes, the mirror keeps its name and follows.
	mustNoErr(t, k8s.Get(ctx, client.ObjectKeyFromObject(prA), prA))
	prA.Spec.Groups[0].Rules[0].Expr = intstr.FromString("up{job=\"api\"} == 0")
	mustNoErr(t, k8s.Update(ctx, prA))
	prom.set(func(f *fakeProm) { f.groups = loadedFrom(prA) })
	reconcile()
	m = mirrors(t)
	if m["ApiDown"].Name != down.Name || m["ApiDown"].Spec.Expr != `up{job="api"} == 0` {
		t.Errorf("update: %s %q", m["ApiDown"].Name, m["ApiDown"].Spec.Expr)
	}

	// 4. ApiDown becomes noisy in the next analysis: one event, and none again while it stays.
	prom.set(func(f *fakeProm) { f.activations["ApiDown"] = 79 })
	clk.advance(16 * time.Minute)
	reconcile()
	ev := drain(rec)
	if len(ev) != 1 || !strings.Contains(ev[0], "FindingAdded") || !strings.Contains(ev[0], "ApiDown became Noisy") {
		t.Errorf("events after becoming noisy: %v", ev)
	}
	if q := mirrors(t)["ApiDown"].Status.Quality; q.Activations != 80 || q.ActivationsPerDay != 5.33 {
		t.Errorf("quality: %+v", q)
	}
	clk.advance(16 * time.Minute)
	reconcile()
	if ev := drain(rec); len(ev) != 0 {
		t.Errorf("no transition, but events: %v", ev)
	}
	prom.set(func(f *fakeProm) { delete(f.activations, "ApiDown") })
	clk.advance(16 * time.Minute)
	reconcile()
	if ev := drain(rec); len(ev) != 1 || !strings.Contains(ev[0], "ApiDown is no longer Noisy") {
		t.Errorf("events after quieting down: %v", ev)
	}

	// 5. Prometheus down, and ApiSlow removed meanwhile: nothing is deleted.
	mustNoErr(t, k8s.Get(ctx, client.ObjectKeyFromObject(prA), prA))
	prA.Spec.Groups[0].Rules = prA.Spec.Groups[0].Rules[:1]
	mustNoErr(t, k8s.Update(ctx, prA))
	prom.set(func(f *fakeProm) { f.down = true })
	res = reconcile()
	if res.RequeueAfter != RetryInterval {
		t.Errorf("retry after %v", res.RequeueAfter)
	}
	if _, ok := mirrors(t)["ApiSlow"]; !ok {
		t.Fatal("a mirror was deleted while Prometheus was down")
	}
	mustNoErr(t, k8s.Get(ctx, req.NamespacedName, scope))
	if c := meta.FindStatusCondition(scope.Status.Conditions, "Ready"); c == nil || c.Status != "False" ||
		c.Reason != ReasonPrometheusUnreachable {
		t.Errorf("Ready while down: %+v", c)
	}

	// 6. Back up: the removed rule's mirror goes.
	prom.set(func(f *fakeProm) { f.down = false; f.groups = loadedFrom(prA) })
	clk.advance(time.Minute)
	reconcile()
	if _, ok := mirrors(t)["ApiSlow"]; ok {
		t.Error("the mirror of a removed rule survived")
	}

	// 7. The scope goes: its mirrors go, the foreign object stays.
	mustNoErr(t, k8s.Delete(ctx, scope))
	reconcile()
	if m := mirrors(t); len(m) != 0 {
		t.Errorf("mirrors left after the scope was deleted: %v", m)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(foreign), &alertsv1alpha1.AlertRule{}); apierrors.IsNotFound(err) {
		t.Error("the foreign object was deleted with the scope")
	}

	prom.mu.Lock()
	defer prom.mu.Unlock()
	for method := range prom.methods {
		if method != "GET" {
			t.Errorf("the operator sent a %s to Prometheus or Alertmanager", method)
		}
	}
}

func TestMirrorName(t *testing.T) {
	cases := map[string]string{
		"PaymentsHighLatency":   "paymentshighlatency-3f9c1a",
		"KubePod_CrashLooping":  "kubepod-crashlooping-3f9c1a",
		"!!!":                   "rule-3f9c1a",
		strings.Repeat("A", 80): strings.Repeat("a", 56) + "-3f9c1a",
	}
	for in, want := range cases {
		if got := MirrorName(in, "3f9c1a7e"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

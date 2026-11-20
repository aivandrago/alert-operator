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

package webhook

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
)

// warnings records the admission warnings the API server passes on.
type warnings struct {
	mu   sync.Mutex
	list []string
}

func (w *warnings) HandleWarningHeader(_ int, _ string, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list = append(w.list, text)
}

func (w *warnings) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.list
	w.list = nil
	return out
}

var (
	k8s     client.Client
	warned  = &warnings{}
	scheme  = runtime.NewScheme()
	slowDQ  atomic.Bool
	queries atomic.Int32
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

	failurePolicy := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone
	path := Path
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			ValidatingWebhooks: []*admissionv1.ValidatingWebhookConfiguration{{
				ObjectMeta: metav1.ObjectMeta{Name: "alert-operator"},
				Webhooks: []admissionv1.ValidatingWebhook{{
					Name:                    "prometheusrules.alerts.hypersurgery.dev",
					AdmissionReviewVersions: []string{"v1"},
					SideEffects:             &sideEffects,
					FailurePolicy:           &failurePolicy,
					ClientConfig:            admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{Name: "x", Namespace: "x", Path: &path}},
					NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key: LabelMode, Operator: metav1.LabelSelectorOpNotIn, Values: []string{ModeOff},
					}}},
					Rules: []admissionv1.RuleWithOperations{{
						Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
						Rule: admissionv1.Rule{APIGroups: []string{"monitoring.coreos.com"}, APIVersions: []string{"v1"},
							Resources: []string{"prometheusrules"}},
					}},
				}},
			}},
		},
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	code := func() int {
		defer func() { _ = env.Stop() }()
		opts := env.WebhookInstallOptions
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{Host: opts.LocalServingHost, Port: opts.LocalServingPort,
				CertDir: opts.LocalServingCertDir}),
		})
		if err != nil {
			panic(err)
		}
		mgr.GetWebhookServer().Register(Path, &ctrlwebhook.Admission{Handler: &Validator{
			Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Decoder: admission.NewDecoder(scheme),
			OperatorNamespace: "alert-operator-system", DefaultMode: ModeWarn,
		}})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = mgr.Start(ctx) }()

		wcfg := rest.CopyConfig(cfg)
		wcfg.WarningHandler = warned
		k8s, err = client.New(wcfg, client.Options{Scheme: scheme})
		if err != nil {
			panic(err)
		}
		// Wait until the webhook serves.
		deadline := time.Now().Add(30 * time.Second)
		for {
			err := k8s.Create(ctx, &monitoringv1.PrometheusRule{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "probe"}})
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				panic(fmt.Sprintf("webhook not serving: %v", err))
			}
			time.Sleep(200 * time.Millisecond)
		}
		warned.take()
		return m.Run()
	}()
	os.Exit(code)
}

func promRule(ns, name string, objLabels map[string]string, rules ...monitoringv1.Rule) *monitoringv1.PrometheusRule {
	return &monitoringv1.PrometheusRule{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: objLabels},
		Spec:       monitoringv1.PrometheusRuleSpec{Groups: []monitoringv1.RuleGroup{{Name: "g", Rules: rules}}},
	}
}

func alert(name, expr string, labels map[string]string, runbook bool) monitoringv1.Rule {
	r := monitoringv1.Rule{Alert: name, Expr: intstr.FromString(expr), Labels: labels}
	if runbook {
		r.Annotations = map[string]string{"runbook_url": "https://runbooks/x"}
	}
	return r
}

func namespace(t *testing.T, name string, labels map[string]string) {
	t.Helper()
	if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}); err != nil {
		t.Fatal(err)
	}
}

func joined(ws []string) string { return strings.Join(ws, "\n") }

func TestModesWithoutScope(t *testing.T) {
	ctx := context.Background()
	namespace(t, "warn-ns", nil)
	namespace(t, "enforce-ns", map[string]string{LabelMode: ModeEnforce})
	namespace(t, "off-ns", map[string]string{LabelMode: ModeOff})
	bad := func(ns string) *monitoringv1.PrometheusRule {
		return promRule(ns, "bad", nil, alert("Broken", "rate(x[5m]", nil, false), alert("Nobody", "up == 0", nil, false))
	}

	// warn (the default): admitted, every problem as a warning.
	if err := k8s.Create(ctx, bad("warn-ns")); err != nil {
		t.Fatalf("warn mode refused: %v", err)
	}
	ws := joined(warned.take())
	for _, want := range []string{"g/Broken: LoadError", "g/Nobody: Unowned", "g/Nobody: NoRunbook", "g/Nobody: NoSeverity",
		"not checked: no AlertScope selects this namespace"} {
		if !strings.Contains(ws, want) {
			t.Errorf("warn mode: warnings lack %q:\n%s", want, ws)
		}
	}

	// enforce: denied, the problems in the message.
	err := k8s.Create(ctx, bad("enforce-ns"))
	if err == nil || !strings.Contains(err.Error(), "enforces alerts.hypersurgery.dev/validation") ||
		!strings.Contains(err.Error(), "g/Broken: LoadError") {
		t.Errorf("enforce mode: %v", err)
	}
	warned.take()

	// A good rule is admitted in enforce mode without a problem.
	good := promRule("enforce-ns", "good", map[string]string{"hs/team": "a"},
		alert("ApiDown", "up == 0", map[string]string{"severity": "critical"}, true))
	if err := k8s.Create(ctx, good); err != nil {
		t.Errorf("good rule refused: %v", err)
	}
	if ws := warned.take(); strings.Contains(joined(ws), "LoadError") || strings.Contains(joined(ws), "Unowned") {
		t.Errorf("good rule warned: %v", ws)
	}

	// off: not even sent.
	if err := k8s.Create(ctx, bad("off-ns")); err != nil {
		t.Errorf("off: %v", err)
	}
	if ws := warned.take(); len(ws) != 0 {
		t.Errorf("off: warnings %v", ws)
	}
}

func TestWithScope(t *testing.T) {
	ctx := context.Background()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries.Add(1)
		if slowDQ.Load() {
			time.Sleep(4 * time.Second)
		}
		result := `[{"metric":{},"value":[1,"1"]}]`
		if strings.Contains(r.URL.Query().Get("query"), "renamed") {
			result = `[]`
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, result)
	}))
	defer prom.Close()

	namespace(t, "monitoring", nil)
	namespace(t, "team-x", map[string]string{"scoped": "yes", LabelMode: ModeEnforce})
	p := &monitoringv1.Prometheus{ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "k8s"}}
	p.Spec.RuleSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"release": "kps"}}
	p.Spec.RuleNamespaceSelector = &metav1.LabelSelector{}
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	no := false
	scope := &alertsv1alpha1.AlertScope{ObjectMeta: metav1.ObjectMeta{Name: "scoped"}, Spec: alertsv1alpha1.AlertScopeSpec{
		Prometheus: alertsv1alpha1.PrometheusEndpoint{Endpoint: alertsv1alpha1.Endpoint{URL: prom.URL},
			ResourceRef: &alertsv1alpha1.ObjectReference{Namespace: "monitoring", Name: "k8s"}},
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"scoped": "yes"}},
		Ownership:         alertsv1alpha1.Ownership{RequireRunbook: &no},
	}}
	if err := k8s.Create(ctx, scope); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"severity": "warning", "team": "x"}
	try := func(name string, objLabels map[string]string, expr string) ([]string, error) {
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := k8s.Create(ctx, promRule("team-x", name, objLabels, alert("A", expr, labels, false)))
			ws := warned.take()
			// The scope reaches the webhook's cache a moment after it is created.
			if strings.Contains(joined(ws), "no AlertScope selects") && time.Now().Before(deadline) {
				if err == nil {
					_ = k8s.Delete(ctx, promRule("team-x", name, nil))
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return ws, err
		}
	}

	// The scope's ownership settings apply: no runbook needed. Loaded by kps: admitted.
	if ws, err := try("fine", map[string]string{"release": "kps"}, "up == 0"); err != nil || len(ws) != 0 {
		t.Errorf("fine rule: %v %v", err, ws)
	}
	// Not picked up by the Prometheus ruleSelector: denied in enforce mode.
	if _, err := try("unselected", nil, "up == 0"); err == nil || !strings.Contains(err.Error(), "NotLoaded") {
		t.Errorf("NotLoaded: %v", err)
	}
	// A dead selector never blocks, even in enforce mode.
	ws, err := try("dead", map[string]string{"release": "kps"}, "renamed_metric > 0")
	if err != nil || !strings.Contains(joined(ws), "DeadQuery") {
		t.Errorf("DeadQuery: %v %v", err, ws)
	}
	// The answer is cached: the same selector again asks Prometheus no more.
	before := queries.Load()
	if _, err := try("dead-again", map[string]string{"release": "kps"}, "renamed_metric > 0"); err != nil || queries.Load() != before {
		t.Errorf("cache: %v, %d queries more", err, queries.Load()-before)
	}
	// A slow Prometheus: the review ends within its budget, the check is skipped with a note.
	slowDQ.Store(true)
	start := time.Now()
	ws, err = try("slow", map[string]string{"release": "kps"}, "new_metric > 0")
	slowDQ.Store(false)
	if err != nil || time.Since(start) > 4*time.Second || !strings.Contains(joined(ws), "not checked: DeadQuery") {
		t.Errorf("slow Prometheus: %v after %v, %v", err, time.Since(start), ws)
	}
}

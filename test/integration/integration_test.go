//go:build integration

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

// Package integration runs the whole operator against real components without Docker: a
// kube-apiserver (envtest), a Prometheus that scrapes a tiny exporter and evaluates the rules of
// the PrometheusRule objects (written to rule files the way prometheus-operator names them),
// and an Alertmanager holding a silence the test made. It proves every v0.1 finding except
// NeverFired, which needs a rule known for at least an hour.
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/controller"
)

var (
	scheme = runtime.NewScheme()
	cfg    *rest.Config
	k8s    client.Client
)

func TestMain(m *testing.M) {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = alertsv1alpha1.AddToScheme(scheme)
	_ = monitoringv1.AddToScheme(scheme)
	ctrl.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(false)))
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	cfg, err = env.Start()
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

func bin(t *testing.T, env string) string {
	t.Helper()
	p := os.Getenv(env)
	if p == "" {
		t.Skipf("%s is not set: run make test-integration", env)
	}
	return p
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func start(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	logf, err := os.Create(filepath.Join(t.TempDir(), filepath.Base(name)+".log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(logf.Name())
			t.Logf("%s log:\n%s", filepath.Base(name), b)
		}
	})
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready", url)
}

func alert(name, expr string, labels, ann map[string]string) monitoringv1.Rule {
	return monitoringv1.Rule{Alert: name, Expr: intstr.FromString(expr), Labels: labels, Annotations: ann}
}

func promRule(ns, name string, objLabels map[string]string, rules ...monitoringv1.Rule) *monitoringv1.PrometheusRule {
	return &monitoringv1.PrometheusRule{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: objLabels},
		Spec:       monitoringv1.PrometheusRuleSpec{Groups: []monitoringv1.RuleGroup{{Name: name, Rules: rules}}},
	}
}

// writeRuleFile writes the object's groups to <dir>/<namespace>-<name>-<uid>.yaml, the file
// name prometheus-operator gives it.
func writeRuleFile(t *testing.T, dir string, pr *monitoringv1.PrometheusRule) {
	t.Helper()
	type rule struct {
		Alert       string            `yaml:"alert"`
		Expr        string            `yaml:"expr"`
		Labels      map[string]string `yaml:"labels,omitempty"`
		Annotations map[string]string `yaml:"annotations,omitempty"`
	}
	type group struct {
		Name  string `yaml:"name"`
		Rules []rule `yaml:"rules"`
	}
	var out struct {
		Groups []group `yaml:"groups"`
	}
	for _, g := range pr.Spec.Groups {
		gg := group{Name: g.Name}
		for _, r := range g.Rules {
			gg.Rules = append(gg.Rules, rule{Alert: r.Alert, Expr: r.Expr.String(), Labels: r.Labels, Annotations: r.Annotations})
		}
		out.Groups = append(out.Groups, gg)
	}
	b, err := yaml.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%s-%s-%s.yaml", pr.Namespace, pr.Name, pr.UID)
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func silences(t *testing.T, amURL string) string {
	t.Helper()
	resp, err := http.Get(amURL + "/api/v2/silences")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestInventoryAgainstRealPrometheus(t *testing.T) {
	prometheus, alertmanager := bin(t, "PROMETHEUS"), bin(t, "ALERTMANAGER")
	ctx := t.Context()
	dir := t.TempDir()

	// A tiny exporter: demo_flap toggles every three seconds.
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flap := (time.Now().Unix() / 3) % 2
		_, _ = fmt.Fprintf(w, "demo_up 1\ndemo_silenced 1\ndemo_flap %d\n", flap)
	}))
	defer exporter.Close()

	for _, ns := range []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{"hs/team": "a"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alert-operator-system"}},
	} {
		if err := k8s.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	promObj := &monitoringv1.Prometheus{ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "k8s"}}
	promObj.Spec.RuleSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"release": "kps"}}
	promObj.Spec.RuleNamespaceSelector = &metav1.LabelSelector{}
	if err := k8s.Create(ctx, promObj); err != nil {
		t.Fatal(err)
	}

	kps := map[string]string{"release": "kps"}
	rb := map[string]string{"runbook_url": "https://runbooks.example/demo"}
	warn := map[string]string{"severity": "warning"}
	objs := []*monitoringv1.PrometheusRule{
		promRule("team-a", "fine", kps, alert("DemoFine", "demo_up == 0", warn, rb)),
		promRule("team-a", "flappy", kps, alert("DemoFlapping", "demo_flap == 1", warn, rb)),
		promRule("team-a", "dead", kps, alert("DemoDead", "demo_renamed_metric > 0", warn, rb)),
		promRule("team-a", "silenced", kps, alert("DemoSilenced", "demo_silenced == 1", warn, rb)),
		promRule("team-b", "unowned", kps, alert("DemoUnowned", "demo_up == 0", nil, nil)),
		promRule("team-b", "notloaded", map[string]string{"release": "other"}, alert("DemoNotLoaded", "demo_up == 0", warn, rb)),
	}
	rulesDir := filepath.Join(dir, "rules")
	_ = os.MkdirAll(rulesDir, 0o700)
	for _, pr := range objs {
		if err := k8s.Create(ctx, pr); err != nil {
			t.Fatal(err)
		}
		if pr.Labels["release"] == "kps" { // what prometheus-operator would hand to Prometheus
			writeRuleFile(t, rulesDir, pr)
		}
	}
	resourceVersions := map[string]string{}
	for _, pr := range objs {
		resourceVersions[pr.Namespace+"/"+pr.Name] = pr.ResourceVersion
	}

	// Alertmanager, with a silence made by somebody else (the test), before anything fires.
	amAddr := freeAddr(t)
	amCfg := filepath.Join(dir, "alertmanager.yml")
	_ = os.WriteFile(amCfg, []byte("route:\n  receiver: blackhole\nreceivers:\n  - name: blackhole\n"), 0o600)
	start(t, alertmanager, "--config.file="+amCfg, "--storage.path="+filepath.Join(dir, "am"),
		"--web.listen-address="+amAddr, "--cluster.listen-address=")
	amURL := "http://" + amAddr
	waitReady(t, amURL+"/-/ready")
	silence := map[string]any{
		"matchers": []map[string]any{{"name": "alertname", "value": "DemoSilenced", "isRegex": false, "isEqual": true}},
		"startsAt": time.Now().UTC().Format(time.RFC3339), "endsAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"createdBy": "integration-test", "comment": "made by the test, not by the operator",
	}
	body, _ := json.Marshal(silence)
	resp, err := http.Post(amURL+"/api/v2/silences", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("creating the silence: %v %v", err, resp)
	}
	_ = resp.Body.Close()
	silencesBefore := silences(t, amURL)

	// Prometheus: scrapes the exporter every second, evaluates the rules every second.
	promAddr := freeAddr(t)
	promCfg := filepath.Join(dir, "prometheus.yml")
	_ = os.WriteFile(promCfg, fmt.Appendf(nil, `global:
  scrape_interval: 1s
  evaluation_interval: 1s
rule_files: [%q]
alerting:
  alertmanagers:
    - static_configs: [{targets: [%q]}]
scrape_configs:
  - job_name: demo
    static_configs: [{targets: [%q]}]
`, filepath.Join(rulesDir, "*.yaml"), amAddr, strings.TrimPrefix(exporter.URL, "http://")), 0o600)
	start(t, prometheus, "--config.file="+promCfg, "--storage.tsdb.path="+filepath.Join(dir, "prom"),
		"--web.listen-address="+promAddr, "--log.level=warn")
	promURL := "http://" + promAddr
	waitReady(t, promURL+"/-/ready")

	// The operator, in-process.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&controller.AlertScopeReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		Scheme: scheme, Recorder: mgr.GetEventRecorder("alert-operator"),
		OperatorNamespace: "alert-operator-system"}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	scope := &alertsv1alpha1.AlertScope{ObjectMeta: metav1.ObjectMeta{Name: "main"},
		Spec: alertsv1alpha1.AlertScopeSpec{
			Prometheus: alertsv1alpha1.PrometheusEndpoint{Endpoint: alertsv1alpha1.Endpoint{URL: promURL},
				ResourceRef: &alertsv1alpha1.ObjectReference{Namespace: "monitoring", Name: "k8s"}},
			Alertmanager: &alertsv1alpha1.Endpoint{URL: amURL},
			Quality: alertsv1alpha1.Quality{Window: "1h", ResyncInterval: "1m",
				Noisy:          alertsv1alpha1.NoisyThreshold{FiringsAbove: 3},
				Flapping:       alertsv1alpha1.FlappingThreshold{ActivationsPerDayAbove: 1, MedianDurationBelow: "10m"},
				AlwaysSilenced: alertsv1alpha1.AlwaysSilencedThreshold{SilencedFractionAbove: 0.5}},
		}}
	if err := k8s.Create(ctx, scope); err != nil {
		t.Fatal(err)
	}

	rules := func() map[string]alertsv1alpha1.AlertRule {
		list := &alertsv1alpha1.AlertRuleList{}
		if err := k8s.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		out := map[string]alertsv1alpha1.AlertRule{}
		for _, r := range list.Items {
			out[r.Spec.Source.Alert] = r
		}
		return out
	}
	has := func(r alertsv1alpha1.AlertRule, ft alertsv1alpha1.FindingType) bool {
		return slices.ContainsFunc(r.Status.Findings, func(f alertsv1alpha1.Finding) bool { return f.Type == ft })
	}

	// The history verdicts need data: they settle after a few one-minute analyses.
	deadline := time.Now().Add(6 * time.Minute)
	var got map[string]alertsv1alpha1.AlertRule
	for {
		got = rules()
		if has(got["DemoFlapping"], alertsv1alpha1.FindingNoisy) && has(got["DemoFlapping"], alertsv1alpha1.FindingFlapping) &&
			has(got["DemoSilenced"], alertsv1alpha1.FindingAlwaysSilenced) {
			break
		}
		if time.Now().After(deadline) {
			for name, r := range got {
				t.Logf("%s: %s %+v", name, r.Status.FindingsSummary, r.Status.Quality)
			}
			t.Fatal("verdicts did not settle")
		}
		time.Sleep(5 * time.Second)
	}
	for name, r := range got {
		t.Logf("%-14s %-4s loaded=%-5v findings=%-40s fired=%d median=%s silenced=%.2f", name, r.Namespace,
			r.Status.Loaded, r.Status.FindingsSummary, r.Status.Quality.Activations, r.Status.Quality.MedianFiringDuration,
			r.Status.Quality.SilencedFraction)
	}

	want := map[string]string{
		"DemoFine":      "",
		"DemoFlapping":  "Noisy,Flapping",
		"DemoDead":      "DeadQuery",
		"DemoSilenced":  "AlwaysSilenced",
		"DemoUnowned":   "Unowned,NoRunbook,NoSeverity",
		"DemoNotLoaded": "NotLoaded,Unowned", // team-b names no team
	}
	if len(got) != len(want) {
		t.Errorf("%d AlertRules, want %d", len(got), len(want))
	}
	for name, findings := range want {
		r, ok := got[name]
		if !ok {
			t.Errorf("no AlertRule for %s", name)
			continue
		}
		if r.Status.FindingsSummary != findings {
			t.Errorf("%s: findings %q, want %q", name, r.Status.FindingsSummary, findings)
		}
	}
	if r := got["DemoNotLoaded"]; !strings.Contains(r.Status.NotLoadedReason, "ruleSelector") {
		t.Errorf("NotLoaded reason: %q", r.Status.NotLoadedReason)
	}
	if r := got["DemoFine"]; r.Status.Team != "a" || r.Namespace != "team-a" || !r.Status.Loaded {
		t.Errorf("DemoFine: %+v", r.Status)
	}

	if err := k8s.Get(ctx, client.ObjectKeyFromObject(scope), scope); err != nil {
		t.Fatal(err)
	}
	if s := scope.Status; s.Rules.Total != 6 || s.Rules.NotLoaded != 1 || s.EffectiveWindow != "1h" ||
		meta.FindStatusCondition(s.Conditions, "Ready").Status != metav1.ConditionTrue {
		t.Errorf("scope status: %+v", s)
	}

	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() != "hs_alert_rule_findings" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var alertname, finding string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "alertname":
					alertname = l.GetValue()
				case "finding":
					finding = l.GetValue()
				}
			}
			if alertname == "DemoFlapping" && finding == "Flapping" {
				found = true
			}
		}
	}
	if !found {
		t.Error(`no hs_alert_rule_findings{alertname="DemoFlapping",finding="Flapping"}`)
	}

	// An Event when DemoFlapping became Noisy (it had no history at the first analysis).
	evs := &eventsv1.EventList{}
	if err := k8s.List(ctx, evs, client.InNamespace("team-a")); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(evs.Items, func(e eventsv1.Event) bool {
		return e.Reason == "FindingAdded" && strings.Contains(e.Note, "DemoFlapping became")
	}) {
		t.Errorf("no FindingAdded event for DemoFlapping in %d events", len(evs.Items))
	}

	// Read-only: no PrometheusRule and no silence was touched.
	for _, pr := range objs {
		cur := &monitoringv1.PrometheusRule{}
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(pr), cur); err != nil {
			t.Fatal(err)
		}
		if cur.ResourceVersion != resourceVersions[pr.Namespace+"/"+pr.Name] {
			t.Errorf("PrometheusRule %s/%s was modified", pr.Namespace, pr.Name)
		}
	}
	cur := &monitoringv1.Prometheus{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(promObj), cur); err != nil || cur.ResourceVersion != promObj.ResourceVersion {
		t.Errorf("the Prometheus object was modified: %v", err)
	}
	if after := silences(t, amURL); after != silencesBefore {
		t.Errorf("silences changed:\nbefore %s\nafter  %s", silencesBefore, after)
	}
}

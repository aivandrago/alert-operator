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

// Package crd checks the generated CRDs and the AlertRule admission policy against a real
// kube-apiserver (envtest): what the schema refuses, and who may write AlertRule objects.
package crd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
)

type scope = alertsv1alpha1.AlertScope

const operatorUser = "system:serviceaccount:alert-operator-system:alert-operator-controller-manager"

var (
	scheme   = runtime.NewScheme()
	admin    client.Client
	operator client.Client
	user     client.Client
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// make test sets it; a bare go test has no kube-apiserver to start.
		if dir := firstEnvtestDir(); dir != "" {
			_ = os.Setenv("KUBEBUILDER_ASSETS", dir)
		}
	}
	_ = clientgoscheme.AddToScheme(scheme)
	_ = alertsv1alpha1.AddToScheme(scheme)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	code := func() int {
		defer func() { _ = env.Stop() }()
		admin = mustClient(env, "admin")
		operator = mustClient(env, operatorUser)
		user = mustClient(env, "alice")
		_ = cfg
		if err := applyPolicy(context.Background()); err != nil {
			panic(err)
		}
		return m.Run()
	}()
	os.Exit(code)
}

func firstEnvtestDir() string {
	matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*"))
	if len(matches) == 0 {
		return ""
	}
	abs, _ := filepath.Abs(matches[len(matches)-1])
	return abs
}

func mustClient(env *envtest.Environment, name string) client.Client {
	u, err := env.AddUser(envtest.User{Name: name, Groups: []string{"system:masters"}}, nil)
	if err != nil {
		panic(err)
	}
	c, err := client.New(u.Config(), client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
	return c
}

// applyPolicy creates the policy and binding from config/policy as kustomize ships them.
func applyPolicy(ctx context.Context) error {
	f, err := os.Open(filepath.Join("..", "..", "config", "policy", "alertrule-protection.yaml"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	policy := &admissionv1.ValidatingAdmissionPolicy{}
	if err := dec.Decode(policy); err != nil {
		return err
	}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
	if err := dec.Decode(binding); err != nil {
		return err
	}
	if err := admin.Create(ctx, policy); err != nil {
		return err
	}
	return admin.Create(ctx, binding)
}

func validScope(name string) *alertsv1alpha1.AlertScope {
	return &alertsv1alpha1.AlertScope{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: alertsv1alpha1.AlertScopeSpec{
			Prometheus: alertsv1alpha1.PrometheusEndpoint{
				Endpoint: alertsv1alpha1.Endpoint{URL: "http://prometheus-operated.monitoring.svc:9090"},
			},
		},
	}
}

func TestScopeDefaults(t *testing.T) {
	ctx := context.Background()
	s := validScope("defaults")
	if err := admin.Create(ctx, s); err != nil {
		t.Fatalf("valid scope refused: %v", err)
	}
	got := &alertsv1alpha1.AlertScope{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(s), got); err != nil {
		t.Fatal(err)
	}
	q := got.Spec.Quality
	if q.Window != "30d" || q.ResyncInterval != "15m" || q.Noisy.FiringsAbove != 50 ||
		q.Flapping.ActivationsPerDayAbove != 10 || q.Flapping.MedianDurationBelow != "10m" ||
		q.NeverFired.After != "30d" || q.AlwaysSilenced.SilencedFractionAbove != 0.8 {
		t.Errorf("quality defaults not applied: %+v", q)
	}
	b := got.Spec.QueryBudget
	if b.Timeout != "20s" || b.MaxConcurrent != 2 || b.MaxQueriesPerMinute != 120 {
		t.Errorf("budget defaults not applied: %+v", b)
	}
	if got.Spec.Ownership.RequireRunbook == nil || !*got.Spec.Ownership.RequireRunbook {
		t.Errorf("requireRunbook should default to true")
	}
}

func TestInvalidScopesAreRefused(t *testing.T) {
	cases := map[string]struct {
		mutate func(*scope)
		want   string
	}{
		"url not a URL": {func(s *scope) { s.Spec.Prometheus.URL = "prometheus:9090" }, "http or https URL"},
		"url ftp":       {func(s *scope) { s.Spec.Prometheus.URL = "ftp://prometheus" }, "http or https URL"},
		"alertmanager url": {func(s *scope) {
			s.Spec.Alertmanager = &alertsv1alpha1.Endpoint{URL: "not a url"}
		}, "http or https URL"},
		"window too short": {func(s *scope) { s.Spec.Quality.Window = "30m" }, "at least 1h"},
		"window too long":  {func(s *scope) { s.Spec.Quality.Window = "91d" }, "at most 90d"},
		"window unit":      {func(s *scope) { s.Spec.Quality.Window = "4w" }, "should match"},
		"resync too short": {func(s *scope) { s.Spec.Quality.ResyncInterval = "30s" }, "at least 1m"},
		"noisy zero":       {func(s *scope) { s.Spec.Quality.Noisy.FiringsAbove = -1 }, "greater than or equal to 1"},
		"flapping per day zero": {func(s *scope) {
			s.Spec.Quality.Flapping.ActivationsPerDayAbove = -3
		}, "greater than or equal to 1"},
		"median zero": {func(s *scope) { s.Spec.Quality.Flapping.MedianDurationBelow = "0m" }, "greater than zero"},
		"silenced fraction above 1": {func(s *scope) {
			s.Spec.Quality.AlwaysSilenced.SilencedFractionAbove = 1.5
		}, "less than or equal to 1"},
		"concurrency": {func(s *scope) { s.Spec.QueryBudget.MaxConcurrent = 100 }, "less than or equal to 16"},
		"team source empty": {func(s *scope) {
			s.Spec.Ownership.TeamFrom = []alertsv1alpha1.TeamSource{{}}
		}, "exactly one of"},
		"team source two": {func(s *scope) {
			s.Spec.Ownership.TeamFrom = []alertsv1alpha1.TeamSource{{RuleLabel: "team", ObjectLabel: "hs/team"}}
		}, "exactly one of"},
	}
	ctx := context.Background()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := validScope("invalid-" + strings.ReplaceAll(name, " ", "-"))
			tc.mutate(s)
			err := admin.Create(ctx, s)
			if err == nil {
				t.Fatalf("scope was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func rule(ns, name string) *alertsv1alpha1.AlertRule {
	return &alertsv1alpha1.AlertRule{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{
			alertsv1alpha1.LabelManagedBy: alertsv1alpha1.ManagedByValue, alertsv1alpha1.LabelScope: "main",
		}},
		Spec: alertsv1alpha1.AlertRuleSpec{
			ScopeRef: "main",
			Source:   alertsv1alpha1.RuleSource{Kind: "PrometheusRule", Namespace: ns, Name: "api", Group: "g", Alert: "A"},
			Expr:     "up == 0",
		},
	}
}

func TestOnlyTheOperatorWritesAlertRules(t *testing.T) {
	ctx := context.Background()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}); err != nil {
		t.Fatal(err)
	}
	// The policy takes a moment to be type-checked and served; until then writes pass.
	deadline := time.Now().Add(30 * time.Second)
	for {
		probe := rule("team-a", "probe")
		err := user.Create(ctx, probe)
		if err != nil && strings.Contains(err.Error(), "written by alert-operator only") {
			break
		}
		if err == nil {
			_ = operator.Delete(ctx, probe)
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy never refused a user's create (last error: %v)", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	r := rule("team-a", "a-3f9c1a")
	if err := operator.Create(ctx, r); err != nil {
		t.Fatalf("operator could not create: %v", err)
	}
	r.Status.Team = "a"
	if err := operator.Status().Update(ctx, r); err != nil {
		t.Fatalf("operator could not update status: %v", err)
	}

	edited := r.DeepCopy()
	edited.Spec.Expr = "vector(1)"
	if err := user.Update(ctx, edited); err == nil || !strings.Contains(err.Error(), "alert-operator only") {
		t.Errorf("user update: want refusal, got %v", err)
	}
	edited = r.DeepCopy()
	edited.Status.Team = "someone-else"
	if err := user.Status().Update(ctx, edited); err == nil || !strings.Contains(err.Error(), "alert-operator only") {
		t.Errorf("user status update: want refusal, got %v", err)
	}
	if err := user.Delete(ctx, r); err == nil || !strings.Contains(err.Error(), "alert-operator only") {
		t.Errorf("user delete: want refusal, got %v", err)
	}
	// Reading is never refused.
	if err := user.Get(ctx, client.ObjectKeyFromObject(r), &alertsv1alpha1.AlertRule{}); err != nil {
		t.Errorf("user read: %v", err)
	}
	if err := operator.Delete(ctx, r, &client.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}); err != nil {
		t.Errorf("operator delete: %v", err)
	}
}

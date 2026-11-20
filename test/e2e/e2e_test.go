//go:build e2e

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

// Package e2e installs everything the way a user would, in a Kind cluster: kube-prometheus-stack
// (prometheus-operator, Prometheus, Alertmanager), a tiny exporter with one PrometheusRule per
// v0.1 finding, a silence made by the test, and the operator from its Helm chart and image. It
// checks the verdicts, the scope status, the metrics, the admission policy, and that no
// PrometheusRule or silence was touched. make test-e2e creates and deletes the cluster.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
)

const (
	kpsVersion   = "93.4.0"
	kpsRepo      = "https://prometheus-community.github.io/helm-charts"
	curlImage    = "curlimages/curl:8.17.0"
	operatorNS   = "alert-operator"
	release      = "alert-operator"
	promResource = "kps-kube-prometheus-stack-prometheus"
)

func tool(name, env string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return name
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := runErr(name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func runErr(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = "../.."
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return b.String(), err
}

func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	return run(t, tool("kubectl", "KUBECTL"), args...)
}

// curl runs curl in a short-lived pod and returns what it printed.
func curl(t *testing.T, name string, args ...string) string {
	t.Helper()
	const ns = "monitoring"
	k := tool("kubectl", "KUBECTL")
	_, _ = runErr(k, "delete", "pod", name, "-n", ns, "--ignore-not-found", "--wait=true")
	run(t, k, append([]string{"run", name, "-n", ns, "--restart=Never", "--image=" + curlImage,
		"--command", "--", "curl", "-sS", "--fail-with-body", "--max-time", "20",
		// A Service can exist before anything serves behind it: retry refused connections and
		// errors for up to two minutes.
		"--retry", "40", "--retry-delay", "3", "--retry-max-time", "120", "--retry-all-errors"}, args...)...)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		phase, _ := runErr(k, "get", "pod", name, "-n", ns, "-o", "jsonpath={.status.phase}")
		if phase == "Succeeded" || phase == "Failed" {
			out, _ := runErr(k, "logs", name, "-n", ns)
			_, _ = runErr(k, "delete", "pod", name, "-n", ns, "--wait=false")
			if phase == "Failed" {
				t.Fatalf("curl %v failed: %s", args, out)
			}
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("curl pod %s did not finish", name)
		}
		time.Sleep(2 * time.Second)
	}
}

func silenceIDs(t *testing.T) string {
	t.Helper()
	out := curl(t, "list-silences", "http://alertmanager-operated.monitoring.svc:9093/api/v2/silences")
	var sil []struct {
		ID        string `json:"id"`
		UpdatedAt string `json:"updatedAt"`
	}
	if err := json.Unmarshal([]byte(out), &sil); err != nil {
		t.Fatalf("silences: %v: %s", err, out)
	}
	parts := make([]string, 0, len(sil))
	for _, s := range sil {
		parts = append(parts, s.ID+"@"+s.UpdatedAt)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

func ruleVersions(t *testing.T) string {
	t.Helper()
	return kubectl(t, "get", "prometheusrules", "-A", "-o",
		`jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}={.metadata.resourceVersion}{"\n"}{end}`)
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", d)
		}
		time.Sleep(5 * time.Second)
	}
}

func TestE2E(t *testing.T) {
	cluster := os.Getenv("KIND_CLUSTER")
	if cluster == "" {
		t.Skip("KIND_CLUSTER is not set: run make test-e2e")
	}
	helm := tool("helm", "HELM")
	image := "example.com/alert-operator:" + cluster

	t.Log("Building and loading the operator image")
	run(t, "docker", "build", "-t", image, ".")
	run(t, tool("kind", "KIND"), "load", "docker-image", image, "--name", cluster)

	t.Log("Installing kube-prometheus-stack " + kpsVersion)
	run(t, helm, "upgrade", "--install", "kps", "kube-prometheus-stack", "--repo", kpsRepo, "--version", kpsVersion,
		"--namespace", "monitoring", "--create-namespace", "-f", "test/e2e/testdata/kube-prometheus-stack.yaml",
		"--wait", "--timeout", "10m")

	// helm --wait returns when the Deployments are ready; the Prometheus and Alertmanager
	// StatefulSets that prometheus-operator creates come after it.
	t.Log("Waiting for Prometheus and Alertmanager to serve")
	for _, sts := range []string{"prometheus-" + promResource, "alertmanager-kps-kube-prometheus-stack-alertmanager"} {
		waitFor(t, 3*time.Minute, func() bool {
			_, err := runErr(tool("kubectl", "KUBECTL"), "get", "statefulset", sts, "-n", "monitoring")
			return err == nil
		})
		kubectl(t, "rollout", "status", "statefulset/"+sts, "-n", "monitoring", "--timeout", "5m")
	}

	t.Log("Applying the fixtures")
	kubectl(t, "apply", "-f", "test/e2e/testdata/fixtures.yaml")
	kubectl(t, "rollout", "status", "deployment/demo-exporter", "-n", "team-a", "--timeout", "3m")
	rulesBefore := ruleVersions(t)

	t.Log("Silencing DemoSilenced in Alertmanager, as a user would")
	silence, _ := json.Marshal(map[string]any{
		"matchers":  []map[string]any{{"name": "alertname", "value": "DemoSilenced", "isRegex": false, "isEqual": true}},
		"startsAt":  time.Now().UTC().Format(time.RFC3339),
		"endsAt":    time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"createdBy": "e2e", "comment": "made by the test, not by the operator",
	})
	curl(t, "create-silence", "-X", "POST", "-H", "Content-Type: application/json", "-d", string(silence),
		"http://alertmanager-operated.monitoring.svc:9093/api/v2/silences")
	silencesBefore := silenceIDs(t)

	t.Log("Installing the operator from its chart")
	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, fmt.Appendf(nil, `image:
  repository: example.com/alert-operator
  tag: %q
  pullPolicy: IfNotPresent
replicaCount: 1
scope:
  create: true
  name: main
  spec:
    prometheus:
      url: http://prometheus-operated.monitoring.svc:9090
      resourceRef: { namespace: monitoring, name: %s }
    alertmanager:
      url: http://alertmanager-operated.monitoring.svc:9093
    quality:
      window: 1h
      resyncInterval: 1m
      noisy: { firingsAbove: 3 }
      flapping: { activationsPerDayAbove: 1, medianDurationBelow: 10m }
      alwaysSilenced: { silencedFractionAbove: 0.5 }
`, cluster, promResource), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, helm, "upgrade", "--install", release, "charts/alert-operator", "--namespace", operatorNS,
		"--create-namespace", "-f", values, "--wait", "--timeout", "5m")
	defer func() {
		if t.Failed() {
			out, _ := runErr(tool("kubectl", "KUBECTL"), "logs", "-n", operatorNS, "deployment/"+release, "--tail=200")
			t.Logf("operator logs:\n%s", out)
		}
	}()

	t.Log("Waiting for the verdicts")
	want := map[string]string{
		"DemoFine":      "",
		"DemoFlapping":  "Noisy,Flapping",
		"DemoDead":      "DeadQuery",
		"DemoSilenced":  "AlwaysSilenced",
		"DemoUnowned":   "Unowned,NoRunbook,NoSeverity",
		"DemoNotLoaded": "NotLoaded,Unowned",
	}
	var got map[string]alertsv1alpha1.AlertRule
	deadline := time.Now().Add(10 * time.Minute)
	for {
		list := &alertsv1alpha1.AlertRuleList{}
		if err := json.Unmarshal([]byte(kubectl(t, "get", "alertrules", "-A", "-o", "json")), list); err != nil {
			t.Fatal(err)
		}
		got = map[string]alertsv1alpha1.AlertRule{}
		for _, r := range list.Items {
			got[r.Spec.Source.Alert] = r
		}
		settled := len(got) == len(want)
		for name, findings := range want {
			if got[name].Status.FindingsSummary != findings {
				settled = false
			}
		}
		if settled {
			break
		}
		if time.Now().After(deadline) {
			for name, r := range got {
				t.Logf("%s: %q (want %q) %+v", name, r.Status.FindingsSummary, want[name], r.Status.Quality)
			}
			t.Fatal("verdicts did not settle")
		}
		time.Sleep(10 * time.Second)
	}
	t.Log("\n" + kubectl(t, "get", "alertrules", "-A"))
	if r := got["DemoNotLoaded"]; !strings.Contains(r.Status.NotLoadedReason, "ruleSelector") {
		t.Errorf("NotLoaded reason: %q", r.Status.NotLoadedReason)
	}
	if r := got["DemoFine"]; r.Status.Team != "a" || r.Labels["hs/team"] != "a" || len(r.OwnerReferences) != 1 {
		t.Errorf("DemoFine: team %q labels %v owners %v", r.Status.Team, r.Labels, r.OwnerReferences)
	}

	scope := &alertsv1alpha1.AlertScope{}
	if err := json.Unmarshal([]byte(kubectl(t, "get", "alertscope", "main", "-o", "json")), scope); err != nil {
		t.Fatal(err)
	}
	if s := scope.Status; s.Rules.Total != 6 || s.Rules.NotLoaded != 1 || s.EffectiveWindow != "1h" || s.LastSync == nil {
		t.Errorf("scope status: %+v", s)
	}
	t.Log("\n" + kubectl(t, "get", "alertscope"))

	t.Log("Only the operator writes AlertRule objects")
	out, err := runErr(tool("kubectl", "KUBECTL"), "delete", "alertrule", got["DemoFine"].Name, "-n", "team-a")
	if err == nil || !strings.Contains(out, "written by alert-operator only") {
		t.Errorf("cluster admin deleted an AlertRule: %v %s", err, out)
	}

	t.Log("Metrics")
	kubectl(t, "create", "serviceaccount", "metrics-check", "-n", "monitoring")
	kubectl(t, "create", "clusterrolebinding", "metrics-check", "--clusterrole="+release+"-metrics-reader",
		"--serviceaccount=monitoring:metrics-check")
	token := strings.TrimSpace(kubectl(t, "create", "token", "metrics-check", "-n", "monitoring"))
	metrics := curl(t, "scrape-operator", "-k", "-H", "Authorization: Bearer "+token,
		fmt.Sprintf("https://%s-metrics.%s.svc:8443/metrics", release, operatorNS))
	for _, want := range []string{`hs_alert_rules{`, `alertname="DemoFlapping"`, `finding="Flapping"`,
		`hs_alert_scope_effective_window_seconds{scope="main"} 3600`, `hs_alert_query_total{`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	t.Log("Nothing the operator does not own was touched")
	if after := ruleVersions(t); after != rulesBefore {
		t.Errorf("PrometheusRules changed:\nbefore %s\nafter  %s", rulesBefore, after)
	}
	if after := silenceIDs(t); after != silencesBefore {
		t.Errorf("silences changed:\nbefore %s\nafter  %s", silencesBefore, after)
	}

	testWebhook(t, helm, values)
	testSilences(t, helm, values)

	t.Log("Deleting the scope removes its AlertRules")
	kubectl(t, "delete", "alertscope", "main", "--wait=true")
	deadline = time.Now().Add(2 * time.Minute)
	for {
		left := strings.TrimSpace(kubectl(t, "get", "alertrules", "-A", "-o", "name"))
		if left == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("AlertRules left after the scope was deleted: %s", left)
		}
		time.Sleep(5 * time.Second)
	}
}

// testWebhook turns the webhook on (chart-signed certificate, there is no cert-manager here),
// enforces it in team-b and leaves team-a in the default warn mode, and runs hsalert lint on
// the same files.
func testWebhook(t *testing.T, helm, values string) {
	t.Helper()
	k := tool("kubectl", "KUBECTL")
	t.Log("Turning the webhook on")
	run(t, helm, "upgrade", release, "charts/alert-operator", "--namespace", operatorNS, "-f", values,
		"--set", "webhook.enabled=true", "--set", "webhook.certificate.certManager=false", "--wait", "--timeout", "5m")
	kubectl(t, "rollout", "status", "deployment/"+release, "-n", operatorNS, "--timeout", "3m")
	kubectl(t, "label", "namespace", "team-b", "alerts.hypersurgery.dev/validation=enforce", "--overwrite")

	t.Log("enforce: a bad PrometheusRule is denied")
	// failurePolicy is Ignore, so until the webhook answers the object would get in: retry
	// until it is refused, removing what slipped through.
	var out string
	waitFor(t, 3*time.Minute, func() bool {
		var err error
		out, err = runErr(k, "apply", "-n", "team-b", "-f", "test/e2e/testdata/bad-rule.yaml")
		if err == nil {
			_, _ = runErr(k, "delete", "prometheusrule", "bad", "-n", "team-b", "--ignore-not-found")
			return false
		}
		return true
	})
	for _, want := range []string{"denied the request", "enforces alerts.hypersurgery.dev/validation",
		"bad/DemoBroken: LoadError", "bad/DemoNobody: Unowned", "bad/DemoNobody: NoSeverity"} {
		if !strings.Contains(out, want) {
			t.Errorf("enforce: output lacks %q:\n%s", want, out)
		}
	}

	t.Log("enforce: a good PrometheusRule is admitted")
	out = kubectl(t, "apply", "-n", "team-b", "-f", "test/e2e/testdata/good-rule.yaml")
	if strings.Contains(out, "LoadError") || strings.Contains(out, "Unowned") || strings.Contains(out, "NotLoaded") {
		t.Errorf("good rule warned: %s", out)
	}

	t.Log("warn: the bad PrometheusRule is admitted with warnings")
	out = kubectl(t, "apply", "-n", "team-a", "-f", "test/e2e/testdata/bad-rule.yaml")
	if !strings.Contains(out, "Warning: alert-operator: bad/DemoBroken: LoadError") ||
		!strings.Contains(out, "prometheusrule.monitoring.coreos.com/bad created") {
		t.Errorf("warn: %s", out)
	}
	kubectl(t, "delete", "prometheusrule", "bad", "-n", "team-a")
	kubectl(t, "delete", "prometheusrule", "good", "-n", "team-b")

	t.Log("hsalert lint")
	bin := filepath.Join(t.TempDir(), "hsalert")
	run(t, "go", "build", "-o", bin, "./cmd/hsalert")
	prom := filepath.Join(t.TempDir(), "prometheus.yaml")
	if err := os.WriteFile(prom, []byte(kubectl(t, "get", "prometheus", promResource, "-n", "monitoring", "-o", "yaml")), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runErr(bin, "lint", "--prometheus", prom, "test/e2e/testdata/bad-rule.yaml"); err == nil ||
		!strings.Contains(out, "LoadError") {
		t.Errorf("hsalert lint on the bad file: %v\n%s", err, out)
	} else if ee, ok := errors.AsType[*exec.ExitError](err); !ok || ee.ExitCode() != 1 {
		t.Errorf("hsalert lint on the bad file: exit %v, want 1", err)
	}
	if out, err := runErr(bin, "lint", "--prometheus", prom, "test/e2e/testdata/good-rule.yaml"); err != nil {
		t.Errorf("hsalert lint on the good file: %v\n%s", err, out)
	}
}

type amSilence struct {
	ID        string `json:"id"`
	CreatedBy string `json:"createdBy"`
	Comment   string `json:"comment"`
	UpdatedAt string `json:"updatedAt"`
	Status    struct {
		State string `json:"state"`
	} `json:"status"`
}

func amSilences(t *testing.T) map[string]amSilence {
	t.Helper()
	out := curl(t, "list-silences", "http://alertmanager-operated.monitoring.svc:9093/api/v2/silences")
	var list []amSilence
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("silences: %v: %s", err, out)
	}
	m := map[string]amSilence{}
	for _, s := range list {
		m[s.ID] = s
	}
	return m
}

func applyYAML(t *testing.T, doc string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "obj.yaml")
	if err := os.WriteFile(f, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	kubectl(t, "apply", "-f", f)
}

func silenceDoc(ns, name, alert, expiry string, dryRun bool) string {
	return fmt.Sprintf(`apiVersion: alerts.hypersurgery.dev/v1alpha1
kind: AlertSilence
metadata: { name: %s, namespace: %s }
spec:
  scopeRef: main
  matchers:
    - { name: alertname, value: %s }
  %s
  owner: a
  reason: "e2e: silences as code, in a real Alertmanager"
  dryRun: %v
`, name, ns, alert, expiry, dryRun)
}

func silenceField(t *testing.T, ns, name, path string) string {
	t.Helper()
	out, _ := runErr(tool("kubectl", "KUBECTL"), "get", "alertsilence", name, "-n", ns, "-o", "jsonpath={"+path+"}")
	return strings.TrimSpace(out)
}

func waitSilenceState(t *testing.T, ns, name, state string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		got := silenceField(t, ns, name, ".status.state")
		if got == state {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("AlertSilence %s/%s is %q (%s), want %s", ns, name, got, silenceField(t, ns, name, ".status.message"), state)
		}
		time.Sleep(5 * time.Second)
	}
}

// testSilences turns writes on with the scope in Apply mode and checks the silences the
// operator creates, updates and expires in the real Alertmanager, and that the silence the test
// made by hand at the start is left exactly as it was.
func testSilences(t *testing.T, helm, values string) {
	t.Helper()
	var human amSilence
	for _, s := range amSilences(t) {
		if s.CreatedBy == "e2e" {
			human = s
		}
	}
	if human.ID == "" {
		t.Fatal("the human silence is missing")
	}

	t.Log("Turning writes on, scope in Apply mode")
	run(t, helm, "upgrade", release, "charts/alert-operator", "--namespace", operatorNS, "-f", values,
		"--set", "webhook.enabled=true", "--set", "webhook.certificate.certManager=false",
		"--set", "writes.enabled=true", "--set", "scope.spec.silences.mode=Apply", "--wait", "--timeout", "5m")
	kubectl(t, "rollout", "status", "deployment/"+release, "-n", operatorNS, "--timeout", "3m")

	t.Log("An AlertSilence for team-a's DemoFlapping becomes Active")
	applyYAML(t, silenceDoc("team-a", "flap", "DemoFlapping", "duration: 1h", false))
	waitSilenceState(t, "team-a", "flap", "Active", 3*time.Minute)
	id := silenceField(t, "team-a", "flap", ".status.silenceID")
	uid := strings.TrimSpace(kubectl(t, "get", "alertsilence", "flap", "-n", "team-a", "-o", "jsonpath={.metadata.uid}"))
	got := amSilences(t)[id]
	if got.CreatedBy != "alert-operator" || got.Status.State != "active" ||
		!strings.HasPrefix(got.Comment, "[alert-operator team-a/flap uid="+uid+"] owner: a; reason: e2e") {
		t.Errorf("silence %s in Alertmanager: %+v", id, got)
	}

	t.Log("A short AlertSilence expires on its own")
	applyYAML(t, silenceDoc("team-a", "short", "DemoFine",
		"expiresAt: \""+time.Now().Add(90*time.Second).UTC().Format(time.RFC3339)+"\"", false))
	waitSilenceState(t, "team-a", "short", "Active", 2*time.Minute)
	waitSilenceState(t, "team-a", "short", "Expired", 4*time.Minute)

	t.Log("Rejected: team-b may not silence team-a's alert")
	applyYAML(t, silenceDoc("team-b", "foreign", "DemoFlapping", "duration: 1h", false))
	waitSilenceState(t, "team-b", "foreign", "Rejected", 2*time.Minute)
	if msg := silenceField(t, "team-b", "foreign", ".status.message"); !strings.Contains(msg, "not a rule of namespace team-b") {
		t.Errorf("rejection: %s", msg)
	}

	t.Log("dryRun only proposes")
	applyYAML(t, silenceDoc("team-a", "dry", "DemoDead", "duration: 1h", true))
	waitSilenceState(t, "team-a", "dry", "Proposed", 2*time.Minute)
	if silenceField(t, "team-a", "dry", ".status.silenceID") != "" || silenceField(t, "team-a", "dry", ".status.proposed.createdBy") != "alert-operator" {
		t.Error("dryRun wrote a silence, or proposed none")
	}

	t.Log("Deleting an AlertSilence expires its silence")
	kubectl(t, "delete", "alertsilence", "flap", "-n", "team-a", "--timeout", "2m")
	if st := amSilences(t)[id].Status.State; st != "expired" {
		t.Errorf("silence %s after delete: %s", id, st)
	}

	t.Log("The silence made by hand is untouched")
	after := amSilences(t)[human.ID]
	if after.UpdatedAt != human.UpdatedAt || after.Status.State != human.Status.State || after.Comment != human.Comment {
		t.Errorf("the human silence changed:\nbefore %+v\nafter  %+v", human, after)
	}
	for _, s := range amSilences(t) {
		if s.CreatedBy == "alert-operator" && !strings.HasPrefix(s.Comment, "[alert-operator team-a/") {
			t.Errorf("a silence for another namespace was written: %+v", s)
		}
	}
	kubectl(t, "delete", "alertsilence", "--all", "-n", "team-a", "--timeout", "2m")
	kubectl(t, "delete", "alertsilence", "--all", "-n", "team-b", "--timeout", "2m")
}

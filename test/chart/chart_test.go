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

// Package chart renders the Helm chart and checks what it would install: the RBAC (no write on
// anything the operator does not own), the admission policy, the dashboard and the alerts.
package chart

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const chartDir = "../../charts/alert-operator"

func helmBin(t *testing.T) string {
	t.Helper()
	if h := os.Getenv("HELM"); h != "" {
		return h
	}
	p, _ := filepath.Abs("../../bin/helm")
	if _, err := os.Stat(p); err != nil {
		t.Skip("helm not found: run make helm, or set HELM")
	}
	return p
}

func render(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(helmBin(t), append([]string{"template", "ao", chartDir, "--namespace", "observability"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	return out
}

type object = map[string]any

func objects(t *testing.T, manifest []byte) []object {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	var out []object
	for {
		var o object
		if err := dec.Decode(&o); err != nil {
			break
		}
		if o != nil {
			out = append(out, o)
		}
	}
	return out
}

func find(objs []object, kind, nameSuffix string) object {
	for _, o := range objs {
		md, _ := o["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		if o["kind"] == kind && strings.HasSuffix(name, nameSuffix) {
			return o
		}
	}
	return nil
}

type rule struct {
	APIGroups       []string `yaml:"apiGroups"`
	Resources       []string `yaml:"resources"`
	Verbs           []string `yaml:"verbs"`
	NonResourceURLs []string `yaml:"nonResourceURLs"`
}

func rulesOf(t *testing.T, o object) []rule {
	t.Helper()
	b, _ := yaml.Marshal(o["rules"])
	var rs []rule
	if err := yaml.Unmarshal(b, &rs); err != nil {
		t.Fatal(err)
	}
	return rs
}

var writeVerbs = []string{"create", "update", "patch", "delete", "deletecollection", "*"}

// The operator's ClusterRole: reads prometheus-operator's objects, owns AlertRule, writes the
// status of AlertScope and Events, and nothing else.
func TestRBACIsLeastPrivilege(t *testing.T) {
	objs := objects(t, render(t))
	cr := find(objs, "ClusterRole", "ao-alert-operator")
	if cr == nil {
		t.Fatal("no ClusterRole")
	}
	allowedWrites := map[string]bool{
		"alerts.hypersurgery.dev/alertrules": true, "alerts.hypersurgery.dev/alertrules/status": true,
		"alerts.hypersurgery.dev/alertscopes/status": true, "events.k8s.io/events": true,
		"authentication.k8s.io/tokenreviews": true, "authorization.k8s.io/subjectaccessreviews": true,
	}
	for _, r := range rulesOf(t, cr) {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				key := g + "/" + res
				if g == "" {
					key = "core/" + res
				}
				if res == "*" || g == "*" || slices.Contains(r.Verbs, "*") {
					t.Errorf("wildcard in %v", r)
				}
				for _, v := range r.Verbs {
					if slices.Contains(writeVerbs, v) && !allowedWrites[key] {
						t.Errorf("write verb %s on %s", v, key)
					}
				}
				if g == "monitoring.coreos.com" && !slices.Equal(r.Verbs, []string{"get", "list", "watch"}) {
					t.Errorf("monitoring.coreos.com/%s: verbs %v, want get/list/watch only", res, r.Verbs)
				}
				if key == "core/secrets" {
					t.Error("Secrets must not be readable cluster-wide")
				}
			}
		}
	}

	// Secrets: get, in the release namespace only.
	role := find(objs, "Role", "ao-alert-operator")
	md := role["metadata"].(map[string]any)
	if md["namespace"] != "observability" {
		t.Errorf("Role namespace %v", md["namespace"])
	}
	for _, r := range rulesOf(t, role) {
		if slices.Contains(r.Resources, "secrets") && !slices.Equal(r.Verbs, []string{"get"}) {
			t.Errorf("secrets verbs %v, want get", r.Verbs)
		}
	}
}

// The chart's ClusterRole carries the rules controller-gen generates from the code's RBAC
// markers, so a new marker cannot ship without the chart granting it.
func TestRBACMatchesGeneratedRole(t *testing.T) {
	gen, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var generated []rule
	for _, o := range objects(t, gen) {
		if o["kind"] == "ClusterRole" {
			generated = rulesOf(t, o)
		}
	}
	chart := rulesOf(t, find(objects(t, render(t)), "ClusterRole", "ao-alert-operator"))
	norm := func(rs []rule) []string {
		var out []string
		for _, r := range rs {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, v := range r.Verbs {
						out = append(out, g+"/"+res+":"+v)
					}
				}
			}
		}
		slices.Sort(out)
		return out
	}
	want, got := norm(generated), norm(chart)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("generated permission missing from the chart: %s", w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) && !strings.HasPrefix(g, "authentication.k8s.io/") && !strings.HasPrefix(g, "authorization.k8s.io/") {
			t.Errorf("chart grants more than the code asks for: %s", g)
		}
	}
}

func TestAdmissionPolicyNamesTheReleaseServiceAccount(t *testing.T) {
	objs := objects(t, render(t))
	vap := find(objs, "ValidatingAdmissionPolicy", "-alertrules")
	b, _ := json.Marshal(vap)
	if !strings.Contains(string(b), "system:serviceaccount:observability:ao-alert-operator") {
		t.Errorf("policy does not allow the release's service account: %s", b)
	}
	binding := find(objs, "ValidatingAdmissionPolicyBinding", "-alertrules")
	if binding["spec"].(map[string]any)["policyName"] != vap["metadata"].(map[string]any)["name"] {
		t.Error("binding does not point at the policy")
	}
	if find(objects(t, render(t, "--set", "alertRuleProtection.enabled=false")), "ValidatingAdmissionPolicy", "-alertrules") != nil {
		t.Error("policy rendered although disabled")
	}
}

// helm template with default values, against a snapshot. UPDATE_GOLDEN=1 rewrites it.
func TestDefaultRenderSnapshot(t *testing.T) {
	got := render(t)
	golden := filepath.Join("testdata", "default.yaml")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the default render changed; check it and run UPDATE_GOLDEN=1 go test ./test/chart/")
	}
}

func TestDashboard(t *testing.T) {
	chartCopy, err := os.ReadFile(chartDir + "/dashboards/alert-quality.json")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../../deploy/grafana/alert-quality.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(chartCopy, source) {
		t.Error("the chart's dashboard differs from deploy/grafana: run make helm-crds")
	}
	var dash struct {
		UID    string `json:"uid"`
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(source, &dash); err != nil {
		t.Fatal(err)
	}
	docs, _ := os.ReadFile("../../docs/metrics.md")
	metricRe := regexp.MustCompile(`hs_alert_[a-z_]+`)
	for _, p := range dash.Panels {
		for _, tg := range p.Targets {
			for _, m := range metricRe.FindAllString(tg.Expr, -1) {
				base := strings.TrimSuffix(m, "_bucket")
				if !strings.Contains(string(docs), "`"+base+"`") {
					t.Errorf("panel %q uses %s, which docs/metrics.md does not list", p.Title, m)
				}
			}
		}
	}
	cm := find(objects(t, render(t, "--set", "grafanaDashboard.enabled=true")), "ConfigMap", "-dashboard")
	data := cm["data"].(map[string]any)["alert-quality.json"].(string)
	if !strings.Contains(data, `"uid": "hs-alert-quality"`) || cm["metadata"].(map[string]any)["labels"].(map[string]any)["grafana_dashboard"] != "1" {
		t.Error("dashboard ConfigMap without the dashboard or the sidecar label")
	}
}

// The operator's own alerts are valid rules, and good ones by its own standards.
func TestOwnAlerts(t *testing.T) {
	pr := find(objects(t, render(t, "--set", "prometheusRule.enabled=true")), "PrometheusRule", "ao-alert-operator")
	if pr == nil {
		t.Fatal("no PrometheusRule")
	}
	spec, _ := yaml.Marshal(pr["spec"])
	var groups struct {
		Groups []struct {
			Rules []map[string]any `yaml:"rules"`
		} `yaml:"groups"`
	}
	_ = yaml.Unmarshal(spec, &groups)
	for _, r := range groups.Groups[0].Rules {
		labels, _ := r["labels"].(map[string]any)
		ann, _ := r["annotations"].(map[string]any)
		if labels["severity"] == nil || labels["team"] == nil || ann["runbook_url"] == nil {
			t.Errorf("%v: needs severity, team and runbook_url", r["alert"])
		}
	}
	promtool := os.Getenv("PROMTOOL")
	if promtool == "" {
		promtool, _ = filepath.Abs("../../bin/promtool")
	}
	if _, err := os.Stat(promtool); err != nil {
		t.Skip("promtool not found")
	}
	f := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(f, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(promtool, "check", "rules", f).CombinedOutput(); err != nil {
		t.Errorf("promtool check rules: %v\n%s", err, out)
	}
}

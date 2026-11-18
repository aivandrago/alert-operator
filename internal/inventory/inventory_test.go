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

package inventory

import (
	"strings"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

var now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

type rdef struct {
	group, alert, expr string
	labels             map[string]string
}

func promRule(ns, name, uid string, objLabels map[string]string, rules ...rdef) monitoringv1.PrometheusRule {
	pr := monitoringv1.PrometheusRule{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name, UID: types.UID(uid), Labels: objLabels,
		CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
	}}
	idx := map[string]int{}
	for _, r := range rules {
		i, ok := idx[r.group]
		if !ok {
			pr.Spec.Groups = append(pr.Spec.Groups, monitoringv1.RuleGroup{Name: r.group})
			i = len(pr.Spec.Groups) - 1
			idx[r.group] = i
		}
		pr.Spec.Groups[i].Rules = append(pr.Spec.Groups[i].Rules, monitoringv1.Rule{
			Alert: r.alert, Expr: intstr.FromString(r.expr), Labels: r.labels,
		})
	}
	return pr
}

func fileOf(ns, name, uid string) string {
	return "/etc/prometheus/rules/prometheus-k8s-rulefiles-0/" + ns + "-" + name + "-" + uid + ".yaml"
}

func loadedGroup(file, group string, alerts ...string) promapi.RuleGroup {
	g := promapi.RuleGroup{Name: group, File: file, Interval: 30}
	for _, a := range alerts {
		g.Rules = append(g.Rules, promapi.Rule{Name: a, Query: "up == 0", Health: "ok", Type: "alerting",
			Labels: map[string]string{"severity": "warning"}})
	}
	return g
}

func baseInput() Input {
	return Input{
		Scope:      "main",
		Namespaces: map[string]map[string]string{"team-a": {"hs/team": "a"}, "team-b": {}, "kube-system": {}},
		InScope:    func(ns string) bool { return ns != "kube-system" },
		Settings:   (&alertsv1alpha1.AlertScopeSpec{}).Settings(),
		Now:        now,
	}
}

func hasFinding(e Entry, t alertsv1alpha1.FindingType) bool {
	for _, f := range e.Findings {
		if f.Type == t {
			return true
		}
	}
	return false
}

func TestMatchedByFile(t *testing.T) {
	in := baseInput()
	in.Rules = []monitoringv1.PrometheusRule{promRule("team-a", "api", "u1", nil,
		rdef{"api.rules", "ApiDown", "up == 0", map[string]string{"severity": "critical"}})}
	in.Loaded = []promapi.RuleGroup{loadedGroup(fileOf("team-a", "api", "u1"), "api.rules", "ApiDown")}
	r := Build(in)
	if len(r.Entries) != 1 {
		t.Fatalf("entries: %+v", r.Entries)
	}
	e := r.Entries[0]
	if !e.Loaded || e.Source.Kind != "PrometheusRule" || e.SourceUID != "u1" || e.Interval != 30*time.Second {
		t.Errorf("entry: %+v", e)
	}
	if e.Team != "a" || hasFinding(e, alertsv1alpha1.FindingUnowned) {
		t.Errorf("team from namespace label: %q %+v", e.Team, e.Findings)
	}
	if hasFinding(e, alertsv1alpha1.FindingNotLoaded) || !hasFinding(e, alertsv1alpha1.FindingNoRunbook) {
		t.Errorf("findings: %+v", e.Findings)
	}
}

func TestNotLoadedReasons(t *testing.T) {
	prom := &monitoringv1.Prometheus{ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "k8s"}}
	prom.Spec.RuleSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"release": "kps"}}
	prom.Spec.RuleNamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"monitored": "yes"}}

	cases := map[string]struct {
		objLabels map[string]string
		nsLabels  map[string]string
		prom      *monitoringv1.Prometheus
		want      string
	}{
		"ruleSelector":   {nil, map[string]string{"monitored": "yes"}, prom, "do not match ruleSelector"},
		"namespace":      {map[string]string{"release": "kps"}, map[string]string{}, prom, "outside ruleNamespaceSelector"},
		"no resourceRef": {nil, nil, nil, "absent from the Prometheus rules API"},
		"selectors match": {map[string]string{"release": "kps"}, map[string]string{"monitored": "yes"}, prom,
			"may have rejected it"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := baseInput()
			in.Namespaces["team-b"] = tc.nsLabels
			in.Prometheus = tc.prom
			in.Rules = []monitoringv1.PrometheusRule{promRule("team-b", "web", "u2", tc.objLabels,
				rdef{"web", "WebDown", "up == 0", nil})}
			e := Build(in).Entries[0]
			if e.Loaded || !hasFinding(e, alertsv1alpha1.FindingNotLoaded) {
				t.Fatalf("want NotLoaded: %+v", e)
			}
			if !strings.Contains(e.NotLoadedReason, tc.want) {
				t.Errorf("reason %q does not say %q", e.NotLoadedReason, tc.want)
			}
			if e.Findings[0].Type != alertsv1alpha1.FindingNotLoaded {
				t.Errorf("critical finding must come first: %+v", e.Findings)
			}
		})
	}
}

func TestNullSelectors(t *testing.T) {
	in := baseInput()
	in.Prometheus = &monitoringv1.Prometheus{ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "k8s"}}
	in.Rules = []monitoringv1.PrometheusRule{promRule("team-a", "api", "u1", nil, rdef{"g", "A", "x", nil})}
	if e := Build(in).Entries[0]; !strings.Contains(e.NotLoadedReason, "no ruleSelector") {
		t.Errorf("null ruleSelector: %q", e.NotLoadedReason)
	}
	in.Prometheus.Spec.RuleSelector = &metav1.LabelSelector{}
	if e := Build(in).Entries[0]; !strings.Contains(e.NotLoadedReason, "only its own namespace monitoring") {
		t.Errorf("null ruleNamespaceSelector: %q", e.NotLoadedReason)
	}
}

func TestPendingWithinGrace(t *testing.T) {
	in := baseInput()
	pr := promRule("team-a", "new", "u9", nil, rdef{"g", "Fresh", "x", nil})
	pr.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
	in.Rules = []monitoringv1.PrometheusRule{pr}
	e := Build(in).Entries[0]
	if e.Loaded || !e.Pending || hasFinding(e, alertsv1alpha1.FindingNotLoaded) {
		t.Errorf("a minute-old object should be pending, not NotLoaded: %+v", e)
	}
}

func TestFileOnlyAndOutOfScope(t *testing.T) {
	in := baseInput()
	in.Rules = []monitoringv1.PrometheusRule{promRule("kube-system", "k", "u3", nil, rdef{"kube", "KubeDown", "x", nil})}
	in.Loaded = []promapi.RuleGroup{
		loadedGroup("/etc/prometheus/extra/node.yml", "node", "NodeDown"),
		loadedGroup(fileOf("kube-system", "k", "u3"), "kube", "KubeDown"),
	}
	r := Build(in)
	if len(r.Entries) != 1 {
		t.Fatalf("want only the file rule, got %+v", r.Entries)
	}
	e := r.Entries[0]
	if e.Source.Kind != "File" || e.Source.File != "/etc/prometheus/extra/node.yml" || !e.Loaded || e.Expr != "up == 0" {
		t.Errorf("file entry: %+v", e)
	}
	if e.Team != alertsv1alpha1.NoTeam || !hasFinding(e, alertsv1alpha1.FindingUnowned) {
		t.Errorf("file rule without team label must be Unowned: %+v", e)
	}
}

func TestFallbackMatchByName(t *testing.T) {
	in := baseInput()
	in.Rules = []monitoringv1.PrometheusRule{promRule("team-a", "api", "u1", nil, rdef{"api", "ApiDown", "x", nil})}
	// A Prometheus not run by prometheus-operator loads the same rules from its own path.
	in.Loaded = []promapi.RuleGroup{loadedGroup("/rules/api.yaml", "api", "ApiDown")}
	r := Build(in)
	if len(r.Entries) != 1 || !r.Entries[0].Loaded || r.Entries[0].Source.Kind != "PrometheusRule" {
		t.Errorf("fallback by group and alert name: %+v", r.Entries)
	}
}

func TestRenamedGroup(t *testing.T) {
	in := baseInput()
	before := promRule("team-a", "api", "u1", nil, rdef{"api.v1", "ApiDown", "x", nil})
	in.Rules = []monitoringv1.PrometheusRule{before}
	in.Loaded = []promapi.RuleGroup{loadedGroup(fileOf("team-a", "api", "u1"), "api.v1", "ApiDown")}
	oldID := Build(in).Entries[0].Identity

	// Renamed, not reloaded yet: the new group is reported not loaded, the stale one ignored.
	in.Rules = []monitoringv1.PrometheusRule{promRule("team-a", "api", "u1", nil, rdef{"api.v2", "ApiDown", "x", nil})}
	r := Build(in)
	if len(r.Entries) != 1 || r.Entries[0].Loaded || r.Entries[0].Source.Group != "api.v2" {
		t.Fatalf("before reload: %+v", r.Entries)
	}
	// Reloaded: loaded again, under a new identity (the group is part of it).
	in.Loaded = []promapi.RuleGroup{loadedGroup(fileOf("team-a", "api", "u1"), "api.v2", "ApiDown")}
	r = Build(in)
	if len(r.Entries) != 1 || !r.Entries[0].Loaded || r.Entries[0].Identity == oldID {
		t.Errorf("after reload: %+v (old id %s)", r.Entries, oldID)
	}
}

func TestSameAlertInTwoGroups(t *testing.T) {
	in := baseInput()
	in.Rules = []monitoringv1.PrometheusRule{promRule("team-a", "api", "u1", nil,
		rdef{"latency", "ApiSlow", "a > 1", nil}, rdef{"errors", "ApiSlow", "b > 1", nil},
		rdef{"errors", "ApiSlow", "c > 1", nil})}
	file := fileOf("team-a", "api", "u1")
	in.Loaded = []promapi.RuleGroup{loadedGroup(file, "latency", "ApiSlow"), loadedGroup(file, "errors", "ApiSlow", "ApiSlow")}
	r := Build(in)
	if len(r.Entries) != 3 {
		t.Fatalf("entries: %+v", r.Entries)
	}
	ids := map[string]bool{}
	for _, e := range r.Entries {
		if !e.Loaded {
			t.Errorf("not loaded: %+v", e)
		}
		ids[e.Identity] = true
	}
	if len(ids) != 3 {
		t.Errorf("identities collide: %v", ids)
	}
}

func TestIdentitySurvivesEdits(t *testing.T) {
	in := baseInput()
	in.Rules = []monitoringv1.PrometheusRule{promRule("team-a", "api", "u1", nil,
		rdef{"g", "A", "up == 0", map[string]string{"severity": "warning"}})}
	a := Build(in).Entries[0].Identity
	in.Rules = []monitoringv1.PrometheusRule{promRule("team-a", "api", "u1", nil,
		rdef{"g", "A", "up < 1", map[string]string{"severity": "critical", "team": "x"}})}
	b := Build(in).Entries[0].Identity
	if a != b || len(a) != 8 {
		t.Errorf("identity changed with the expression: %s vs %s", a, b)
	}
	other := Identity("other-scope", alertsv1alpha1.RuleSource{Kind: "PrometheusRule", Namespace: "team-a",
		Name: "api", Group: "g", Alert: "A"}, 0)
	if other == a {
		t.Error("identity must include the scope")
	}
}

func TestLoadError(t *testing.T) {
	in := baseInput()
	in.Rules = []monitoringv1.PrometheusRule{promRule("team-a", "api", "u1", nil, rdef{"g", "A", "x", nil})}
	g := loadedGroup(fileOf("team-a", "api", "u1"), "g", "A")
	g.Rules[0].Health = "err"
	g.Rules[0].LastError = "many-to-many matching not allowed"
	in.Loaded = []promapi.RuleGroup{g}
	e := Build(in).Entries[0]
	if !e.Loaded || !hasFinding(e, alertsv1alpha1.FindingLoadError) ||
		!strings.Contains(e.Findings[0].Message, "many-to-many") {
		t.Errorf("LoadError: %+v", e)
	}
}

func TestResolveTeamOrder(t *testing.T) {
	obj := &monitoringv1.PrometheusRule{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Labels: map[string]string{"hs/team": "obj"}}}
	nss := map[string]map[string]string{"ns": {"hs/team": "namespace"}}
	def := (&alertsv1alpha1.AlertScopeSpec{}).Settings().TeamFrom
	cases := []struct {
		name    string
		sources []alertsv1alpha1.TeamSource
		rule    map[string]string
		obj     *monitoringv1.PrometheusRule
		want    string
	}{
		{"rule label first", def, map[string]string{"team": "rule"}, obj, "rule"},
		{"then object label", def, nil, obj, "obj"},
		{"then namespace label", def, nil, &monitoringv1.PrometheusRule{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}}, "namespace"},
		{"blank values are skipped", def, map[string]string{"team": " "}, obj, "obj"},
		{"custom order", []alertsv1alpha1.TeamSource{{NamespaceLabel: "hs/team"}, {RuleLabel: "team"}},
			map[string]string{"team": "rule"}, obj, "namespace"},
		{"file rule: rule labels only", def, nil, nil, ""},
		{"nothing", def, nil, &monitoringv1.PrometheusRule{ObjectMeta: metav1.ObjectMeta{Namespace: "other"}}, ""},
	}
	for _, tc := range cases {
		if got, _ := ResolveTeam(tc.sources, tc.rule, tc.obj, nss); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHygieneFindings(t *testing.T) {
	s := (&alertsv1alpha1.AlertScopeSpec{}).Settings()
	run := func(labels, ann map[string]string, s alertsv1alpha1.Settings) Entry {
		e := Entry{Labels: labels, Annotations: ann}
		applyOwnership(&e, nil, nil, s)
		return e
	}
	e := run(map[string]string{"team": "a", "severity": "warning"}, map[string]string{"runbook_url": "https://r"}, s)
	if len(e.Findings) != 0 || e.Team != "a" {
		t.Errorf("clean rule: %+v", e)
	}
	e = run(map[string]string{"team": "a"}, nil, s)
	if !hasFinding(e, alertsv1alpha1.FindingNoRunbook) || !hasFinding(e, alertsv1alpha1.FindingNoSeverity) {
		t.Errorf("missing runbook and severity: %+v", e.Findings)
	}
	e = run(map[string]string{"team": "a", "severity": "page"}, map[string]string{"runbook_url": " "}, s)
	if !hasFinding(e, alertsv1alpha1.FindingNoRunbook) || !strings.Contains(e.Findings[1].Message, `"page"`) {
		t.Errorf("blank runbook, unknown severity: %+v", e.Findings)
	}
	s.RequireRunbook = false
	s.RequireSeverity = []string{"page"}
	e = run(map[string]string{"team": "a", "severity": "page"}, nil, s)
	if len(e.Findings) != 0 {
		t.Errorf("runbook not required, custom severity allowed: %+v", e.Findings)
	}
}

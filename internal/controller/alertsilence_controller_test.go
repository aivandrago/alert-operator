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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

// silenceAM is an Alertmanager holding silences in memory, with one silence made by a person.
type silenceAM struct {
	mu       sync.Mutex
	silences map[string]*promapi.Silence
	writes   []string // METHOD id
	down     bool
	next     int
	srv      *httptest.Server
}

func newSilenceAM() *silenceAM {
	f := &silenceAM{silences: map[string]*promapi.Silence{}}
	human := &promapi.Silence{ID: "human", CreatedBy: "alice", Comment: "maintenance window",
		Matchers: []promapi.Matcher{{Name: "alertname", Value: "ApiDown"}}, EndsAt: time.Now().Add(time.Hour)}
	human.Status.State = "active"
	f.silences["human"] = human
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *silenceAM) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.URL.Path == "/api/v2/alerts":
		_, _ = fmt.Fprint(w, `[{"labels":{"alertname":"ApiDown","job":"api"},"status":{"state":"active"}},
			{"labels":{"alertname":"WebDown"},"status":{"state":"active"}}]`)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/silences":
		var p promapi.PostableSilence
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &p)
		id := p.ID
		old := f.silences[id]
		if id == "" || old == nil || old.Status.State == "expired" || !sameMatchers(old.Matchers, p.Matchers) {
			if old != nil {
				old.Status.State = "expired"
			}
			f.next++
			id = fmt.Sprintf("s%d", f.next)
		}
		s := &promapi.Silence{ID: id, Matchers: p.Matchers, StartsAt: p.StartsAt, EndsAt: p.EndsAt,
			CreatedBy: p.CreatedBy, Comment: p.Comment}
		s.Status.State = "active"
		f.silences[id] = s
		f.writes = append(f.writes, "POST "+id)
		_, _ = fmt.Fprintf(w, `{"silenceID":%q}`, id)
	case strings.HasPrefix(r.URL.Path, "/api/v2/silence/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/silence/")
		s := f.silences[id]
		if s == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			s.Status.State = "expired"
			f.writes = append(f.writes, "DELETE "+id)
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	default:
		http.NotFound(w, r)
	}
}

func sameMatchers(a, b []promapi.Matcher) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (f *silenceAM) humanUntouched(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.silences["human"].Status.State != "active" || f.silences["human"].Comment != "maintenance window" {
		t.Errorf("the human silence changed: %+v", f.silences["human"])
	}
	for _, w := range f.writes {
		if strings.HasSuffix(w, " human") {
			t.Errorf("a write touched the human silence: %s", w)
		}
	}
}

func TestSilenceLifecycle(t *testing.T) {
	ctx := context.Background()
	am := newSilenceAM()
	defer am.srv.Close()
	clk := &clock{t: time.Now()}
	rec := events.NewFakeRecorder(100)
	r := &AlertSilenceReconciler{Client: k8s, APIReader: k8s, Scheme: scheme, Recorder: rec,
		OperatorNamespace: "alert-operator-system", EnableWrites: true, Now: clk.now}

	for _, ns := range []string{"sil-a", "sil-b"} {
		if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	mode := func(m alertsv1alpha1.SilenceMode) {
		s := &alertsv1alpha1.AlertScope{}
		if err := k8s.Get(ctx, types.NamespacedName{Name: "silences"}, s); err != nil {
			s = &alertsv1alpha1.AlertScope{ObjectMeta: metav1.ObjectMeta{Name: "silences"}, Spec: alertsv1alpha1.AlertScopeSpec{
				Prometheus:   alertsv1alpha1.PrometheusEndpoint{Endpoint: alertsv1alpha1.Endpoint{URL: am.srv.URL}},
				Alertmanager: &alertsv1alpha1.Endpoint{URL: am.srv.URL},
				Silences:     alertsv1alpha1.SilencePolicy{Mode: m, MaxDuration: "1d"},
			}}
			if err := k8s.Create(ctx, s); err != nil {
				t.Fatal(err)
			}
			return
		}
		s.Spec.Silences.Mode = m
		if err := k8s.Update(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	mode(alertsv1alpha1.SilenceModeApply)
	// The inventory: ApiDown is a rule of sil-a, Shared of both namespaces.
	for _, m := range []struct{ ns, name, alert string }{
		{"sil-a", "apidown-000001", "ApiDown"}, {"sil-a", "shared-000002", "Shared"}, {"sil-b", "shared-000003", "Shared"},
	} {
		ar := &alertsv1alpha1.AlertRule{ObjectMeta: metav1.ObjectMeta{Namespace: m.ns, Name: m.name, Labels: map[string]string{
			alertsv1alpha1.LabelManagedBy: alertsv1alpha1.ManagedByValue, alertsv1alpha1.LabelScope: "silences"}},
			Spec: alertsv1alpha1.AlertRuleSpec{ScopeRef: "silences", Expr: "up == 0",
				Source: alertsv1alpha1.RuleSource{Kind: "PrometheusRule", Namespace: m.ns, Name: "x", Group: "g", Alert: m.alert}}}
		if err := k8s.Create(ctx, ar); err != nil {
			t.Fatal(err)
		}
	}

	newSilence := func(ns, name, alert, duration string) *alertsv1alpha1.AlertSilence {
		t.Helper()
		s := &alertsv1alpha1.AlertSilence{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: alertsv1alpha1.AlertSilenceSpec{ScopeRef: "silences", Duration: alertsv1alpha1.Duration(duration),
				Matchers: []alertsv1alpha1.SilenceMatcher{{Name: "alertname", Value: alert}},
				Owner:    "a", Reason: "database migration CHG-1234"}}
		if err := k8s.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	reconcile := func(s *alertsv1alpha1.AlertSilence) *alertsv1alpha1.AlertSilence {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(s)}); err != nil {
			t.Fatal(err)
		}
		got := &alertsv1alpha1.AlertSilence{}
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(s), got); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			t.Fatal(err)
		}
		return got
	}
	drain(rec)

	// Apply: created, Active, marked as ours, with a finalizer.
	s := reconcile(newSilence("sil-a", "api", "ApiDown", "4h"))
	if s.Status.State != alertsv1alpha1.SilenceActive || s.Status.SilenceID != "s1" || s.Status.MatchedAlerts != 1 ||
		len(s.Finalizers) != 1 {
		t.Fatalf("apply: %+v %v", s.Status, s.Finalizers)
	}
	if c := am.silences["s1"]; c.CreatedBy != "alert-operator" ||
		!strings.HasPrefix(c.Comment, "[alert-operator sil-a/api uid="+string(s.UID)+"] owner: a; reason: database migration") {
		t.Errorf("silence in Alertmanager: %+v", c)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.Contains(ev[0], "SilenceCreated") {
		t.Errorf("events: %v", ev)
	}
	// Reconciling again changes nothing.
	n := len(am.writes)
	s = reconcile(s)
	if len(am.writes) != n || s.Status.State != alertsv1alpha1.SilenceActive {
		t.Errorf("second reconcile wrote: %v", am.writes[n:])
	}

	// A spec change updates the silence (same matchers: same ID in Alertmanager's semantics).
	s.Spec.Reason = "database migration CHG-1234, extended"
	if err := k8s.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	s = reconcile(s)
	if s.Status.SilenceID != "s1" || !strings.HasSuffix(am.silences["s1"].Comment, "extended") {
		t.Errorf("update: %+v", s.Status)
	}

	// Someone ends it in Alertmanager: it is not recreated.
	am.mu.Lock()
	am.silences["s1"].Status.State = "expired"
	am.mu.Unlock()
	n = len(am.writes)
	s = reconcile(s)
	if s.Status.State != alertsv1alpha1.SilenceExpiredExternally || len(am.writes) != n {
		t.Errorf("expired externally: %+v, writes %v", s.Status, am.writes[n:])
	}

	// Its end: Expired.
	s2 := reconcile(newSilence("sil-a", "short", "ApiDown", "1h"))
	if s2.Status.State != alertsv1alpha1.SilenceActive {
		t.Fatalf("short: %+v", s2.Status)
	}
	clk.advance(2 * time.Hour)
	if s2 = reconcile(s2); s2.Status.State != alertsv1alpha1.SilenceExpired {
		t.Errorf("after its end: %+v", s2.Status)
	}
	clk.advance(-2 * time.Hour)

	// Deleted: the silence is expired, the object goes.
	s3 := reconcile(newSilence("sil-a", "gone", "ApiDown", "2h"))
	id := s3.Status.SilenceID
	if err := k8s.Delete(ctx, s3); err != nil {
		t.Fatal(err)
	}
	if reconcile(s3) != nil || am.silences[id].Status.State != "expired" {
		t.Errorf("delete: silence %s is %s", id, am.silences[id].Status.State)
	}

	// Deleted while Alertmanager is down: kept for DeleteRetry, then let go.
	s4 := reconcile(newSilence("sil-a", "outage", "ApiDown", "2h"))
	if err := k8s.Delete(ctx, s4); err != nil {
		t.Fatal(err)
	}
	am.mu.Lock()
	am.down = true
	am.mu.Unlock()
	if reconcile(s4) == nil {
		t.Error("let go while Alertmanager was down for less than DeleteRetry")
	}
	clk.advance(DeleteRetry + time.Minute)
	if reconcile(s4) != nil {
		t.Error("still held after DeleteRetry")
	}
	clk.advance(-(DeleteRetry + time.Minute))
	am.mu.Lock()
	am.down = false
	am.mu.Unlock()

	// Rejections: nothing is written.
	n = len(am.writes)
	for name, want := range map[string]struct{ alert, duration, msg string }{
		"other-ns": {"ApiDown", "1h", "not a rule of namespace sil-b"},
		"shared":   {"Shared", "1h", "rule of several namespaces"},
		"too-long": {"ApiDown", "2d", "longer than silences.maxDuration"},
		"no-such":  {"Missing", "1h", "not a rule of namespace"},
	} {
		ns := "sil-a"
		if name == "other-ns" {
			ns = "sil-b"
		}
		got := reconcile(newSilence(ns, name, want.alert, want.duration))
		if got.Status.State != alertsv1alpha1.SilenceRejected || !strings.Contains(got.Status.Message, want.msg) {
			t.Errorf("%s: %+v", name, got.Status)
		}
	}
	matchAll := newSilence("sil-a", "match-all", "ApiDown", "1h")
	matchAll.Spec.Matchers = []alertsv1alpha1.SilenceMatcher{{Name: "alertname", Value: ".*", IsRegex: true}}
	if err := k8s.Update(ctx, matchAll); err != nil {
		t.Fatal(err)
	}
	if got := reconcile(matchAll); got.Status.State != alertsv1alpha1.SilenceRejected || !strings.Contains(got.Status.Message, "match every alert") {
		t.Errorf("match-all: %+v", got.Status)
	}
	if len(am.writes) != n {
		t.Errorf("rejected objects wrote: %v", am.writes[n:])
	}

	// Propose: by dryRun, by the scope's mode, without --enable-writes. Nothing written.
	dry := newSilence("sil-a", "dry", "ApiDown", "1h")
	dry.Spec.DryRun = true
	if err := k8s.Update(ctx, dry); err != nil {
		t.Fatal(err)
	}
	if got := reconcile(dry); got.Status.State != alertsv1alpha1.SilenceProposed || got.Status.Proposed == nil ||
		got.Status.MatchedAlerts != 1 || !strings.Contains(got.Status.Message, "dryRun") {
		t.Errorf("dryRun: %+v", got.Status)
	}
	r.EnableWrites = false
	if got := reconcile(newSilence("sil-a", "nowrites", "ApiDown", "1h")); got.Status.State != alertsv1alpha1.SilenceProposed ||
		!strings.Contains(got.Status.Message, "--enable-writes") {
		t.Errorf("without writes: %+v", got.Status)
	}
	r.EnableWrites = true
	mode(alertsv1alpha1.SilenceModePropose)
	if got := reconcile(newSilence("sil-a", "proposed", "ApiDown", "1h")); got.Status.State != alertsv1alpha1.SilenceProposed {
		t.Errorf("Propose mode: %+v", got.Status)
	}
	mode(alertsv1alpha1.SilenceModeOff)
	if got := reconcile(newSilence("sil-a", "off", "ApiDown", "1h")); got.Status.State != alertsv1alpha1.SilenceDisabled {
		t.Errorf("Off mode: %+v", got.Status)
	}
	if len(am.writes) != n {
		t.Errorf("proposals wrote: %v", am.writes[n:])
	}

	am.humanUntouched(t)
}

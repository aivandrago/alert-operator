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

package analyze

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/promapi"
)

// This test runs the quality analyzer against a real Prometheus. promtool backfills a TSDB
// with the ALERTS and ALERTS_FOR_STATE series a Prometheus writes for its alerting rules, and
// Prometheus serves it with a retention shorter than the configured window.

func tool(t *testing.T, env, name string) string {
	t.Helper()
	if p := os.Getenv(env); p != "" {
		return p
	}
	p, _ := filepath.Abs(filepath.Join("..", "..", "bin", name))
	if _, err := os.Stat(p); err != nil {
		t.Skipf("%s not found: run make prometheus, or set %s", name, env)
	}
	return p
}

// alertHistory writes the series of one alert that fired over the given periods, sampled at
// the rule evaluation interval, the way Prometheus records them.
type alertHistory struct {
	labels  string // without alertname/alertstate, e.g. `severity="warning"`
	alert   string
	periods [][2]time.Time
}

const evalInterval = 30 * time.Second

func writeOpenMetrics(t *testing.T, path string, hs []alertHistory) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	lbl := func(h alertHistory, extra string) string {
		parts := []string{fmt.Sprintf("alertname=%q", h.alert)}
		if extra != "" {
			parts = append(parts, extra)
		}
		if h.labels != "" {
			parts = append(parts, h.labels)
		}
		slices.Sort(parts)
		return "{" + strings.Join(parts, ",") + "}"
	}
	_, _ = fmt.Fprintln(w, "# TYPE ALERTS gauge")
	for _, h := range hs {
		for _, p := range h.periods {
			for ts := p[0]; !ts.After(p[1]); ts = ts.Add(evalInterval) {
				_, _ = fmt.Fprintf(w, "ALERTS%s 1 %d\n", lbl(h, `alertstate="firing"`), ts.Unix())
			}
		}
	}
	_, _ = fmt.Fprintln(w, "# TYPE ALERTS_FOR_STATE gauge")
	for _, h := range hs {
		for _, p := range h.periods {
			for ts := p[0]; !ts.After(p[1]); ts = ts.Add(evalInterval) {
				_, _ = fmt.Fprintf(w, "ALERTS_FOR_STATE%s %d %d\n", lbl(h, ""), p[0].Unix(), ts.Unix())
			}
		}
	}
	_, _ = fmt.Fprintln(w, "# EOF")
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func startPrometheus(t *testing.T, hs []alertHistory, retention string) string {
	t.Helper()
	prom, promtool := tool(t, "PROMETHEUS", "prometheus"), tool(t, "PROMTOOL", "promtool")
	dir := t.TempDir()
	om := filepath.Join(dir, "history.om")
	writeOpenMetrics(t, om, hs)
	data := filepath.Join(dir, "data")
	if out, err := exec.Command(promtool, "tsdb", "create-blocks-from", "openmetrics", om, data).CombinedOutput(); err != nil {
		t.Fatalf("promtool: %v\n%s", err, out)
	}
	cfg := filepath.Join(dir, "prometheus.yml")
	if err := os.WriteFile(cfg, []byte("global:\n  evaluation_interval: 30s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cmd := exec.Command(prom, "--config.file="+cfg, "--storage.tsdb.path="+data,
		"--storage.tsdb.retention.time="+retention, "--web.listen-address="+addr, "--log.level=warn")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	url := "http://" + addr
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(url + "/-/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return url
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("prometheus did not become ready: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// measureFixture fills a Prometheus with the history of six rules, two days of retention, and
// measures them over a 30-day window (capped to two days).
func measureFixture(t *testing.T) (got map[string]Measured, now time.Time, window time.Duration) {
	t.Helper()
	now = time.Now().Truncate(time.Minute).Add(-10 * time.Minute)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	flappy := make([][2]time.Time, 0, 72)
	for i := range 72 { // every 30 minutes for 36 hours, firing for two minutes
		start := ago(36*time.Hour - time.Duration(i)*30*time.Minute)
		flappy = append(flappy, [2]time.Time{start, start.Add(2 * time.Minute)})
	}
	dupA := make([][2]time.Time, 0, 3)
	for i := range 3 {
		start := ago(time.Duration(30-i*5) * time.Hour)
		dupA = append(dupA, [2]time.Time{start, start.Add(20 * time.Minute)})
	}
	history := []alertHistory{
		{alert: "Flappy", labels: `severity="warning"`, periods: flappy},
		{alert: "Steady", labels: `severity="critical"`, periods: [][2]time.Time{{ago(10 * time.Hour), ago(4 * time.Hour)}}},
		{alert: "Muted", labels: `severity="warning"`, periods: [][2]time.Time{{ago(20 * time.Hour), ago(2 * time.Hour)}}},
		{alert: "Dup", labels: `team="a"`, periods: dupA},
		{alert: "Dup", labels: `team="b"`, periods: [][2]time.Time{{ago(3 * time.Hour), ago(2 * time.Hour)}}},
	}
	url := startPrometheus(t, history, "2d")

	p, err := promapi.NewPrometheus(url, promapi.Auth{}, promapi.NewBudget(10*time.Second, 2, 600, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	retention, known, err := p.Retention(ctx)
	if err != nil || !known || retention != 48*time.Hour {
		t.Fatalf("retention: %v %v %v", retention, known, err)
	}
	window = EffectiveWindow(30*24*time.Hour, retention, known)
	if window != 48*time.Hour {
		t.Fatalf("window not capped to retention: %v", window)
	}

	falseVal := false
	silences := []promapi.Silence{{
		Matchers: []promapi.Matcher{{Name: "alertname", Value: "Muted"}},
		StartsAt: ago(19 * time.Hour), EndsAt: now.Add(time.Hour),
	}, {
		// Matches nothing: every Steady alert has severity="critical", which this silence excludes.
		Matchers: []promapi.Matcher{{Name: "alertname", Value: "Steady"}, {Name: "severity", Value: "critical", IsEqual: &falseVal}},
		StartsAt: ago(48 * time.Hour), EndsAt: now,
	}}
	q := &Quality{P: p, Silences: silences, Now: now, Window: window}
	rules := []RuleRef{
		{Key: "flappy", Alert: "Flappy", Labels: map[string]string{"severity": "warning"}, Interval: evalInterval},
		{Key: "steady", Alert: "Steady", Labels: map[string]string{"severity": "critical"}, Interval: evalInterval},
		{Key: "muted", Alert: "Muted", Interval: evalInterval},
		{Key: "quiet", Alert: "Quiet", Interval: evalInterval},
		{Key: "dup-a", Alert: "Dup", Labels: map[string]string{"team": "a", "instance": "{{ $labels.instance }}"}, Interval: evalInterval},
		{Key: "dup-b", Alert: "Dup", Labels: map[string]string{"team": "b"}, Interval: evalInterval},
	}
	got, err = q.MeasureGroup(ctx, rules)
	if err != nil {
		t.Fatal(err)
	}
	return got, now, window
}

func TestQualityAgainstPrometheus(t *testing.T) {
	got, now, window := measureFixture(t)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	settings := (&alertsv1alpha1.AlertScopeSpec{}).Settings()
	verdict := func(key string) []alertsv1alpha1.FindingType {
		fs := Verdicts(got[key], VerdictInput{Settings: settings, Known: 60 * 24 * time.Hour, Silences: true, Window: window})
		out := make([]alertsv1alpha1.FindingType, 0, len(fs))
		for _, f := range fs {
			out = append(out, f.Type)
		}
		return out
	}
	near := func(d, want, tol time.Duration) bool { return d >= want-tol && d <= want+tol }

	f := got["flappy"]
	if f.Activations != 72 || f.PerDay != 36 || !near(f.Median, 2*time.Minute, time.Minute) {
		t.Errorf("flappy: %d activations, %.1f/day, median %v", f.Activations, f.PerDay, f.Median)
	}
	if v := verdict("flappy"); !equalTypes(v, alertsv1alpha1.FindingNoisy, alertsv1alpha1.FindingFlapping) {
		t.Errorf("flappy verdicts: %v", v)
	}

	s := got["steady"]
	if s.Activations != 1 || !near(s.Median, 6*time.Hour, 2*time.Minute) || !near(s.LastFired.Sub(ago(4*time.Hour)), 0, time.Minute) {
		t.Errorf("steady: %+v", s)
	}
	if !(s.FiringFraction > 0.12 && s.FiringFraction < 0.13) || s.SilencedFraction != 0 {
		t.Errorf("steady fractions: firing %.3f silenced %.3f", s.FiringFraction, s.SilencedFraction)
	}
	if v := verdict("steady"); len(v) != 0 {
		t.Errorf("steady verdicts: %v", v)
	}

	m := got["muted"]
	if m.SilencedFraction < 0.93 || m.SilencedFraction > 0.96 {
		t.Errorf("muted silenced fraction %.3f, want about 17/18", m.SilencedFraction)
	}
	if v := verdict("muted"); !equalTypes(v, alertsv1alpha1.FindingAlwaysSilenced) {
		t.Errorf("muted verdicts: %v", v)
	}

	if qt := got["quiet"]; qt.Activations != 0 || len(qt.Runs) != 0 || !qt.LastFired.IsZero() {
		t.Errorf("quiet: %+v", qt)
	}
	if v := verdict("quiet"); !equalTypes(v, alertsv1alpha1.FindingNeverFired) {
		t.Errorf("quiet verdicts: %v", v)
	}
	// Not known long enough: no NeverFired yet.
	if v := Verdicts(got["quiet"], VerdictInput{Settings: settings, Known: time.Hour, Window: window}); len(v) != 0 {
		t.Errorf("a rule known for an hour must not be NeverFired: %v", v)
	}

	if a, b := got["dup-a"], got["dup-b"]; a.Activations != 3 || b.Activations != 1 {
		t.Errorf("same alert name told apart by label: a=%d b=%d", a.Activations, b.Activations)
	}
}

func equalTypes(got []alertsv1alpha1.FindingType, want ...alertsv1alpha1.FindingType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

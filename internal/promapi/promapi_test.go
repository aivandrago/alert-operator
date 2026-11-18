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

package promapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type countingRecorder struct {
	mu        sync.Mutex
	queries   map[string]int
	exhausted int
}

func (r *countingRecorder) Query(target, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.queries == nil {
		r.queries = map[string]int{}
	}
	r.queries[target+"/"+result]++
}

func (r *countingRecorder) BudgetExhausted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exhausted++
}

const rulesBody = `{"status":"success","data":{"groups":[{"name":"payments.latency",
"file":"/etc/prometheus/rules/prometheus-k8s-rulefiles-0/team-payments-payments-api-1b2c.yaml","interval":30,
"rules":[{"name":"PaymentsHighLatency","query":"x > 0.5","duration":300,"labels":{"severity":"warning"},
"annotations":{"summary":"s"},"health":"ok","type":"alerting","state":"inactive"},
{"name":"payments:rate5m","query":"rate(x[5m])","health":"ok","type":"recording"}]}]}}`

func fakePrometheus(t *testing.T, check func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/rules":
			_, _ = fmt.Fprint(w, rulesBody)
		case "/api/v1/query":
			switch r.URL.Query().Get("query") {
			case "bad(":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
			case "scalar(1)":
				_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"scalar","result":[1700000000.5,"1"]}}`)
			default:
				_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
					{"metric":{"alertname":"A"},"value":[1700000000,"3"]}]}}`)
			}
		case "/api/v1/query_range":
			_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"alertname":"A"},"values":[[1700000000,"1"],[1700000060,"1"]]}]}}`)
		case "/api/v1/status/runtimeinfo":
			_, _ = fmt.Fprint(w, `{"status":"success","data":{"storageRetention":"15d or 10GiB"}}`)
		case "/api/v1/status/tsdb":
			_, _ = fmt.Fprint(w, `{"status":"success","data":{"headStats":{"minTime":1700000000000}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPrometheusReads(t *testing.T) {
	srv := fakePrometheus(t, nil)
	defer srv.Close()
	rec := &countingRecorder{}
	p, err := NewPrometheus(srv.URL, Auth{}, NewBudget(time.Second, 2, 600, rec))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	groups, err := p.AlertingRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Rules) != 1 || groups[0].Rules[0].Name != "PaymentsHighLatency" ||
		groups[0].Rules[0].Duration != 300 || !strings.Contains(groups[0].File, "team-payments-payments-api") {
		t.Errorf("rules: %+v", groups)
	}

	samples, err := p.Query(ctx, "up", time.Time{})
	if err != nil || len(samples) != 1 || samples[0].Value != 3 || samples[0].Metric["alertname"] != "A" {
		t.Errorf("vector: %+v %v", samples, err)
	}
	samples, err = p.Query(ctx, "scalar(1)", time.Now())
	if err != nil || len(samples) != 1 || samples[0].Value != 1 || samples[0].Time.UnixMilli() != 1700000000500 {
		t.Errorf("scalar: %+v %v", samples, err)
	}
	_, err = p.Query(ctx, "bad(", time.Time{})
	var qe *QueryError
	if !errors.As(err, &qe) || qe.Type != "bad_data" || IsUnreachable(err) {
		t.Errorf("bad query: %v", err)
	}

	series, err := p.QueryRange(ctx, "ALERTS", time.Unix(1700000000, 0), time.Unix(1700000060, 0), time.Minute)
	if err != nil || len(series) != 1 || len(series[0].Points) != 2 {
		t.Errorf("range: %+v %v", series, err)
	}

	ret, ok, err := p.Retention(ctx)
	if err != nil || !ok || ret != 15*24*time.Hour {
		t.Errorf("retention: %v %v %v", ret, ok, err)
	}
	oldest, err := p.OldestSample(ctx)
	if err != nil || oldest.Unix() != 1700000000 {
		t.Errorf("oldest: %v %v", oldest, err)
	}
	if rec.queries["prometheus/ok"] != 6 || rec.queries["prometheus/error"] != 1 {
		t.Errorf("recorded: %v", rec.queries)
	}
}

func TestParseRetention(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"15d": 15 * 24 * time.Hour, "1y": 365 * 24 * time.Hour, "10GiB or 2w": 14 * 24 * time.Hour, "6h": 6 * time.Hour,
	} {
		got, ok, err := parseRetention(in)
		if err != nil || !ok || got != want {
			t.Errorf("%q: %v %v %v", in, got, ok, err)
		}
	}
	for _, in := range []string{"", "10GiB", "0s"} {
		if _, ok, _ := parseRetention(in); ok {
			t.Errorf("%q: want not ok", in)
		}
	}
}

func TestPrometheusDown(t *testing.T) {
	srv := fakePrometheus(t, nil)
	url := srv.URL
	srv.Close()
	rec := &countingRecorder{}
	p, _ := NewPrometheus(url, Auth{}, NewBudget(time.Second, 1, 600, rec))
	_, err := p.AlertingRules(context.Background())
	if !IsUnreachable(err) {
		t.Fatalf("want unreachable, got %v", err)
	}
	if rec.queries["prometheus/unreachable"] != 1 {
		t.Errorf("recorded: %v", rec.queries)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	p, _ = NewPrometheus(failing.URL, Auth{}, nil)
	if _, err := p.Query(context.Background(), "up", time.Time{}); !IsUnreachable(err) {
		t.Errorf("503: want unreachable, got %v", err)
	}
}

func TestTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	p, _ := NewPrometheus(srv.URL, Auth{}, NewBudget(100*time.Millisecond, 1, 600, nil))
	start := time.Now()
	_, err := p.Query(context.Background(), "up", time.Time{})
	if !IsUnreachable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want a deadline error, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("timeout not applied: %v", time.Since(start))
	}
}

func TestBudgetExhaustion(t *testing.T) {
	var served atomic.Int32
	srv := fakePrometheus(t, func(*http.Request) { served.Add(1) })
	defer srv.Close()
	rec := &countingRecorder{}
	// Three queries a minute: the bucket starts full, the fourth would wait 20s.
	p, _ := NewPrometheus(srv.URL, Auth{}, NewBudget(time.Second, 1, 3, rec))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := range 3 {
		if _, err := p.Query(ctx, "up", time.Time{}); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	start := time.Now()
	_, err := p.Query(ctx, "up", time.Time{})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("want ErrBudgetExhausted, got %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("exhaustion should be reported without waiting, took %v", time.Since(start))
	}
	if served.Load() != 3 || rec.exhausted != 1 || rec.queries["prometheus/budget_exhausted"] != 1 {
		t.Errorf("served %d, exhausted %d, %v", served.Load(), rec.exhausted, rec.queries)
	}
}

func TestBudgetConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer srv.Close()
	p, _ := NewPrometheus(srv.URL, Auth{}, NewBudget(time.Second, 2, 6000, nil))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _, _ = p.Query(context.Background(), "up", time.Time{}) })
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Errorf("%d queries in flight, budget allows 2", peak.Load())
	}
}

func TestBearerAndBasicAuth(t *testing.T) {
	var gotAuth string
	srv := fakePrometheus(t, func(r *http.Request) { gotAuth = r.Header.Get("Authorization") })
	defer srv.Close()

	p, _ := NewPrometheus(srv.URL, Auth{BearerToken: "s3cret"}, nil)
	if _, err := p.Query(context.Background(), "up", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("bearer: %q", gotAuth)
	}
	p, _ = NewPrometheus(srv.URL, Auth{Username: "u", Password: "p"}, nil)
	if _, err := p.Query(context.Background(), "up", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Basic dTpw" {
		t.Errorf("basic: %q", gotAuth)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer denied.Close()
	p, _ = NewPrometheus(denied.URL, Auth{}, nil)
	if _, err := p.Query(context.Background(), "up", time.Time{}); !IsUnreachable(err) ||
		!strings.Contains(err.Error(), "authSecretRef") {
		t.Errorf("401: %v", err)
	}
}

func TestAuthFromSecret(t *testing.T) {
	s := func(data map[string]string) *corev1.Secret {
		out := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "reader"}, Data: map[string][]byte{}}
		for k, v := range data {
			out.Data[k] = []byte(v)
		}
		return out
	}
	a, err := AuthFromSecret(s(map[string]string{"token": "abc\n"}))
	if err != nil || a.BearerToken != "abc" {
		t.Errorf("token: %+v %v", a, err)
	}
	if _, err := AuthFromSecret(s(map[string]string{"token": "a", "username": "u"})); err == nil {
		t.Error("token with basic auth: want error")
	}
	if _, err := AuthFromSecret(s(map[string]string{"tls.crt": "x"})); err == nil {
		t.Error("certificate without key: want error")
	}
	if a, err := AuthFromSecret(nil); err != nil || !reflect.DeepEqual(a, Auth{}) {
		t.Error("nil secret")
	}
}

func selfSigned(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestTLSWithCAAndClientCertificate(t *testing.T) {
	var clientCN string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			clientCN = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	cert, key := selfSigned(t, "alert-operator")

	// Without the CA the server certificate is not trusted.
	p, _ := NewPrometheus(srv.URL, Auth{Cert: cert, Key: key}, nil)
	if _, err := p.Query(context.Background(), "up", time.Time{}); err == nil {
		t.Error("untrusted server certificate accepted")
	}
	p, err := NewPrometheus(srv.URL, Auth{CA: ca, Cert: cert, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Query(context.Background(), "up", time.Time{}); err != nil {
		t.Fatalf("with CA and client certificate: %v", err)
	}
	if clientCN != "alert-operator" {
		t.Errorf("client certificate not presented: %q", clientCN)
	}
	if _, err := NewPrometheus(srv.URL, Auth{CA: []byte("not pem")}, nil); err == nil {
		t.Error("bad CA accepted")
	}
}

func TestBadURL(t *testing.T) {
	for _, u := range []string{"", "prometheus:9090", "ftp://x", "http://"} {
		if _, err := NewPrometheus(u, Auth{}, nil); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}

func TestOnlyGETLeavesTheProcess(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/silences":
			_, _ = fmt.Fprint(w, `[]`)
		case "/api/v2/status":
			_, _ = fmt.Fprint(w, `{"cluster":{"status":"ready"}}`)
		default:
			_, _ = fmt.Fprint(w, `{"status":"success","data":{"groups":[],"resultType":"vector","result":[]}}`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	p, _ := NewPrometheus(srv.URL, Auth{}, nil)
	a, _ := NewAlertmanager(srv.URL, Auth{}, nil)
	_, _ = p.AlertingRules(ctx)
	_, _ = p.Query(ctx, "up", time.Time{})
	_, _ = a.Silences(ctx)
	_, _ = a.Status(ctx)
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("a %s request reached the server", m)
		}
	}

	// The transport itself refuses anything else, whoever builds the request.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v2/silences", strings.NewReader("{}"))
	if _, err := a.hc.Do(req); !errors.Is(err, ErrNotReadOnly) {
		t.Errorf("POST through the client: %v", err)
	}
}

// The Alertmanager client exposes reads only. Adding a method here is a design change
// (docs/design.md §6): v0.x must not be able to create or expire a silence.
func TestAlertmanagerClientHasOnlyReadMethods(t *testing.T) {
	var got []string
	typ := reflect.TypeFor[*Alertmanager]()
	for m := range typ.Methods() {
		got = append(got, m.Name)
	}
	slices.Sort(got)
	if want := []string{"Silences", "Status"}; !slices.Equal(got, want) {
		t.Errorf("Alertmanager methods = %v, want %v", got, want)
	}
	typ = reflect.TypeFor[*Prometheus]()
	got = got[:0]
	for m := range typ.Methods() {
		got = append(got, m.Name)
	}
	slices.Sort(got)
	if want := []string{"AlertingRules", "OldestSample", "Query", "QueryRange", "Retention"}; !slices.Equal(got, want) {
		t.Errorf("Prometheus methods = %v, want %v", got, want)
	}
}

func TestSilencesAndMatching(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[{"id":"1","matchers":[{"name":"alertname","value":"Payments.*","isRegex":true},
			{"name":"env","value":"dev","isRegex":false,"isEqual":false}],
			"startsAt":"2026-11-01T00:00:00Z","endsAt":"2026-11-02T00:00:00Z","createdBy":"bob","status":{"state":"expired"}}]`)
	}))
	defer srv.Close()
	a, _ := NewAlertmanager(srv.URL, Auth{}, nil)
	sil, err := a.Silences(context.Background())
	if err != nil || len(sil) != 1 || sil[0].Status.State != "expired" || sil[0].EndsAt.Day() != 2 {
		t.Fatalf("%+v %v", sil, err)
	}
	s := sil[0]
	if !s.Matches(map[string]string{"alertname": "PaymentsHighLatency", "env": "prod"}) {
		t.Error("should match")
	}
	if s.Matches(map[string]string{"alertname": "PaymentsHighLatency", "env": "dev"}) {
		t.Error("env!=dev should exclude dev")
	}
	if s.Matches(map[string]string{"alertname": "XPaymentsHighLatency"}) {
		t.Error("regex must be anchored")
	}
	if (&Silence{}).Matches(map[string]string{"a": "b"}) {
		t.Error("a silence without matchers matches nothing")
	}
}

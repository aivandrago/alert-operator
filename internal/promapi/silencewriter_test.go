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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAM keeps silences in memory and records every request.
type fakeAM struct {
	mu       sync.Mutex
	silences map[string]*Silence
	requests []string
	next     int
}

func newFakeAM() (*fakeAM, *httptest.Server) {
	f := &fakeAM{silences: map[string]*Silence{}}
	return f, httptest.NewServer(http.HandlerFunc(f.serve))
}

func (f *fakeAM) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/silences":
		var p PostableSilence
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := p.ID
		if id == "" || f.silences[id] == nil {
			f.next++
			id = fmt.Sprintf("s%d", f.next)
		}
		s := &Silence{ID: id, Matchers: p.Matchers, StartsAt: p.StartsAt, EndsAt: p.EndsAt, CreatedBy: p.CreatedBy, Comment: p.Comment}
		s.Status.State = "active"
		f.silences[id] = s
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
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	default:
		http.NotFound(w, r)
	}
}

func TestSilenceWriterOwnSilences(t *testing.T) {
	f, srv := newFakeAM()
	defer srv.Close()
	w, err := NewSilenceWriter(srv.URL, Auth{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	marker := Marker("team-a", "flap", "u-1")
	s := PostableSilence{Matchers: []Matcher{{Name: "alertname", Value: "DemoFlapping"}},
		StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), CreatedBy: CreatedBy, Comment: marker + " owner: a"}

	id, err := w.CreateOrUpdateSilence(ctx, s, marker)
	if err != nil || id != "s1" {
		t.Fatalf("create: %q %v", id, err)
	}
	s.ID = id
	s.EndsAt = s.EndsAt.Add(time.Hour)
	if id2, err := w.CreateOrUpdateSilence(ctx, s, marker); err != nil || id2 != id {
		t.Errorf("update: %q %v", id2, err)
	}
	if _, err := w.ExpireSilence(ctx, id, marker); err != nil || f.silences[id].Status.State != "expired" {
		t.Errorf("expire: %v %+v", err, f.silences[id])
	}
	// Expiring an expired silence sends nothing more.
	before := len(f.requests)
	if _, err := w.ExpireSilence(ctx, id, marker); err != nil || len(f.requests) != before+1 {
		t.Errorf("expire again: %v, %d requests", err, len(f.requests)-before)
	}

	// The marker must be on what is written.
	bad := s
	bad.ID, bad.Comment = "", "no marker"
	if _, err := w.CreateOrUpdateSilence(ctx, bad, marker); err == nil {
		t.Error("a silence without the marker was written")
	}
}

func TestSilenceWriterLeavesOtherSilencesAlone(t *testing.T) {
	f, srv := newFakeAM()
	defer srv.Close()
	human := &Silence{ID: "human", CreatedBy: "alice", Comment: "maintenance",
		Matchers: []Matcher{{Name: "alertname", Value: "DemoFlapping"}}, EndsAt: time.Now().Add(time.Hour)}
	human.Status.State = "active"
	// Another AlertSilence's silence: ours by author, not by marker.
	other := &Silence{ID: "other", CreatedBy: CreatedBy, Comment: Marker("team-b", "x", "u-2") + " owner: b"}
	other.Status.State = "active"
	f.silences["human"], f.silences["other"] = human, other

	w, _ := NewSilenceWriter(srv.URL, Auth{}, nil)
	ctx := context.Background()
	marker := Marker("team-a", "flap", "u-1")
	for _, id := range []string{"human", "other"} {
		if _, err := w.ExpireSilence(ctx, id, marker); !errors.Is(err, ErrNotOurs) {
			t.Errorf("expire %s: %v", id, err)
		}
		s := PostableSilence{ID: id, Matchers: human.Matchers, EndsAt: time.Now().Add(time.Hour), CreatedBy: CreatedBy,
			Comment: marker + " owner: a"}
		if _, err := w.CreateOrUpdateSilence(ctx, s, marker); !errors.Is(err, ErrNotOurs) {
			t.Errorf("update %s: %v", id, err)
		}
	}
	for _, r := range f.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("a write reached Alertmanager for a silence that is not ours: %s", r)
		}
	}
	if human.Status.State != "active" || other.Status.State != "active" {
		t.Error("a silence that is not ours changed")
	}
}

func TestSilenceWriterTransport(t *testing.T) {
	_, srv := newFakeAM()
	defer srv.Close()
	w, _ := NewSilenceWriter(srv.URL, Auth{BearerToken: "t"}, nil)
	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/api/v2/silences"}, {http.MethodDelete, "/api/v2/silences"},
		{http.MethodPost, "/api/v2/alerts"}, {http.MethodDelete, "/api/v2/silence/../status"},
		{http.MethodPost, "/-/reload"},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, nil)
		if _, err := w.hc.Do(req); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s %s: %v", c.method, c.path, err)
		}
	}
}

// The writer can do exactly two things. Adding a method is a design change (docs/design.md §11.2).
func TestSilenceWriterMethods(t *testing.T) {
	var got []string
	for m := range reflect.TypeFor[*SilenceWriter]().Methods() {
		got = append(got, m.Name)
	}
	slices.Sort(got)
	if want := []string{"CreateOrUpdateSilence", "ExpireSilence"}; !slices.Equal(got, want) {
		t.Errorf("SilenceWriter methods = %v, want %v", got, want)
	}
}

func TestAlertsAndSilenceReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/alerts":
			_, _ = fmt.Fprint(w, `[{"labels":{"alertname":"A","env":"prod"},"status":{"state":"active","silencedBy":[]}}]`)
		case "/api/v2/silence/x":
			_, _ = fmt.Fprint(w, `{"id":"x","createdBy":"alice","status":{"state":"active"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a, _ := NewAlertmanager(srv.URL, Auth{}, nil)
	alerts, err := a.Alerts(context.Background())
	if err != nil || len(alerts) != 1 || !Matching([]Matcher{{Name: "alertname", Value: "A"}}, alerts[0].Labels) {
		t.Errorf("alerts: %+v %v", alerts, err)
	}
	if s, err := a.Silence(context.Background(), "x"); err != nil || s.CreatedBy != "alice" {
		t.Errorf("silence: %+v %v", s, err)
	}
	if _, err := a.Silence(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing silence: %v", err)
	}
}

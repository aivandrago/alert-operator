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
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"

	"hypersurgery.dev/alert-operator/internal/promapi"
)

// fakeProm is a Prometheus and Alertmanager API with state the test sets: loaded rule groups,
// activations per alert name, and whether it is down. Selectors containing "missing_" have no
// series.
type fakeProm struct {
	mu          sync.Mutex
	groups      []promapi.RuleGroup
	activations map[string]int // alertname -> value of changes()
	down        bool
	methods     map[string]int
	srv         *httptest.Server
}

func newFakeProm() *fakeProm {
	f := &fakeProm{activations: map[string]int{}, methods: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *fakeProm) set(fn func(f *fakeProm)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

var alertnameRe = regexp.MustCompile(`alertname=~"([^"]*)"`)

// alertLabels are the labels of an alert of the loaded rule with that name.
func (f *fakeProm) alertLabels(name string) map[string]string {
	out := map[string]string{"alertname": name}
	for _, g := range f.groups {
		for _, r := range g.Rules {
			if r.Name == name {
				maps.Copy(out, r.Labels)
			}
		}
	}
	return out
}

func (f *fakeProm) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods[r.Method]++
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ok := func(data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
	}
	switch r.URL.Path {
	case "/api/v1/rules":
		ok(map[string]any{"groups": f.groups})
	case "/api/v1/status/runtimeinfo":
		ok(map[string]any{"storageRetention": "15d"})
	case "/api/v1/query":
		q := r.URL.Query().Get("query")
		var result []map[string]any
		switch {
		case strings.HasPrefix(q, "count(last_over_time("):
			if !strings.Contains(q, "missing_") {
				result = append(result, map[string]any{"metric": map[string]string{}, "value": []any{1, "1"}})
			}
		case strings.HasPrefix(q, "changes(ALERTS_FOR_STATE"):
			m := alertnameRe.FindStringSubmatch(q)
			if len(m) == 2 {
				for name := range strings.SplitSeq(strings.ReplaceAll(m[1], `\\`, `\`), "|") {
					if v, ok := f.activations[name]; ok {
						result = append(result, map[string]any{"metric": f.alertLabels(name), "value": []any{1, fmt.Sprint(v)}})
					}
				}
			}
		}
		ok(map[string]any{"resultType": "vector", "result": result})
	case "/api/v1/query_range":
		ok(map[string]any{"resultType": "matrix", "result": []any{}})
	case "/api/v2/silences":
		_, _ = fmt.Fprint(w, `[]`)
	default:
		http.NotFound(w, r)
	}
}

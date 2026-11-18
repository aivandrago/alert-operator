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

// Package cli builds hsalert and runs it the way a CI pipeline would.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var bin string

// TestMain builds hsalert once, with a version stamped, into a directory removed at the end.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hsalert-test")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "hsalert")
	cmd := exec.Command("go", "build", "-ldflags", "-X hypersurgery.dev/alert-operator/internal/version.Version=9.9.9-test",
		"-o", bin, "../../cmd/hsalert")
	if b, err := cmd.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("go build: %v\n%s", err, b))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func hsalert(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var o, e strings.Builder
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String(), code
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		out  []string
	}{
		{"bad file", []string{"lint", "testdata/bad.yaml"}, 1,
			[]string{"bad/Broken: LoadError", "bad/Nobody: Unowned", "bad/Nobody: NoRunbook", "bad/Nobody: NoSeverity", "4 blocking"}},
		{"good plain rule file", []string{"lint", "testdata/good.rules.yml"}, 0, []string{"1 objects, 0 problems"}},
		{"not loaded with a Prometheus object", []string{"lint", "--prometheus", "testdata/prometheus.yaml", "testdata/unselected.yaml"}, 1,
			[]string{"NotLoaded: the labels of PrometheusRule team-a/unselected do not match ruleSelector"}},
		{"runbook not required", []string{"lint", "--require-runbook=false", "--team-from", "rule:owner", "testdata/good.rules.yml"}, 1,
			[]string{"Unowned"}},
		{"no files", []string{"lint"}, 2, nil},
		{"missing file", []string{"lint", "testdata/nope.yaml"}, 2, nil},
		{"bad flag", []string{"lint", "--format", "xml", "testdata/good.rules.yml"}, 2, nil},
		{"unknown command", []string{"frobnicate"}, 2, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := hsalert(t, tc.args...)
			if code != tc.code {
				t.Errorf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, out, errOut)
			}
			for _, w := range tc.out {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestFormats(t *testing.T) {
	out, _, code := hsalert(t, "lint", "--format", "json", "testdata/bad.yaml", "testdata/good.rules.yml")
	var reports []struct {
		File     string `json:"file"`
		Name     string `json:"name"`
		Problems []struct {
			Check    string `json:"check"`
			Blocking bool   `json:"blocking"`
		} `json:"problems"`
	}
	if err := json.Unmarshal([]byte(out), &reports); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if code != 1 || len(reports) != 2 || len(reports[0].Problems) != 4 || reports[1].Name != "good.rules" || len(reports[1].Problems) != 0 {
		t.Errorf("json reports: %+v", reports)
	}
	out, _, _ = hsalert(t, "lint", "--format", "github", "testdata/bad.yaml")
	if !strings.Contains(out, "::error file=testdata/bad.yaml,title=LoadError::") {
		t.Errorf("github annotations:\n%s", out)
	}
	out, _, _ = hsalert(t, "version")
	if strings.TrimSpace(out) != "hsalert 9.9.9-test" {
		t.Errorf("version: %q", out)
	}
}

// With a Prometheus, a selector without series is a warning: exit 0, 1 with --strict.
func TestDeadQueryWarnsOnly(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := `[]`
		if !strings.Contains(r.URL.Query().Get("query"), "renamed") {
			result = `[{"metric":{},"value":[1,"1"]}]`
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, result)
	}))
	defer prom.Close()
	file := filepath.Join(t.TempDir(), "dead.yml")
	writeFile(t, file, `groups:
  - name: g
    rules:
      - alert: Renamed
        expr: renamed_metric > 0
        labels: { severity: warning, team: a }
        annotations: { runbook_url: "https://r" }
`)
	out, _, code := hsalert(t, "lint", "--prometheus-url", prom.URL, file)
	if code != 0 || !strings.Contains(out, "warning: g/Renamed: DeadQuery") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if _, _, code = hsalert(t, "lint", "--strict", "--prometheus-url", prom.URL, file); code != 1 {
		t.Errorf("--strict: exit %d", code)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

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

// Command hsalert checks Prometheus alert rules the way alert-operator's admission webhook does,
// for CI pipelines (docs/cli.md).
//
//	hsalert lint [flags] FILE...
//	hsalert version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	alertsv1alpha1 "hypersurgery.dev/alert-operator/api/v1alpha1"
	"hypersurgery.dev/alert-operator/internal/lint"
	"hypersurgery.dev/alert-operator/internal/promapi"
	"hypersurgery.dev/alert-operator/internal/version"
)

// Exit codes.
const (
	exitClean    = 0
	exitProblems = 1
	exitUsage    = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `hsalert checks Prometheus alert rules the way alert-operator's admission webhook does.

Usage:
  hsalert lint [flags] FILE...   check PrometheusRule manifests and Prometheus rule files
  hsalert version                print the version

Run "hsalert lint -h" for the flags. Exit codes: 0 no blocking problem, 1 at least one
blocking problem (any problem with --strict), 2 usage or read error.
`)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "lint":
		return runLint(args[1:], stdout, stderr)
	case "version", "--version", "-version":
		_, _ = fmt.Fprintln(stdout, "hsalert", version.Version)
		return exitClean
	case "help", "-h", "--help":
		usage(stdout)
		return exitClean
	default:
		_, _ = fmt.Fprintf(stderr, "hsalert: unknown command %q\n\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

type lintFlags struct {
	teamFrom        string
	requireRunbook  bool
	severities      string
	prometheus      string
	namespace       string
	namespaceLabels string
	prometheusURL   string
	tokenFile       string
	timeout         time.Duration
	format          string
	strict          bool
}

func runLint(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hsalert lint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f lintFlags
	fs.StringVar(&f.teamFrom, "team-from", "rule:team,object:hs/team,namespace:hs/team",
		"where the owning team comes from, first match wins: rule:<label>, object:<label>, namespace:<label>")
	fs.BoolVar(&f.requireRunbook, "require-runbook", true, "report alerting rules without a runbook_url annotation")
	fs.StringVar(&f.severities, "severities", strings.Join(alertsv1alpha1.DefaultSeverities, ","),
		"allowed values of the severity label")
	fs.StringVar(&f.prometheus, "prometheus", "",
		"a Prometheus object manifest (prometheus-operator); with it, objects its selectors would not load are reported")
	fs.StringVar(&f.namespace, "namespace", "default", "namespace of objects that name none, and of plain rule files")
	fs.StringVar(&f.namespaceLabels, "namespace-labels", "",
		"labels of the namespace the rules go to, k=v,k=v (for namespace:<label> owners and ruleNamespaceSelector)")
	fs.StringVar(&f.prometheusURL, "prometheus-url", "",
		"a Prometheus to ask whether the rules' selectors match any series (DeadQuery); never blocks")
	fs.StringVar(&f.tokenFile, "bearer-token-file", "", "file with a bearer token for --prometheus-url")
	fs.DurationVar(&f.timeout, "timeout", 30*time.Second, "time allowed for all queries to --prometheus-url")
	fs.StringVar(&f.format, "format", "text", "output: text, json or github (workflow annotations)")
	fs.BoolVar(&f.strict, "strict", false, "exit 1 on any problem, warnings included")
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, "Usage: hsalert lint [flags] FILE...\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitClean
		}
		return exitUsage
	}
	if fs.NArg() == 0 {
		_, _ = fmt.Fprintln(stderr, "hsalert lint: no files given")
		fs.Usage()
		return exitUsage
	}
	cfg, err := f.config()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hsalert lint:", err)
		return exitUsage
	}

	var reports []report
	for _, path := range fs.Args() {
		objs, err := readRules(path, f.namespace)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "hsalert lint:", err)
			return exitUsage
		}
		if len(objs) == 0 {
			_, _ = fmt.Fprintf(stderr, "hsalert lint: %s: no PrometheusRule and no rule groups found\n", path)
		}
		ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
		for _, o := range objs {
			in := cfg.input
			in.Rule = o.rule
			in.Now = time.Now()
			res := lint.Check(ctx, in)
			reports = append(reports, report{File: path, Namespace: o.rule.Namespace, Name: o.rule.Name,
				Problems: res.Problems, Skipped: res.Skipped})
		}
		cancel()
	}
	if err := write(stdout, stderr, f.format, reports); err != nil {
		_, _ = fmt.Fprintln(stderr, "hsalert lint:", err)
		return exitUsage
	}
	for _, r := range reports {
		for _, p := range r.Problems {
			if p.Blocking || f.strict {
				return exitProblems
			}
		}
	}
	return exitClean
}

type config struct{ input lint.Input }

func (f lintFlags) config() (config, error) {
	var c config
	spec := alertsv1alpha1.AlertScopeSpec{Ownership: alertsv1alpha1.Ownership{RequireRunbook: &f.requireRunbook}}
	for part := range strings.SplitSeq(f.teamFrom, ",") {
		kind, label, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || label == "" {
			return c, fmt.Errorf("--team-from: %q is not rule:<label>, object:<label> or namespace:<label>", part)
		}
		switch kind {
		case "rule":
			spec.Ownership.TeamFrom = append(spec.Ownership.TeamFrom, alertsv1alpha1.TeamSource{RuleLabel: label})
		case "object":
			spec.Ownership.TeamFrom = append(spec.Ownership.TeamFrom, alertsv1alpha1.TeamSource{ObjectLabel: label})
		case "namespace":
			spec.Ownership.TeamFrom = append(spec.Ownership.TeamFrom, alertsv1alpha1.TeamSource{NamespaceLabel: label})
		default:
			return c, fmt.Errorf("--team-from: unknown source %q", kind)
		}
	}
	for s := range strings.SplitSeq(f.severities, ",") {
		if s = strings.TrimSpace(s); s != "" {
			spec.Ownership.RequireSeverity = append(spec.Ownership.RequireSeverity, s)
		}
	}
	c.input.Settings = spec.Settings()
	labels, err := parseLabels(f.namespaceLabels)
	if err != nil {
		return c, err
	}
	c.input.NamespaceLabels = labels
	if f.prometheus != "" {
		p, err := readPrometheus(f.prometheus)
		if err != nil {
			return c, err
		}
		c.input.Prometheus = p
	}
	if f.prometheusURL != "" {
		auth := promapi.Auth{}
		if f.tokenFile != "" {
			b, err := os.ReadFile(f.tokenFile)
			if err != nil {
				return c, fmt.Errorf("--bearer-token-file: %w", err)
			}
			auth.BearerToken = strings.TrimSpace(string(b))
		}
		p, err := promapi.NewPrometheus(f.prometheusURL, auth, promapi.NewBudget(10*time.Second, 4, 6000, nil))
		if err != nil {
			return c, fmt.Errorf("--prometheus-url: %w", err)
		}
		c.input.Querier = p
	}
	switch f.format {
	case "text", "json", "github":
	default:
		return c, fmt.Errorf("--format: %q is not text, json or github", f.format)
	}
	return c, nil
}

func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--namespace-labels: %q is not k=v", part)
		}
		out[k] = v
	}
	return out, nil
}

type report struct {
	File      string         `json:"file"`
	Namespace string         `json:"namespace"`
	Name      string         `json:"name"`
	Problems  []lint.Problem `json:"problems"`
	Skipped   []string       `json:"skipped,omitempty"`
}

func write(stdout, stderr io.Writer, format string, reports []report) error {
	switch format {
	case "json":
		for i := range reports {
			if reports[i].Problems == nil {
				reports[i].Problems = []lint.Problem{}
			}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reports)
	case "github":
		for _, r := range reports {
			for _, p := range r.Problems {
				level := "warning"
				if p.Blocking {
					level = "error"
				}
				_, _ = fmt.Fprintf(stdout, "::%s file=%s,title=%s::%s/%s: %s\n", level, r.File, p.Check,
					r.Namespace, r.Name, escapeAnnotation(p.String()))
			}
		}
	default:
		problems, blocking := 0, 0
		for _, r := range reports {
			for _, p := range r.Problems {
				problems++
				tag := "warning"
				if p.Blocking {
					tag = "error"
					blocking++
				}
				_, _ = fmt.Fprintf(stdout, "%s: %s/%s: %s: %s\n", r.File, r.Namespace, r.Name, tag, p)
			}
		}
		_, _ = fmt.Fprintf(stdout, "%d objects, %d problems, %d blocking\n", len(reports), problems, blocking)
	}
	seen := map[string]bool{}
	for _, r := range reports {
		for _, s := range r.Skipped {
			if !seen[s] {
				seen[s] = true
				_, _ = fmt.Fprintln(stderr, "note: skipped", s)
			}
		}
	}
	return nil
}

// escapeAnnotation escapes a message for a GitHub workflow command.
func escapeAnnotation(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

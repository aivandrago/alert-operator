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

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

type object struct {
	rule *monitoringv1.PrometheusRule
}

// readRules reads every PrometheusRule of a file of YAML documents ("-" is stdin). A document
// with a top-level "groups" and no kind is a plain Prometheus rule file and becomes one object
// named after the file. Other kinds are skipped.
func readRules(path, namespace string) ([]object, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	reader := utilyaml.NewYAMLReader(bufioReader(data))
	var out []object
	for i := 0; ; i++ {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var head struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Groups     []any  `json:"groups"`
		}
		if err := yaml.Unmarshal(doc, &head); err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", path, i+1, err)
		}
		switch {
		case head.Kind == "PrometheusRule" && strings.HasPrefix(head.APIVersion, "monitoring.coreos.com/"):
			pr := &monitoringv1.PrometheusRule{}
			if err := yaml.UnmarshalStrict(doc, pr); err != nil {
				return nil, fmt.Errorf("%s: document %d: %w", path, i+1, err)
			}
			if pr.Namespace == "" {
				pr.Namespace = namespace
			}
			out = append(out, object{rule: pr})
		case head.Kind == "" && head.Groups != nil:
			pr := &monitoringv1.PrometheusRule{}
			// A plain rule file has the PrometheusRule spec's shape (groups, rules), with
			// keep_firing_for and the rest spelled the same.
			if err := yaml.UnmarshalStrict(doc, &pr.Spec); err != nil {
				return nil, fmt.Errorf("%s: document %d: %w", path, i+1, err)
			}
			pr.Namespace = namespace
			pr.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			out = append(out, object{rule: pr})
		}
	}
	return out, nil
}

// readPrometheus reads the first Prometheus object of a manifest.
func readPrometheus(path string) (*monitoringv1.Prometheus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--prometheus: %w", err)
	}
	reader := utilyaml.NewYAMLReader(bufioReader(data))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("--prometheus: %s has no Prometheus object", path)
		}
		if err != nil {
			return nil, fmt.Errorf("--prometheus: %w", err)
		}
		var head struct {
			Kind string `json:"kind"`
		}
		if yaml.Unmarshal(doc, &head) != nil || head.Kind != "Prometheus" {
			continue
		}
		p := &monitoringv1.Prometheus{}
		if err := yaml.Unmarshal(doc, p); err != nil {
			return nil, fmt.Errorf("--prometheus: %w", err)
		}
		return p, nil
	}
}

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }

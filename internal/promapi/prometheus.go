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
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// Prometheus reads the Prometheus HTTP API (v1). It has no method that writes.
type Prometheus struct {
	endpoint
}

// NewPrometheus returns a client for the Prometheus at rawURL.
func NewPrometheus(rawURL string, auth Auth, budget *Budget) (*Prometheus, error) {
	e, err := newEndpoint("prometheus", rawURL, auth, budget)
	if err != nil {
		return nil, err
	}
	return &Prometheus{endpoint: e}, nil
}

// QueryError is a query Prometheus refused (a bad expression, a timeout on its side).
type QueryError struct {
	Type, Message string
}

func (e *QueryError) Error() string { return "prometheus " + e.Type + ": " + e.Message }

type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

func (p *Prometheus) call(ctx context.Context, path string, params url.Values, out any) error {
	var env envelope
	if err := p.get(ctx, path, params, &env); err != nil {
		return err
	}
	if env.Status != "success" {
		return &QueryError{Type: env.ErrorType, Message: env.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("prometheus: decoding %s: %w", path, err)
	}
	return nil
}

// Rule is an alerting rule as /api/v1/rules reports it.
type Rule struct {
	Name           string            `json:"name"`
	Query          string            `json:"query"`
	Duration       float64           `json:"duration"`
	KeepFiringFor  float64           `json:"keepFiringFor"`
	Labels         map[string]string `json:"labels"`
	Annotations    map[string]string `json:"annotations"`
	Health         string            `json:"health"`
	LastError      string            `json:"lastError"`
	Type           string            `json:"type"`
	State          string            `json:"state"`
	LastEvaluation time.Time         `json:"lastEvaluation"`
}

// RuleGroup is a rule group as /api/v1/rules reports it.
type RuleGroup struct {
	Name     string  `json:"name"`
	File     string  `json:"file"`
	Interval float64 `json:"interval"`
	Rules    []Rule  `json:"rules"`
}

// AlertingRules returns the alerting rules Prometheus has loaded, by group.
func (p *Prometheus) AlertingRules(ctx context.Context) ([]RuleGroup, error) {
	var data struct {
		Groups []RuleGroup `json:"groups"`
	}
	if err := p.call(ctx, "/api/v1/rules", url.Values{"type": {"alert"}}, &data); err != nil {
		return nil, err
	}
	// type=alert is honoured by every current Prometheus; filter anyway for older ones.
	for i := range data.Groups {
		rules := data.Groups[i].Rules[:0]
		for _, r := range data.Groups[i].Rules {
			if r.Type == "" || r.Type == "alerting" {
				rules = append(rules, r)
			}
		}
		data.Groups[i].Rules = rules
	}
	return data.Groups, nil
}

// Sample is one series of an instant query result.
type Sample struct {
	Metric map[string]string
	Value  float64
	Time   time.Time
}

// Series is one series of a range query result.
type Series struct {
	Metric map[string]string
	Points []Point
}

// Point is one value of a range query result.
type Point struct {
	Time  time.Time
	Value float64
}

// Query runs an instant query at ts (zero: now). Scalars come back as one sample without
// labels; strings are not supported.
func (p *Prometheus) Query(ctx context.Context, q string, ts time.Time) ([]Sample, error) {
	params := url.Values{"query": {q}}
	if !ts.IsZero() {
		params.Set("time", formatTime(ts))
	}
	var data struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := p.call(ctx, "/api/v1/query", params, &data); err != nil {
		return nil, err
	}
	switch data.ResultType {
	case "vector":
		var raw []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		if err := json.Unmarshal(data.Result, &raw); err != nil {
			return nil, err
		}
		out := make([]Sample, 0, len(raw))
		for _, r := range raw {
			t, v, err := parsePair(r.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, Sample{Metric: r.Metric, Value: v, Time: t})
		}
		return out, nil
	case "scalar":
		var raw [2]any
		if err := json.Unmarshal(data.Result, &raw); err != nil {
			return nil, err
		}
		t, v, err := parsePair(raw)
		if err != nil {
			return nil, err
		}
		return []Sample{{Metric: map[string]string{}, Value: v, Time: t}}, nil
	default:
		return nil, fmt.Errorf("prometheus: unsupported result type %q for an instant query", data.ResultType)
	}
}

// QueryRange runs a range query.
func (p *Prometheus) QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]Series, error) {
	params := url.Values{
		"query": {q},
		"start": {formatTime(start)},
		"end":   {formatTime(end)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	var data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	}
	if err := p.call(ctx, "/api/v1/query_range", params, &data); err != nil {
		return nil, err
	}
	if data.ResultType != "matrix" {
		return nil, fmt.Errorf("prometheus: unexpected result type %q for a range query", data.ResultType)
	}
	out := make([]Series, 0, len(data.Result))
	for _, r := range data.Result {
		s := Series{Metric: r.Metric, Points: make([]Point, 0, len(r.Values))}
		for _, pair := range r.Values {
			t, v, err := parsePair(pair)
			if err != nil {
				return nil, err
			}
			s.Points = append(s.Points, Point{Time: t, Value: v})
		}
		out = append(out, s)
	}
	return out, nil
}

// Retention returns how much history Prometheus keeps, from /api/v1/status/runtimeinfo
// ("15d", or "15d or 10GiB" when a size limit is set too, in which case the time part is used).
// ok is false when the endpoint does not say (a Thanos or Mimir query frontend, a size-only
// retention): the caller then uses its configured window as is.
func (p *Prometheus) Retention(ctx context.Context) (d time.Duration, ok bool, err error) {
	var data struct {
		StorageRetention string `json:"storageRetention"`
	}
	if err := p.call(ctx, "/api/v1/status/runtimeinfo", nil, &data); err != nil {
		if _, isQuery := err.(*QueryError); isQuery {
			return 0, false, nil
		}
		return 0, false, err
	}
	return parseRetention(data.StorageRetention)
}

// OldestSample returns the time of the oldest sample in the TSDB head, from
// /api/v1/status/tsdb. It covers the head block only, so it says nothing about older blocks
// on disk; callers use it for reporting, never to shorten the window.
func (p *Prometheus) OldestSample(ctx context.Context) (time.Time, error) {
	var data struct {
		HeadStats struct {
			MinTime int64 `json:"minTime"`
		} `json:"headStats"`
	}
	if err := p.call(ctx, "/api/v1/status/tsdb", nil, &data); err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(data.HeadStats.MinTime), nil
}

func parseRetention(s string) (time.Duration, bool, error) {
	for part := range strings.SplitSeq(s, " or ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if d, err := model.ParseDuration(part); err == nil && d > 0 {
			return time.Duration(d), true, nil
		}
	}
	return 0, false, nil
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', -1, 64)
}

func parsePair(p [2]any) (time.Time, float64, error) {
	ts, ok := p[0].(float64)
	if !ok {
		return time.Time{}, 0, fmt.Errorf("prometheus: bad timestamp %v", p[0])
	}
	vs, ok := p[1].(string)
	if !ok {
		return time.Time{}, 0, fmt.Errorf("prometheus: bad value %v", p[1])
	}
	v, err := strconv.ParseFloat(vs, 64)
	if err != nil {
		return time.Time{}, 0, err
	}
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), v, nil
}

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
	"net/url"
	"regexp"
	"time"
)

// Alertmanager reads the Alertmanager API v2. Its methods all GET: there is no way to create,
// expire or change a silence through it (SilenceWriter does that, only with --enable-writes).
// A test fails if a method is added.
type Alertmanager struct {
	endpoint
}

// NewAlertmanager returns a client for the Alertmanager at rawURL.
func NewAlertmanager(rawURL string, auth Auth, budget *Budget) (*Alertmanager, error) {
	e, err := newEndpoint("alertmanager", rawURL, auth, budget)
	if err != nil {
		return nil, err
	}
	return &Alertmanager{endpoint: e}, nil
}

// Matcher is one matcher of a silence.
type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	// IsEqual is false for != and !~. Absent in old Alertmanagers, which means true.
	IsEqual *bool `json:"isEqual,omitempty"`
}

// Silence is a silence as /api/v2/silences reports it, active, pending or expired (Alertmanager
// keeps expired silences for its retention, five days by default).
type Silence struct {
	ID        string    `json:"id"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
	Status    struct {
		State string `json:"state"`
	} `json:"status"`
}

// Matches reports whether the silence's matchers all match the label set, the way
// Alertmanager decides it: regular expressions are anchored, a missing label is "".
func (s *Silence) Matches(labels map[string]string) bool {
	for _, m := range s.Matchers {
		v := labels[m.Name]
		var ok bool
		if m.IsRegex {
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil {
				return false
			}
			ok = re.MatchString(v)
		} else {
			ok = v == m.Value
		}
		if m.IsEqual != nil && !*m.IsEqual {
			ok = !ok
		}
		if !ok {
			return false
		}
	}
	return len(s.Matchers) > 0
}

// Silences returns every silence Alertmanager holds.
func (a *Alertmanager) Silences(ctx context.Context) ([]Silence, error) {
	var out []Silence
	if err := a.get(ctx, "/api/v2/silences", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AlertmanagerStatus is the part of /api/v2/status the operator uses.
type AlertmanagerStatus struct {
	Cluster struct {
		Status string `json:"status"`
	} `json:"cluster"`
	VersionInfo map[string]string `json:"versionInfo"`
	Uptime      time.Time         `json:"uptime"`
}

// Status returns Alertmanager's status; the operator calls it to check the endpoint is up.
func (a *Alertmanager) Status(ctx context.Context) (AlertmanagerStatus, error) {
	var out AlertmanagerStatus
	err := a.get(ctx, "/api/v2/status", nil, &out)
	return out, err
}

// Silence returns one silence by ID; ErrNotFound when Alertmanager has none.
func (a *Alertmanager) Silence(ctx context.Context, id string) (*Silence, error) {
	s := &Silence{}
	if err := a.get(ctx, "/api/v2/silence/"+url.PathEscape(id), nil, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Alert is an alert as /api/v2/alerts reports it.
type Alert struct {
	Labels map[string]string `json:"labels"`
	Status struct {
		State      string   `json:"state"`
		SilencedBy []string `json:"silencedBy"`
	} `json:"status"`
}

// Alerts returns the alerts Alertmanager holds, active, silenced and inhibited.
func (a *Alertmanager) Alerts(ctx context.Context) ([]Alert, error) {
	var out []Alert
	if err := a.get(ctx, "/api/v2/alerts", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Matching reports whether every matcher matches the label set, the way Alertmanager decides it.
func Matching(matchers []Matcher, labels map[string]string) bool {
	return (&Silence{Matchers: matchers}).Matches(labels)
}

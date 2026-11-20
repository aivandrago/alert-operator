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
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// CreatedBy is the createdBy of every silence the operator writes.
const CreatedBy = "alert-operator"

// ErrNotOurs is returned instead of changing a silence the operator did not create.
var ErrNotOurs = errors.New("refusing to change a silence alert-operator did not create")

// ErrNotAllowed is returned for a request the writer's transport does not allow.
var ErrNotAllowed = errors.New("refusing a request outside POST /api/v2/silences and DELETE /api/v2/silence/{id}")

// SilenceWriter creates, updates and expires the operator's own silences (docs/design.md
// §11.2). It exists only when the manager runs with --enable-writes. It has exactly two
// methods; both check that the silence they would change is the operator's before changing it,
// and its transport refuses every other request. A test fails if a method is added.
type SilenceWriter struct {
	endpoint
}

var silencePath = regexp.MustCompile(`^/api/v2/silence/[0-9A-Za-z-]+$`)

// writeTransport allows reads, POST /api/v2/silences and DELETE /api/v2/silence/{id}.
type writeTransport struct {
	readOnlyTransport
	base string
}

func (t *writeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(req.URL.Path, t.base)
	switch {
	case req.Method == http.MethodGet:
	case req.Method == http.MethodPost && path == "/api/v2/silences":
	case req.Method == http.MethodDelete && silencePath.MatchString(path):
	default:
		return nil, ErrNotAllowed
	}
	clone := req.Clone(req.Context())
	t.authorize(clone)
	return t.next.RoundTrip(clone)
}

// NewSilenceWriter returns a writer for the Alertmanager at rawURL.
func NewSilenceWriter(rawURL string, auth Auth, budget *Budget) (*SilenceWriter, error) {
	e, err := newEndpoint("alertmanager", rawURL, auth, budget)
	if err != nil {
		return nil, err
	}
	ro := e.hc.Transport.(*readOnlyTransport)
	e.hc = &http.Client{
		Transport:     &writeTransport{readOnlyTransport: *ro, base: strings.TrimSuffix(e.base.Path, "/")},
		CheckRedirect: e.hc.CheckRedirect,
	}
	return &SilenceWriter{endpoint: e}, nil
}

// PostableSilence is a silence to create or, with an ID, to update.
type PostableSilence struct {
	ID        string    `json:"id,omitempty"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
}

// Marker is the start of the comment of every silence written for an AlertSilence: the object
// it belongs to, by namespace, name and UID.
func Marker(namespace, name, uid string) string {
	return fmt.Sprintf("[alert-operator %s/%s uid=%s]", namespace, name, uid)
}

func ours(s *Silence, marker string) bool {
	return s.CreatedBy == CreatedBy && strings.HasPrefix(s.Comment, marker)
}

func (w *SilenceWriter) silence(ctx context.Context, id string) (*Silence, error) {
	s := &Silence{}
	if err := w.get(ctx, "/api/v2/silence/"+url.PathEscape(id), nil, s); err != nil {
		return nil, err
	}
	return s, nil
}

// CreateOrUpdateSilence posts the silence and returns its ID. With s.ID set it first reads that
// silence and refuses with ErrNotOurs unless it is the operator's and carries marker; s itself
// must carry createdBy alert-operator and a comment starting with marker. Alertmanager updates
// a silence in place when only its end or comment change, and otherwise expires it and returns
// a new ID.
func (w *SilenceWriter) CreateOrUpdateSilence(ctx context.Context, s PostableSilence, marker string) (string, error) {
	if marker == "" || s.CreatedBy != CreatedBy || !strings.HasPrefix(s.Comment, marker) {
		return "", fmt.Errorf("a silence the operator writes must carry createdBy %q and the marker %q", CreatedBy, marker)
	}
	if s.ID != "" {
		existing, err := w.silence(ctx, s.ID)
		if err != nil {
			return "", err
		}
		if !ours(existing, marker) {
			return "", ErrNotOurs
		}
	}
	body, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	var out struct {
		SilenceID string `json:"silenceID"`
	}
	if err := w.do(ctx, http.MethodPost, "/api/v2/silences", nil, body, &out); err != nil {
		return "", err
	}
	if out.SilenceID == "" {
		return "", errors.New("alertmanager returned no silence ID")
	}
	return out.SilenceID, nil
}

// ExpireSilence expires the silence id after checking it is the operator's and carries marker.
// An already expired silence is left as it is. It returns the silence as read before.
func (w *SilenceWriter) ExpireSilence(ctx context.Context, id, marker string) (*Silence, error) {
	s, err := w.silence(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ours(s, marker) {
		return s, ErrNotOurs
	}
	if s.Status.State == "expired" {
		return s, nil
	}
	return s, w.do(ctx, http.MethodDelete, "/api/v2/silence/"+url.PathEscape(id), nil, nil, nil)
}

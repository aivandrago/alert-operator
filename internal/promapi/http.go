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

// Package promapi holds the read-only clients of the Prometheus and Alertmanager HTTP APIs and
// the query budget they share. Neither client can send anything but a GET request: there is no
// method that writes, and the transport refuses any other method as a second line of defence.
package promapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Auth is how a client authenticates to an endpoint and checks its certificate.
type Auth struct {
	// BearerToken is sent as "Authorization: Bearer ...".
	BearerToken string
	// Username and Password are sent as basic auth.
	Username, Password string
	// CA is a PEM bundle the server certificate must chain to; empty means the system roots.
	CA []byte
	// Cert and Key are a PEM client certificate and key.
	Cert, Key []byte
}

// Secret keys AuthFromSecret reads.
const (
	SecretKeyToken    = "token"
	SecretKeyUsername = "username"
	SecretKeyPassword = "password"
	SecretKeyCA       = "ca.crt"
	SecretKeyCert     = "tls.crt"
	SecretKeyKey      = "tls.key"
)

// AuthFromSecret reads an Auth from a Secret. Every key is optional; a token together with
// basic auth, or a certificate without its key, is an error.
func AuthFromSecret(s *corev1.Secret) (Auth, error) {
	if s == nil {
		return Auth{}, nil
	}
	a := Auth{
		BearerToken: strings.TrimSpace(string(s.Data[SecretKeyToken])),
		Username:    string(s.Data[SecretKeyUsername]),
		Password:    string(s.Data[SecretKeyPassword]),
		CA:          s.Data[SecretKeyCA],
		Cert:        s.Data[SecretKeyCert],
		Key:         s.Data[SecretKeyKey],
	}
	if a.BearerToken != "" && (a.Username != "" || a.Password != "") {
		return Auth{}, fmt.Errorf("secret %s sets both %q and basic auth", s.Name, SecretKeyToken)
	}
	if (len(a.Cert) == 0) != (len(a.Key) == 0) {
		return Auth{}, fmt.Errorf("secret %s needs both %q and %q for a client certificate",
			s.Name, SecretKeyCert, SecretKeyKey)
	}
	return a, nil
}

// readOnlyTransport sends GET requests only and adds the credentials.
type readOnlyTransport struct {
	next http.RoundTripper
	auth Auth
}

// ErrNotReadOnly is returned for any request that is not a GET.
var ErrNotReadOnly = errors.New("refusing a request that is not a GET: the operator only reads")

func (t *readOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, ErrNotReadOnly
	}
	req = req.Clone(req.Context())
	switch {
	case t.auth.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+t.auth.BearerToken)
	case t.auth.Username != "" || t.auth.Password != "":
		req.SetBasicAuth(t.auth.Username, t.auth.Password)
	}
	req.Header.Set("User-Agent", "alert-operator")
	return t.next.RoundTrip(req)
}

// newHTTPClient returns a client for one endpoint. Timeouts come from the budget, per request.
func newHTTPClient(auth Auth) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(auth.CA) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(auth.CA) {
			return nil, errors.New("no certificate found in the CA bundle")
		}
		tlsCfg.RootCAs = pool
	}
	if len(auth.Cert) > 0 {
		pair, err := tls.X509KeyPair(auth.Cert, auth.Key)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}
	tr.TLSClientConfig = tlsCfg
	return &http.Client{
		Transport: &readOnlyTransport{next: tr, auth: auth},
		// A redirect could carry the credentials elsewhere; Prometheus does not redirect its API.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// endpoint is the part both clients share: base URL, HTTP client, budget.
type endpoint struct {
	target string // "prometheus" or "alertmanager", for metrics
	base   *url.URL
	hc     *http.Client
	budget *Budget
}

func newEndpoint(target, rawURL string, auth Auth, budget *Budget) (endpoint, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return endpoint{}, fmt.Errorf("%s url %q is not an http(s) URL", target, rawURL)
	}
	hc, err := newHTTPClient(auth)
	if err != nil {
		return endpoint{}, fmt.Errorf("%s: %w", target, err)
	}
	if budget == nil {
		budget = NewBudget(0, 1, 6000, nil)
	}
	return endpoint{target: target, base: u, hc: hc, budget: budget}, nil
}

// UnreachableError says the endpoint could not be reached or answered with a server error.
type UnreachableError struct {
	Target string
	Err    error
}

func (e *UnreachableError) Error() string { return e.Target + " unreachable: " + e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

// IsUnreachable reports whether err means the endpoint was down (as opposed to a bad query).
func IsUnreachable(err error) bool {
	var u *UnreachableError
	return errors.As(err, &u)
}

// get sends one GET under the budget and decodes the JSON body into out.
func (e endpoint) get(ctx context.Context, path string, params url.Values, out any) error {
	release, err := e.budget.acquire(ctx)
	if err != nil {
		e.budget.recorder.Query(e.target, "budget_exhausted")
		return err
	}
	defer release()
	if e.budget.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.budget.Timeout)
		defer cancel()
	}
	u := e.base.JoinPath(path)
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		e.budget.recorder.Query(e.target, "unreachable")
		return &UnreachableError{Target: e.target, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		e.budget.recorder.Query(e.target, "unreachable")
		return &UnreachableError{Target: e.target, Err: err}
	}
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		e.budget.recorder.Query(e.target, "unreachable")
		return &UnreachableError{Target: e.target, Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		e.budget.recorder.Query(e.target, "unauthorized")
		return &UnreachableError{Target: e.target, Err: fmt.Errorf("HTTP %d: check authSecretRef", resp.StatusCode)}
	case resp.StatusCode >= 300 && resp.StatusCode != http.StatusBadRequest && resp.StatusCode != 422:
		e.budget.recorder.Query(e.target, "error")
		return fmt.Errorf("%s: HTTP %d: %s", e.target, resp.StatusCode, snippet(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		e.budget.recorder.Query(e.target, "error")
		return fmt.Errorf("%s: decoding %s: %w", e.target, path, err)
	}
	if resp.StatusCode >= 400 {
		e.budget.recorder.Query(e.target, "error")
	} else {
		e.budget.recorder.Query(e.target, "ok")
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

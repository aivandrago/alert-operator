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
	"errors"
	"time"

	"golang.org/x/time/rate"
)

// ErrBudgetExhausted is returned instead of sending a query when the scope's query budget
// cannot afford it before the sync's deadline. Callers keep the previous result and try again
// in the next sync; it is not a failure of the sync.
var ErrBudgetExhausted = errors.New("query budget exhausted")

// Recorder counts queries and budget exhaustion. The metrics package implements it.
type Recorder interface {
	Query(target, result string)
	BudgetExhausted()
}

type nopRecorder struct{}

func (nopRecorder) Query(string, string) {}
func (nopRecorder) BudgetExhausted()     {}

// Budget bounds the load one scope puts on its Prometheus and Alertmanager (spec.queryBudget):
// a timeout per query, a number of queries in flight and a rate, as a token bucket that holds
// one minute's worth of queries.
type Budget struct {
	Timeout  time.Duration
	limiter  *rate.Limiter
	sem      chan struct{}
	recorder Recorder
}

// NewBudget returns a budget. maxConcurrent and perMinute below 1 are taken as 1.
func NewBudget(timeout time.Duration, maxConcurrent, perMinute int, rec Recorder) *Budget {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if perMinute < 1 {
		perMinute = 1
	}
	if rec == nil {
		rec = nopRecorder{}
	}
	return &Budget{
		Timeout:  timeout,
		limiter:  rate.NewLimiter(rate.Limit(float64(perMinute)/60), perMinute),
		sem:      make(chan struct{}, maxConcurrent),
		recorder: rec,
	}
}

// Update changes the limits in place, keeping the tokens already spent.
func (b *Budget) Update(timeout time.Duration, maxConcurrent, perMinute int) {
	b.Timeout = timeout
	if perMinute < 1 {
		perMinute = 1
	}
	b.limiter.SetLimit(rate.Limit(float64(perMinute) / 60))
	b.limiter.SetBurst(perMinute)
	if maxConcurrent >= 1 && maxConcurrent != cap(b.sem) {
		b.sem = make(chan struct{}, maxConcurrent)
	}
}

// acquire waits for a token and a concurrency slot. It gives up with ErrBudgetExhausted
// when the token would only be available after ctx's deadline, without waiting for it.
func (b *Budget) acquire(ctx context.Context) (release func(), err error) {
	if err := b.limiter.Wait(ctx); err != nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ctx.Err()
		}
		b.recorder.BudgetExhausted()
		return nil, ErrBudgetExhausted
	}
	sem := b.sem
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		b.recorder.BudgetExhausted()
		return nil, ErrBudgetExhausted
	}
}

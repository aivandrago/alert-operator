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

package v1alpha1

import (
	"testing"
	"time"
)

func TestDurationParse(t *testing.T) {
	cases := map[Duration]time.Duration{
		"90s": 90 * time.Second, "15m": 15 * time.Minute, "2h": 2 * time.Hour, "30d": 30 * 24 * time.Hour,
	}
	for in, want := range cases {
		got, err := in.Parse()
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v; want %v", in, got, err, want)
		}
		if back := FormatDuration(got); back != in {
			t.Errorf("FormatDuration(%v) = %s, want %s", got, back, in)
		}
	}
	for _, bad := range []Duration{"", "m", "5", "5w", "-1h", "1.5h"} {
		if _, err := bad.Parse(); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if got := Duration("").OrDefault("15m"); got != 15*time.Minute {
		t.Errorf("OrDefault: %v", got)
	}
}

func TestSettingsDefaults(t *testing.T) {
	s := (&AlertScopeSpec{}).Settings()
	if s.Window != 30*24*time.Hour || s.NoisyFiringsAbove != 50 || s.MaxConcurrent != 2 ||
		!s.RequireRunbook || len(s.TeamFrom) != 3 || len(s.RequireSeverity) != 3 || s.SilencedFractionAbove != 0.8 {
		t.Errorf("defaults: %+v", s)
	}
	no := false
	s = (&AlertScopeSpec{Ownership: Ownership{RequireRunbook: &no}, Quality: Quality{Window: "7d"}}).Settings()
	if s.RequireRunbook || s.Window != 7*24*time.Hour {
		t.Errorf("explicit values: %+v", s)
	}
}

func TestSeverityOf(t *testing.T) {
	if SeverityOf(FindingNotLoaded) != SeverityCritical || SeverityOf(FindingNoisy) != SeverityWarning ||
		SeverityOf(FindingNoRunbook) != SeverityInfo {
		t.Error("severity table")
	}
}

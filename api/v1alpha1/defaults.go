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
	"fmt"
	"strconv"
	"time"
)

// Defaults of the AlertScope spec. The CRD sets the same values on create; the controller
// applies them again so an object written before a default existed is judged the same way.
const (
	DefaultWindow                Duration = "30d"
	DefaultResyncInterval        Duration = "15m"
	DefaultNoisyFiringsAbove     int32    = 50
	DefaultFlappingPerDayAbove   int32    = 10
	DefaultFlappingMedianBelow   Duration = "10m"
	DefaultNeverFiredAfter       Duration = "30d"
	DefaultSilencedFractionAbove          = 0.8
	DefaultQueryTimeout          Duration = "20s"
	DefaultMaxConcurrent         int32    = 2
	DefaultMaxQueriesPerMinute   int32    = 120
	DefaultTeamRuleLabel                  = "team"
	DefaultTeamObjectOrNamespace          = "hs/team"
	DefaultSilenceMaxDuration    Duration = "7d"
)

// DefaultSeverities are the severities a rule may carry when requireSeverity is empty.
var DefaultSeverities = []string{"critical", "warning", "info"}

// Parse returns the duration. It accepts the units s, m, h and d.
func (d Duration) Parse() (time.Duration, error) {
	s := string(d)
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	var unit time.Duration
	switch s[len(s)-1] {
	case 's':
		unit = time.Second
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	default:
		return 0, fmt.Errorf("invalid duration %q: unit must be s, m, h or d", s)
	}
	return time.Duration(n) * unit, nil
}

// OrDefault returns d, or def when d is empty or invalid.
func (d Duration) OrDefault(def Duration) time.Duration {
	if v, err := d.Parse(); err == nil && d != "" {
		return v
	}
	v, _ := def.Parse()
	return v
}

// FormatDuration writes a duration in the largest unit that represents it exactly.
func FormatDuration(d time.Duration) Duration {
	switch {
	case d <= 0:
		return "0s"
	case d%(24*time.Hour) == 0:
		return Duration(strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d")
	case d%time.Hour == 0:
		return Duration(strconv.FormatInt(int64(d/time.Hour), 10) + "h")
	case d%time.Minute == 0:
		return Duration(strconv.FormatInt(int64(d/time.Minute), 10) + "m")
	default:
		return Duration(strconv.FormatInt(int64(d.Round(time.Second)/time.Second), 10) + "s")
	}
}

// Settings is an AlertScope spec with every default applied and every duration parsed.
type Settings struct {
	Window                time.Duration
	ResyncInterval        time.Duration
	NoisyFiringsAbove     int
	FlappingPerDayAbove   float64
	FlappingMedianBelow   time.Duration
	NeverFiredAfter       time.Duration
	SilencedFractionAbove float64
	QueryTimeout          time.Duration
	MaxConcurrent         int
	MaxQueriesPerMinute   int
	TeamFrom              []TeamSource
	RequireRunbook        bool
	RequireSeverity       []string
	SilenceMode           SilenceMode
	SilenceMaxDuration    time.Duration
	RestrictSilences      bool
}

// Settings returns the spec with defaults applied.
func (s *AlertScopeSpec) Settings() Settings {
	out := Settings{
		Window:                s.Quality.Window.OrDefault(DefaultWindow),
		ResyncInterval:        s.Quality.ResyncInterval.OrDefault(DefaultResyncInterval),
		NoisyFiringsAbove:     int(DefaultNoisyFiringsAbove),
		FlappingPerDayAbove:   float64(DefaultFlappingPerDayAbove),
		FlappingMedianBelow:   s.Quality.Flapping.MedianDurationBelow.OrDefault(DefaultFlappingMedianBelow),
		NeverFiredAfter:       s.Quality.NeverFired.After.OrDefault(DefaultNeverFiredAfter),
		SilencedFractionAbove: DefaultSilencedFractionAbove,
		QueryTimeout:          s.QueryBudget.Timeout.OrDefault(DefaultQueryTimeout),
		MaxConcurrent:         int(DefaultMaxConcurrent),
		MaxQueriesPerMinute:   int(DefaultMaxQueriesPerMinute),
		TeamFrom:              s.Ownership.TeamFrom,
		RequireRunbook:        s.Ownership.RequireRunbook == nil || *s.Ownership.RequireRunbook,
		RequireSeverity:       s.Ownership.RequireSeverity,
		SilenceMode:           s.Silences.Mode,
		SilenceMaxDuration:    s.Silences.MaxDuration.OrDefault(DefaultSilenceMaxDuration),
		RestrictSilences:      s.Silences.RestrictToNamespaceRules == nil || *s.Silences.RestrictToNamespaceRules,
	}
	if out.SilenceMode == "" {
		out.SilenceMode = SilenceModeOff
	}
	if v := s.Quality.Noisy.FiringsAbove; v > 0 {
		out.NoisyFiringsAbove = int(v)
	}
	if v := s.Quality.Flapping.ActivationsPerDayAbove; v > 0 {
		out.FlappingPerDayAbove = float64(v)
	}
	if v := s.Quality.AlwaysSilenced.SilencedFractionAbove; v > 0 {
		out.SilencedFractionAbove = v
	}
	if v := s.QueryBudget.MaxConcurrent; v > 0 {
		out.MaxConcurrent = int(v)
	}
	if v := s.QueryBudget.MaxQueriesPerMinute; v > 0 {
		out.MaxQueriesPerMinute = int(v)
	}
	if len(out.TeamFrom) == 0 {
		out.TeamFrom = []TeamSource{
			{RuleLabel: DefaultTeamRuleLabel},
			{ObjectLabel: DefaultTeamObjectOrNamespace},
			{NamespaceLabel: DefaultTeamObjectOrNamespace},
		}
	}
	if len(out.RequireSeverity) == 0 {
		out.RequireSeverity = DefaultSeverities
	}
	return out
}

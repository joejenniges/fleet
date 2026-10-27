// Package dur parses durations that also accept a "d" (day) unit, e.g. "14d", "1d12h".
package dur

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var dayRe = regexp.MustCompile(`(\d+(?:\.\d+)?)d`)

// Parse parses a duration string. Empty string returns 0, nil.
func Parse(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	var days float64
	rest := dayRe.ReplaceAllStringFunc(s, func(m string) string {
		v, _ := strconv.ParseFloat(strings.TrimSuffix(m, "d"), 64)
		days += v
		return ""
	})
	var d time.Duration
	if rest != "" {
		var err error
		d, err = time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
	}
	return d + time.Duration(days*float64(24*time.Hour)), nil
}

// MustParse parses or returns def on error.
func MustParse(s string, def time.Duration) time.Duration {
	d, err := Parse(s)
	if err != nil || s == "" {
		return def
	}
	return d
}

// Duration is a TOML/JSON-friendly duration.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(Format(d.Duration)), nil }

// Format renders a duration compactly, using days where it is a whole number of days.
func Format(d time.Duration) string {
	if d == 0 {
		return ""
	}
	day := 24 * time.Hour
	if d%day == 0 {
		return fmt.Sprintf("%dd", d/day)
	}
	return d.String()
}

// Ago renders "3m ago" style relative times.
func Ago(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

package predict

import (
	"fmt"
	"math"
)

// Label renders a graceful human ETA string for the dashboard. It degrades
// honestly:
//
//   - no remaining work / no rate data → "—"
//   - calendar known → the calendar figure (the real wall-clock ETA), with an
//     "(excl. eval)" suffix when eval timing is not yet measured.
//   - calendar unknown (thin/no availability history) → the work-time figure
//     LABELED "work time (calendar depends on runner availability)" rather than a
//     misleading calendar number.
func (e Estimate) Label() string {
	if !e.HasWork {
		return "—"
	}
	suffix := ""
	if !e.EvalIncluded {
		suffix = " (excl. eval)"
	}
	if e.CalendarKnown {
		return humanizeSeconds(e.CalendarSeconds) + " left" + suffix
	}
	return humanizeSeconds(e.WorkSeconds) + " work time (calendar depends on runner availability)" + suffix
}

// humanizeSeconds renders a non-negative duration in seconds as a compact
// "~Nh" / "~N.N days" string. Mirrors the dashboard's prior humanizeETA scale
// but operates on seconds so the model stays unit-clean.
func humanizeSeconds(secs float64) string {
	if secs <= 0 || math.IsInf(secs, 0) || math.IsNaN(secs) {
		return "—"
	}
	hours := secs / 3600
	switch {
	case hours < 1:
		return "<1h"
	case hours < 48:
		return fmt.Sprintf("~%dh", int(hours+0.5))
	default:
		return fmt.Sprintf("~%.1f days", hours/24)
	}
}

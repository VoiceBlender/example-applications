package main

import "time"

// Metrics is the rolling-window contact-centre KPI bundle surfaced on the
// supervisor panel. Pointer fields are nil when the window has no data
// behind them (no calls to average over, no answers to grade), so the UI
// can render an honest "—" instead of a misleading 0. Counters and
// LongestWaitSecs are plain ints because a real 0 is meaningful for them.
type Metrics struct {
	WindowSeconds     int      `json:"window_seconds"`
	SLAThresholdSecs  int      `json:"sla_threshold_secs"`
	ServiceLevelPct   *float64 `json:"service_level_pct,omitempty"`
	ASASecs           *float64 `json:"asa_secs,omitempty"`
	AHTSecs           *float64 `json:"aht_secs,omitempty"`
	AbandonRatePct    *float64 `json:"abandon_rate_pct,omitempty"`
	LongestWaitSecs   int      `json:"longest_wait_secs"`
	OfferedInWindow   int      `json:"offered_in_window"`
	AnsweredInWindow  int      `json:"answered_in_window"`
	AbandonedInWindow int      `json:"abandoned_in_window"`
}

// computeMetrics derives the supervisor KPI bundle from the active call
// list and the historical call log. Pure: no I/O, no clock reads — `now`
// is passed in so tests are deterministic and so the caller can keep the
// snapshot's timestamp consistent across the rest of the envelope.
//
// `failed` outcomes are excluded from numerators and denominators
// entirely — they're internal-error markers, not customer-visible
// outcomes, and lumping them into Abandon Rate would mislead.
func computeMetrics(
	active []callView,
	log []LogEntry,
	now time.Time,
	window time.Duration,
	slaThreshold time.Duration,
) Metrics {
	out := Metrics{
		WindowSeconds:    int(window.Seconds()),
		SLAThresholdSecs: int(slaThreshold.Seconds()),
	}

	windowStart := now.Add(-window)

	var (
		answered    int
		abandoned   int
		metSLA      int
		waitSum     time.Duration
		handleSum   time.Duration
		eligibleSLA int           // answered + abandoned in window (failed excluded)
		longest     time.Duration // peak wait seen anywhere in window
	)
	for _, e := range log {
		if e.EndedAt.Before(windowStart) {
			continue
		}
		switch e.Outcome {
		case "answered":
			answered++
			eligibleSLA++
			wait := e.AnsweredAt.Sub(e.StartedAt)
			waitSum += wait
			handleSum += e.EndedAt.Sub(e.AnsweredAt)
			if wait <= slaThreshold {
				metSLA++
			}
			if wait > longest {
				longest = wait
			}
		case "abandoned":
			abandoned++
			eligibleSLA++
			// An abandoned call by definition never met SLA.
			if wait := e.EndedAt.Sub(e.StartedAt); wait > longest {
				longest = wait
			}
		}
	}
	out.AnsweredInWindow = answered
	out.AbandonedInWindow = abandoned
	out.OfferedInWindow = answered + abandoned

	if eligibleSLA > 0 {
		pct := 100 * float64(metSLA) / float64(eligibleSLA)
		out.ServiceLevelPct = &pct
		abPct := 100 * float64(abandoned) / float64(eligibleSLA)
		out.AbandonRatePct = &abPct
	}
	if answered > 0 {
		asa := waitSum.Seconds() / float64(answered)
		aht := handleSum.Seconds() / float64(answered)
		out.ASASecs = &asa
		out.AHTSecs = &aht
	}

	// Currently-queued callers count toward the peak too. Their wait is
	// still climbing, so the window peak naturally ratchets upward as the
	// snapshot refreshes.
	for _, c := range active {
		if c.State != "queued" {
			continue
		}
		if w := now.Sub(c.StartedAt); w > longest {
			longest = w
		}
	}
	out.LongestWaitSecs = int(longest.Seconds())

	return out
}

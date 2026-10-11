package dronedash

import (
	"fmt"
	"strings"
	"time"
)

// parseFTTime tries common FPVTrackside time formats. Returns zero on failure.
// FPVTrackside emits .NET-style ISO 8601, sometimes with sub-second precision and
// sometimes without, sometimes with TZ offset and sometimes naive.
// parseFTTime parses a FPVTrackside timestamp, treating naive (no-zone) values
// as UTC. The server uses this on snapshots the venue agent has already
// normalised to UTC (see ftToUTC), so the RFC3339 layouts hit first.
func parseFTTime(s string) time.Time { return parseFTTimeIn(s, time.UTC) }

// parseFTTimeIn parses a FPVTrackside timestamp. Values that carry a zone
// (RFC3339) use it; naive values are interpreted in loc. At the venue, loc is
// time.Local so a wall-clock "15:55" becomes the correct instant.
func parseFTTimeIn(s string, loc *time.Location) time.Time {
	s = strings.TrimSpace(s)
	// FPVTrackside's zero/"not set" sentinel is DateTime.MinValue, serialized as
	// year 0001 in either dash or slash form depending on the machine's culture.
	if s == "" || strings.HasPrefix(s, "0001-01-01") || strings.HasPrefix(s, "0001/01/01") {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.0000000",
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		// FPVTrackside serializes DateTime with the running machine's current
		// culture, so some builds (observed on macOS) emit slash-separated dates
		// with a space and fractional seconds, e.g. "2026/06/04 11:04:46.462" —
		// and a single-digit hour. The fractional ".999999999" is optional, so
		// this one layout also matches the no-fraction "2026/06/04 11:04:46".
		"2006/01/02 15:04:05.999999999",
	}
	for _, l := range layouts {
		// ParseInLocation honours an explicit zone in the value (RFC3339) and
		// falls back to loc for naive layouts.
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ftToUTC converts a FPVTrackside venue-local timestamp string to a UTC
// RFC3339Nano string. It MUST run at the venue (the dd-pits agent or the
// in-process poller), where time.Local is the venue's timezone — the cloud
// server runs in UTC and can't know the venue offset. Empty/zero sentinels and
// already-UTC (RFC3339) values pass through unchanged (idempotent).
func ftToUTC(s string) string {
	t := parseFTTimeIn(s, time.Local)
	if t.IsZero() {
		return s
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// normalizeSnapshotTimes rewrites every FPVTrackside local wall-clock timestamp
// in snap to UTC so storage and the live-race timer are timezone-correct
// regardless of where the cloud server runs. Idempotent — safe to call on a
// snapshot whose ended races were reused (already UTC) from the previous tick.
func normalizeSnapshotTimes(snap *Snapshot) {
	snap.Event.Start = ftToUTC(snap.Event.Start)
	snap.Event.End = ftToUTC(snap.Event.End)
	for i := range snap.Races {
		snap.Races[i].Start = ftToUTC(snap.Races[i].Start)
		snap.Races[i].End = ftToUTC(snap.Races[i].End)
		for j := range snap.Races[i].Laps {
			snap.Races[i].Laps[j].StartTime = ftToUTC(snap.Races[i].Laps[j].StartTime)
			snap.Races[i].Laps[j].EndTime = ftToUTC(snap.Races[i].Laps[j].EndTime)
		}
		for j := range snap.Races[i].Detections {
			snap.Races[i].Detections[j].Time = ftToUTC(snap.Races[i].Detections[j].Time)
		}
	}
}

// earliestLapStart returns the earliest parseable lap StartTime, or the zero
// time if none of the laps carry a usable timestamp. Used to synthesize a race
// start for FPVTrackside modes that feed laps without stamping a race Start.
func earliestLapStart(laps []ftLap) time.Time {
	var min time.Time
	for _, l := range laps {
		t := parseFTTime(l.StartTime)
		if t.IsZero() {
			continue
		}
		if min.IsZero() || t.Before(min) {
			min = t
		}
	}
	return min
}

// staleHeatGrace is how long a started-but-unended heat may go without a new lap
// before it's treated as abandoned/done (an operator left a false-started heat
// selected). Generous so a normal inter-lap gap or a brief pause never trips it —
// drone/whoop heats lap far more often than this.
const staleHeatGrace = 4 * time.Minute

// resultsRetireGrace is the shorter quiet window for retiring a heat that still
// carries finishing results: FT computed results but never stamped an End, so
// once the heat has ALSO stopped lapping for this long it's genuinely finished.
// Crucially this guards against a RE-RUN: when an operator reruns a heat, its old
// results linger but it's actively lapping again, so without a quiet requirement
// every between-lap tick would re-end it and flap the live timer to 0.0. Sized
// well above any real inter-lap gap in a contested heat (some pilot crosses every
// few seconds) but well below staleHeatGrace so a true finish still stops fast.
const resultsRetireGrace = 25 * time.Second

// shouldRetireRunningHeat decides whether a heat FT reports as running (Start
// set, End empty) on a tick with no new lap should be force-ended. Two cases,
// both requiring the heat to have gone quiet so a heat still taking laps — e.g.
// one just rerun, whose prior-run results still exist — is never ended out from
// under live racing:
//   - it has finishing results AND has stopped lapping for resultsRetireGrace
//     (FT finished it but never stamped End); or
//   - it has gone quiet far longer than any real inter-lap gap (abandoned).
//
// A heat with results but no usable lap timestamps can't be gauged for activity,
// so it's treated as done (matching the pre-rerun-fix behaviour). captured is the
// agent's venue clock; zero (old agent / idle tick) never retires.
func shouldRetireRunningHeat(hasResults bool, lastLap, captured time.Time) bool {
	if captured.IsZero() {
		return false
	}
	if hasResults && lastLap.IsZero() {
		return true
	}
	if lastLap.IsZero() {
		return false
	}
	quiet := captured.Sub(lastLap)
	if hasResults && quiet > resultsRetireGrace {
		return true
	}
	return quiet > staleHeatGrace
}

// latestLap returns the most recent lap time in a heat (end, else start), or the
// zero time when the heat has no usable lap timestamps. Used to tell a genuinely
// live heat from one that has gone quiet.
func latestLap(laps []ftLap) time.Time {
	var max time.Time
	for _, l := range laps {
		t := parseFTTime(l.EndTime)
		if t.IsZero() {
			t = parseFTTime(l.StartTime)
		}
		if t.After(max) {
			max = t
		}
	}
	return max
}

// raceStateIsStale reports whether an incoming snapshot's race state (start_at/
// end_at) should be IGNORED because it was captured strictly before the snapshot
// that last wrote this race's state. Ordering by capture time (the agent's venue
// clock) rather than arrival order stops a stale snapshot from re-asserting an
// old End after a newer one already cleared it — the rerun "timer flaps to 0.0"
// bug, where a full snapshot captured before a rerun arrives (on a separate
// connection) after the delta that cleared the End. When either time is unknown
// (zero — an old agent or an idle tick that carries no CapturedAt) it returns
// false, preserving the prior always-write behaviour.
func raceStateIsStale(incoming, prev time.Time) bool {
	if incoming.IsZero() || prev.IsZero() {
		return false
	}
	return incoming.Before(prev)
}

// parseDuration parses FPVTrackside's TimeSpan-style duration (e.g. "00:00:12.345") into seconds.
func parseDuration(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	var h, m float64
	var sec float64
	fmt.Sscanf(parts[0], "%f", &h)
	fmt.Sscanf(parts[1], "%f", &m)
	fmt.Sscanf(parts[2], "%f", &sec)
	return h*3600 + m*60 + sec
}

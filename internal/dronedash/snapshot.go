package dronedash

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// Snapshot is the wire format venue agents POST to the cloud's
// /dronedash/v2/ingest endpoint. It bundles everything needed to reproduce a
// FPVTrackside tick into a single payload, so the cloud doesn't need any
// outbound connectivity to the venue.
//
// Field types are unexported FPVTrackside shapes, but their fields are
// exported and JSON-tagged — so json.Marshal/Unmarshal work fine across the
// agent → cloud boundary without having to redefine the schema.
type Snapshot struct {
	Event    ftEvent     `json:"event"`
	Channels []ftChannel `json:"channels"`
	Pilots   []ftPilot   `json:"pilots"`
	Rounds   []ftRound   `json:"rounds"`
	Races    []ftRace    `json:"races"`
	Results  []ftResult  `json:"results"`
	// CapturedAt is the agent's wall clock (RFC3339, UTC) at snapshot time.
	// Race start_at is stamped on the venue laptop's clock (FPVTrackside), which
	// is often unsynced; comparing CapturedAt to the server's NOW() at ingest
	// yields the venue↔server offset so the cloud can correct the live timer.
	// Empty for the in-process poller (venue == server, offset 0).
	CapturedAt string `json:"captured_at,omitempty"`
	// Partial marks a snapshot that carries only the currently-running race(s)
	// — and exactly the rounds/pilots/results they reference — rather than the
	// whole meeting. Agents send these between periodic full reconciles so a live
	// heat's laps reach the cloud cheaply on a constrained uplink (a single heat
	// is a few KB vs the whole meeting). It is SAFE on any server because Ingest
	// only ever upserts/reconciles the races present in the snapshot and never
	// deletes races/pilots/laps that are simply absent — so a partial touches
	// only its own heats and leaves the rest of the meeting untouched. The flag
	// is the explicit contract (and a guard against any future whole-meeting
	// reconciliation being added to Ingest). Omitted (false) for full snapshots.
	Partial bool `json:"partial,omitempty"`
	// SelectedRaceID is the FPVTrackside operator's currently-selected race
	// (RaceManager.CurrentRace) from the patched /current endpoint — the heat
	// they've cued up. It lets the dashboard show the next heat before it starts,
	// even when races are run out of order. Empty on stock FPVTrackside builds
	// (no /current endpoint) or when nothing un-run is selected.
	SelectedRaceID string `json:"selected_race_id,omitempty"`
}

// RunningSignature returns a stable key for the set of currently-running heats
// (their source ids, sorted). The agent forces a full push whenever this
// changes — a heat starting or finishing is a structural transition (new
// pilots/channels, finishing results, the is_current handoff) that a
// current-race-only partial can't convey, so it must go through a full snapshot.
func (s Snapshot) RunningSignature() string {
	var ids []string
	for _, r := range s.Races {
		if !parseFTTime(r.Start).IsZero() && parseFTTime(r.End).IsZero() {
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// CurrentRacePartial builds a Partial snapshot carrying only the running
// race(s) plus exactly the rounds, pilots and results they reference. The event
// header and channel catalogue ride along (both small) so channel colours and
// the live event stay fresh. Returns ok=false when no heat is running (there is
// nothing to send incrementally — the caller should send a full instead).
//
// The running race objects are copied verbatim, so each carries its *complete*
// current lap + detection set ("all times"), which is exactly what the server's
// per-race lap reconciliation needs to keep the live heat correct.
func (s Snapshot) CurrentRacePartial() (Snapshot, bool) {
	pilotIDs := map[string]struct{}{}
	roundIDs := map[string]struct{}{}
	raceIDs := map[string]struct{}{}
	var races []ftRace
	add := func(r ftRace) {
		if _, dup := raceIDs[r.ID]; dup {
			return
		}
		races = append(races, r)
		raceIDs[r.ID] = struct{}{}
		roundIDs[r.Round] = struct{}{}
		for _, pc := range r.PilotChannels {
			pilotIDs[pc.Pilot] = struct{}{}
		}
		for _, d := range r.Detections {
			if d.Pilot != "" {
				pilotIDs[d.Pilot] = struct{}{}
			}
		}
	}
	for _, r := range s.Races {
		if !parseFTTime(r.Start).IsZero() && parseFTTime(r.End).IsZero() {
			add(r) // running heat
		}
	}
	// Also carry the operator's cued race (SelectedRaceID) even when it isn't
	// running, so a selection / re-run reaches the cloud as a small current-race
	// partial — not the whole meeting. This is what keeps a selection change cheap.
	if s.SelectedRaceID != "" {
		for _, r := range s.Races {
			if r.ID == s.SelectedRaceID {
				add(r)
				break
			}
		}
	}
	if len(races) == 0 {
		return Snapshot{}, false
	}
	var pilots []ftPilot
	for _, p := range s.Pilots {
		if _, ok := pilotIDs[p.ID]; ok {
			pilots = append(pilots, p)
		}
	}
	var rounds []ftRound
	for _, rd := range s.Rounds {
		if _, ok := roundIDs[rd.ID]; ok {
			rounds = append(rounds, rd)
		}
	}
	var results []ftResult
	for _, res := range s.Results {
		if _, ok := raceIDs[res.Race]; ok {
			results = append(results, res)
		}
	}
	return Snapshot{
		Event:          s.Event,
		Channels:       s.Channels,
		Pilots:         pilots,
		Rounds:         rounds,
		Races:          races,
		Results:        results,
		CapturedAt:     s.CapturedAt,
		Partial:        true,
		SelectedRaceID: s.SelectedRaceID,
	}, true
}

// HasRunningRace returns true when any race in the snapshot has a Start
// timestamp but no End — i.e. FPVTrackside considers a heat in-flight.
// Agents use this to switch to a faster poll cadence so new laps reach the
// cloud (and SSE subscribers) within a couple hundred milliseconds.
// RunningRaceID reports which heat is under way, if any.
//
// Same test as HasRunningRace, but naming the heat — the booth needs to tell a
// cue about THIS race from one about the last, and "is something running" does
// not answer that.
func (s Snapshot) RunningRaceID() (string, bool) {
	for _, r := range s.Races {
		if !parseFTTime(r.Start).IsZero() && parseFTTime(r.End).IsZero() {
			return r.ID, true
		}
	}
	return "", false
}

func (s Snapshot) HasRunningRace() bool {
	for _, r := range s.Races {
		if !parseFTTime(r.Start).IsZero() && parseFTTime(r.End).IsZero() {
			return true
		}
	}
	return false
}

// HasCuedPendingRace returns true when the operator's selected race exists
// and hasn't started yet — a start is likely imminent. Agents use this to
// poll at the fast cadence BEFORE the start, so the pending→running edge
// (and the GO! message riding on it) is detected within one fast tick
// instead of one idle tick.
func (s Snapshot) HasCuedPendingRace() bool {
	if s.SelectedRaceID == "" {
		return false
	}
	for _, r := range s.Races {
		if r.ID == s.SelectedRaceID {
			return parseFTTime(r.Start).IsZero()
		}
	}
	return false
}

// FetchSnapshot pulls every JSON payload needed to ingest one tick of a
// FPVTrackside event in a single round-trip from the agent's perspective.
// Returns an error only when the event payload itself is unreachable; missing
// pilots/channels/etc. are tolerated as empty slices.
func (c *Client) FetchSnapshot(ctx context.Context, eventID string) (Snapshot, error) {
	return c.FetchSnapshotIncremental(ctx, eventID, Snapshot{})
}

// FetchSnapshotIncremental is the cache-aware version of FetchSnapshot. Races
// in prev that already had an End time set are treated as immutable and
// reused verbatim — no per-race HTTP call to FPVTrackside, no JSON parse.
// Everything else (event payload, pilots, channels, rounds, *running* races,
// results) is always refetched fresh.
//
// Pass an empty Snapshot{} to disable caching (full refetch); callers that
// want to force a periodic reconciliation pass should do that occasionally.
//
// Why this is safe: FPVTrackside writes a race's lap log to disk when the
// race finishes, and the file is essentially write-once after that —
// operators rarely re-open a finished race to edit it. If they do, the
// agent's next reconciliation tick (or restart) picks up the change.
func (c *Client) FetchSnapshotIncremental(ctx context.Context, eventID string, prev Snapshot) (Snapshot, error) {
	var snap Snapshot
	events, err := c.FetchEvent(ctx, eventID)
	if err != nil {
		return snap, err
	}
	if len(events) == 0 {
		return snap, errors.New("dronedash: event payload empty")
	}
	snap.Event = events[0]

	if channels, err := c.FetchChannels(ctx); err == nil {
		snap.Channels = channels
	}
	if pilots, err := c.FetchPilots(ctx, eventID); err == nil {
		snap.Pilots = pilots
	}
	if rounds, err := c.FetchRounds(ctx, eventID); err == nil {
		snap.Rounds = rounds
	}

	// Build a lookup of "ended" races we saw last tick. A race is reusable
	// only if FPVTrackside marked it ended (End time set) AND it was for the
	// same FT event id; we don't try to detect mid-race edits.
	cached := make(map[string]ftRace, len(prev.Races))
	if prev.Event.ID == snap.Event.ID {
		for _, r := range prev.Races {
			if !parseFTTime(r.End).IsZero() {
				cached[r.ID] = r
			}
		}
	}

	// Results live per-race in {eventID}/{raceID}/Result.json; the event-level
	// Results.json is not reliably populated. Reuse results carried in the prev
	// snapshot for cached (ended) heats so we don't re-fetch them every tick.
	prevResultsByRace := map[string][]ftResult{}
	for _, res := range prev.Results {
		prevResultsByRace[res.Race] = append(prevResultsByRace[res.Race], res)
	}

	for _, raceSrcID := range snap.Event.Races {
		if hit, ok := cached[raceSrcID]; ok {
			snap.Races = append(snap.Races, hit)
			// Ended heat reused from cache — reuse its results too, or fetch
			// once if the prev snapshot didn't carry them.
			if rr, ok := prevResultsByRace[raceSrcID]; ok {
				snap.Results = append(snap.Results, rr...)
			} else if rr, err := c.FetchRaceResults(ctx, eventID, raceSrcID); err == nil {
				snap.Results = append(snap.Results, rr...)
			}
			continue
		}
		races, err := c.FetchRace(ctx, eventID, raceSrcID)
		if err != nil || len(races) == 0 {
			continue
		}
		race := races[0]
		snap.Races = append(snap.Races, race)
		// A finished heat has its positions written — pull them so a completed
		// race's result reaches the cloud (and routes back to race brackets).
		if !parseFTTime(race.End).IsZero() {
			if rr, err := c.FetchRaceResults(ctx, eventID, raceSrcID); err == nil {
				snap.Results = append(snap.Results, rr...)
			}
		}
	}
	// Best-effort: the patched FPVTrackside exposes the operator's selected race
	// (RaceManager.CurrentRace) at /current, so the dashboard can surface a cued
	// (possibly out-of-order) heat before it starts. Stock builds 404 here, in
	// which case SelectedRaceID stays empty and resolution is unchanged.
	snap.SelectedRaceID, _ = c.FetchSelectedRace(ctx)

	// Convert FPVTrackside's venue-local wall-clock times to UTC here, at the
	// venue (time.Local = venue tz), so the cloud stores correct instants and the
	// live race timer isn't off by the venue's UTC offset.
	normalizeSnapshotTimes(&snap)
	return snap, nil
}

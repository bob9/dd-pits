package main

// Taking FPVTrackside's events as they happen, instead of polling for them.
//
// WHY THIS EXISTS
//
// Calls were reaching pilots badly late — five or six seconds behind the lap.
// Everything measurable in our own chain was fast: FPVTrackside answers in under
// a millisecond, the tick runs at 100ms, a lap call reaches first audio in about
// 106ms. The delay was upstream of all of it, in the gap between a lap being
// detected and it appearing in the Race.json we poll.
//
// That gap is not ours to close by polling harder. FPVTrackside writes that file
// when it writes it — a fork patch already exists for exactly this, forcing
// db.Update(currentRace) so sector splits reach Race.json immediately instead of
// at the pilot's next lap end.
//
// So we stop asking and let it tell us. FPVTrackside's ExtensionNotifier PUTs
// every event as it occurs, carrying its OWN timestamp and a sequence number.
//
// WHAT THIS DOES NOT DO
//
// It does not replace the snapshot. These are deltas — they say what changed,
// never what is. The roster, the channels, the completed heats and the
// leaderboard all still come from the poll, which also stays as the recovery
// path when a sequence gap says an event was missed. What moves here is the
// latency-critical half: laps and race-state edges.
//
// FAN-OUT IS BUILT IN, NOT ADDED LATER
//
// NotificationURL is a SINGLE url and FPVTrackside PUTs to one place. A venue
// already driving LED gates or strobes from it would have that silently broken
// the moment dd-pits claimed the address. So dd-pits forwards to whatever was
// there before, and the venue chains rather than chooses.
//
// Forwarding happens BEFORE our own handling: those cues are physical things
// people watch, and our processing time should not be added to theirs.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ftEventMaxBody caps a single event. PositionSnapshot carries the whole field,
// so these are not tiny, but they are not megabytes either.
const ftEventMaxBody = 1 << 20

// ftLagWarn is how far behind an event has to be before it is worth a line.
//
// The point of this receiver is that events arrive immediately, so anything past
// this is news — either FPVTrackside stalled or we did.
const ftLagWarn = 750 * time.Millisecond

// ftEvent is one notification from FPVTrackside.
//
// Only the fields dd-pits acts on are named; the rest of the message rides in
// Raw so it can be forwarded byte-for-byte. Decoding everything would mean this
// struct has to track a schema we do not own, and a field we failed to model
// would be silently dropped from anything we forward.
type ftEvent struct {
	Type string `json:"type"`
	Ts   string `json:"ts"`  // FPVTrackside's own clock, ISO8601 UTC
	Seq  int64  `json:"seq"` // monotonic; a gap means an event was missed

	// Detection (a lap or a sector crossing).
	PilotName    string  `json:"pilotName"`
	LapNumber    int     `json:"lapNumber"`
	IsLapEnd     bool    `json:"isLapEnd"`
	LapTimeSoFar float64 `json:"lapTimeSoFar"`
	RaceTime     float64 `json:"raceTime"`
	Position     int     `json:"position"`
	Valid        bool    `json:"valid"`
	Round        int     `json:"round"`
	Race         int     `json:"race"`

	// TimeSecs is STOCK FPVTrackside's DetectionDetails.Time — seconds since
	// the race start, cumulative, NOT the lap's own duration. The stock
	// RemoteNotifier (armed by NotificationURL alone, no fork) serialises
	// PascalCase with no type discriminator; Go's JSON matching is
	// case-insensitive, so PilotName/LapNumber/IsLapEnd above fill from it
	// already — this is the one field whose NAME differs.
	TimeSecs float64 `json:"time"`

	// Stock PilotCrashedOut: the operator (or video auto-detect) marked a
	// pilot down. Note the pilot rides in "Name" here, not "PilotName".
	Name       string `json:"name"`
	CrashedOut bool   `json:"crashedOut"`

	Raw []byte `json:"-"`
}

// stockLapEnd recognises a lap detection from STOCK FPVTrackside's notifier:
// no Type field (the fork sets one), a pilot, and a cumulative race time.
// Everything else that stock PUTs either has no PilotName (RaceState,
// CaptureDetails, PilotRaceState) or names it differently (PilotCrashedOut's
// "Name"), so the shape is unambiguous.
func (e ftEvent) stockLapEnd() bool {
	return e.Type == "" && e.IsLapEnd && e.PilotName != "" && e.TimeSecs > 0
}

// stockCrashedOut recognises stock's PilotCrashedOut. FPVTrackside only sends
// it while a race is RUNNING (RaceManager guards the event), so arriving at
// all means the moment is live.
func (e ftEvent) stockCrashedOut() bool {
	return e.Type == "" && e.CrashedOut && e.Name != ""
}

// at returns FPVTrackside's timestamp, or zero when it carries none.
func (e ftEvent) at() time.Time {
	if e.Ts == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, e.Ts); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ftEventReceiver accepts FPVTrackside's pushed events.
type ftEventReceiver struct {
	mu       sync.Mutex
	lastSeq  int64
	gaps     int64
	count    int64
	worstLag time.Duration

	// forward is where events are relayed on, for a venue that already had a
	// consumer on this address. Empty is the normal case.
	forward []string
	client  *http.Client

	// onEvent is the announcer's hook. Nil until wired, which is deliberate:
	// the receiver is useful on its own, measuring and forwarding, before
	// anything depends on it for a call.
	onEvent func(ftEvent)
}

func newFTEventReceiver(forward []string) *ftEventReceiver {
	return &ftEventReceiver{
		forward: forward,
		// Short timeout: a downstream consumer that has gone away must not
		// hold up the next event. Losing a forward is a great deal better
		// than falling behind on laps.
		client: &http.Client{Timeout: 2 * time.Second},
	}
}

// ServeHTTP takes one event.
//
// It answers 200 before doing anything expensive. FPVTrackside PUTs these
// synchronously from its own loop, and a receiver that blocks is a receiver that
// slows down the timing system — the exact problem this is here to fix.
func (r *ftEventReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPut && req.Method != http.MethodPost {
		http.Error(w, "PUT or POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, ftEventMaxBody))
	if err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)

	arrived := time.Now()
	go r.handle(body, arrived)
}

// handle decodes, forwards and reports one event.
func (r *ftEventReceiver) handle(body []byte, arrived time.Time) {
	defer func() {
		// A panic in here would take the process down with it, and this runs on
		// every lap of every heat. Nothing this does is worth losing a meeting.
		if p := recover(); p != nil {
			log.Printf("ft events: recovered from %v", p)
		}
	}()

	// Forwarded FIRST, and unparsed. A downstream LED or strobe cue is a
	// physical thing someone watches; our decode time should not be added to it,
	// and a message we fail to understand is still one they might.
	r.fanOut(body)

	var e ftEvent
	if err := json.Unmarshal(body, &e); err != nil {
		log.Printf("ft events: could not read an event: %v", err)
		return
	}
	e.Raw = body

	r.record(e, arrived)

	if r.onEvent != nil {
		r.onEvent(e)
	}
}

// record tracks sequence gaps and lag, and says so when either is real.
func (r *ftEventReceiver) record(e ftEvent, arrived time.Time) {
	r.mu.Lock()
	r.count++
	first := r.count == 1
	// A gap means FPVTrackside sent something we never saw. That matters beyond
	// the missing event: it is the signal that the poll, not the stream, is the
	// authority for state until we have caught up.
	if e.Seq > 0 && r.lastSeq > 0 && e.Seq > r.lastSeq+1 {
		missed := e.Seq - r.lastSeq - 1
		r.gaps += missed
		log.Printf("ft events: missed %d event(s) — seq jumped %d -> %d; the poll "+
			"remains the authority until the next full snapshot",
			missed, r.lastSeq, e.Seq)
	}
	if e.Seq > r.lastSeq {
		r.lastSeq = e.Seq
	}
	var lag time.Duration
	if at := e.at(); !at.IsZero() {
		lag = arrived.Sub(at)
		if lag > r.worstLag {
			r.worstLag = lag
		}
	}
	r.mu.Unlock()

	// Proof of life, once.
	//
	// Everything else here reports only PROBLEMS — a gap, or a late event — which
	// meant a receiver working perfectly and a receiver never contacted looked
	// identical in the log: silence. That is the exact failure this whole session
	// has been about, and it wasted a heat.
	if first {
		log.Printf("ft events: connected — FPVTrackside is pushing events "+
			"(first was %s). Lap calls still come from the poll until the cutover.",
			e.Type)
	}

	// Laps are reported individually while the cutover is pending, because the
	// question being answered is precisely "how far ahead of the poll is this?"
	// and that cannot be read from a summary.
	if e.Type == "DetectionExt" && e.IsLapEnd {
		log.Printf("ft events: LAP %s lap %d in %.2fs (pushed at %s, here after %dms)",
			e.PilotName, e.LapNumber, e.LapTimeSoFar,
			e.at().Local().Format("15:04:05.000"), lag.Milliseconds())
	}
	if e.stockLapEnd() {
		log.Printf("ft events: LAP %s lap %d at race time %.2fs (stock push)",
			e.PilotName, e.LapNumber, e.TimeSecs)
	}

	if lag >= ftLagWarn {
		log.Printf("ft events: %s arrived %.1fs after FPVTrackside recorded it",
			e.Type, lag.Seconds())
	}
}

// fanOut relays the event to any consumer that was here first.
func (r *ftEventReceiver) fanOut(body []byte) {
	for _, url := range r.forward {
		go func(u string) {
			defer func() { _ = recover() }()
			req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := r.client.Do(req)
			if err != nil {
				return // best-effort: their outage is not our problem to escalate
			}
			_ = resp.Body.Close()
		}(url)
	}
}

// stats is what the debug page reports.
func (r *ftEventReceiver) stats() (count, gaps int64, worst time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count, r.gaps, r.worstLag
}

// ftForwardTargets reads the consumers that were here before us.
//
// Set DD_PITS_FT_FORWARD to a comma-separated list of urls. This exists because
// FPVTrackside PUTs to exactly ONE address: a venue already driving LED gates or
// strobes from NotificationURL would have that broken the moment dd-pits took
// the address, and the failure would be silent — FPVTrackside neither knows nor
// reports who is listening.
//
// An env var rather than a settings-page field on purpose: it is set once when a
// venue is wired up, by whoever wired it, and never touched again.
func ftForwardTargets() []string {
	raw := strings.TrimSpace(os.Getenv("DD_PITS_FT_FORWARD"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, u := range strings.Split(raw, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	if len(out) > 0 {
		log.Printf("ft events: forwarding to %d other consumer(s): %s",
			len(out), strings.Join(out, ", "))
	}
	return out
}

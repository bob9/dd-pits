// Package ddapi is the shared DroneDash read-API: the JSON view types and the
// HTTP endpoint logic served identically by the offline dd-pits venue agent and
// the cloud platform server. Both implement the small Source interface (one
// backed by an in-memory FPVTrackside snapshot, the other by the MySQL dd_*
// tables) and register the same routes via Register/Handler — so a client can
// point at either base URL and use the exact same calls and JSON shapes.
//
// JSON keys are PascalCase (the Go field names; no json tags) so the bytes match
// byte-for-byte across both servers.
package ddapi

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── View types (the JSON shapes) ─────────────────────────────────────────────

// Pilot is one pilot's slot in a race: channel + lap timing + result.
type Pilot struct {
	Name         string
	ChannelLabel string
	ChannelColor string
	Holeshot     float64
	Laps         []float64 // racing laps (lap number > 0), full/uncapped
	DispLaps     []float64 // laps to show: races cap at TargetLaps, TTs show all
	Total        float64
	CountedLaps  int
	Position     int
	DNF          bool
	BestLap      float64
	BestConsec   float64 // best-X-consecutive total in this race (TT)
	PBConsec     float64 // event-wide best-X-consecutive (TT)
}

// Race is one heat: identity, state, and its pilots.
type Race struct {
	ID    string
	Round int
	// Order is the heat's round's FPVTrackside Round.Order — the meeting's real
	// running order. Round/Number cannot stand in for it: FPVTrackside allocates
	// RoundNumber per event type and restarts RaceNumber per round, so a meeting
	// that opens with a practice round has Practice Round 1 / Race 1 and TimeTrial
	// Round 1 / Race 1 colliding. 0 on meetings ingested before it was carried,
	// which sorts them together and falls through to the Round/Number tiebreak.
	Order  int
	Number int
	// Practice marks a non-competitive heat (FPVTrackside Practice / CasualPractice).
	// It still renders as a heat; it just must not feed the standings.
	Practice     bool
	RoundName    string
	Label        string
	IsTT         bool
	TypeLabel    string // the heat's own effective type — "Time Trial" | "Race". Unlike IsTT (which the meeting-wide TT display enrichment can set on every heat), this always reflects THIS heat's type, so clients can split qualifying from racing on a mixed meeting.
	ConsecWindow int
	State        string // "pending" | "running" | "ended"
	StartMs      int64  // unix ms (0 = not started)
	TargetLaps   int
	Bracket      bool // heat carries a bracket ExternalID
	ExtID        int  // snapshot ExternalID (bracket_races.id); 0 when not a bracket heat
	Pilots       []Pilot
}

// Standing is one row of the event-wide leaderboard.
type Standing struct {
	Pos          int
	Name         string
	ChannelLabel string
	ChannelColor string
	Laps         int
	Holeshot     float64
	BestLap      float64
	BestConsec   float64
	FastestRace  float64
	NextStatus   string // "Racing" | "Staging" | "<n>" | ""
}

// Channel is one VTX channel in the meeting's channel set (the frequency grid).
// Lets a client show the whole spectrum — incl. channels no pilot is on this heat
// — without inferring it from the current race's pilots.
type Channel struct {
	Label     string // e.g. "R1" (DisplayName, else ShortBand+Number)
	Band      string
	ShortBand string
	Number    int
	Frequency int    // MHz
	Color     string // hex; may be ""
}

// Meeting is the whole meeting view.
type Meeting struct {
	MeetingID string // the meeting/event id (dd_events.id on the cloud, FPVTrackside event id locally)
	Name      string
	IsTT      bool
	Window    int
	Races     []Race // all races, start order
	Current   *Race
	Recent    []Race // ended, most-recent first
	Upcoming  []Race // next pending races (capped for display)
	// AllUpcoming is every pending race in running order, for the venue's
	// Upcoming page. Not part of the JSON API, which keeps its shape.
	AllUpcoming   []Race `json:"-"`
	UpcomingTotal int
	Board         []Standing
	Channels      []Channel // the meeting's VTX channel set, frequency order
}

// RoundDetail is one round with its races (from /round.json).
type RoundDetail struct {
	Round     int
	RoundName string
	RaceCount int
	Pending   int
	Running   int
	Ended     int
	Races     []Race
}

// RoundSummary is a round without its race bodies (from /rounds.json).
type RoundSummary struct {
	Round     int
	RoundName string
	RaceCount int
	Pending   int
	Running   int
	Ended     int
	RaceIDs   []string
}

// PilotView is one pilot's meeting-wide detail (from /pilot.json).
type PilotView struct {
	Name     string
	Standing *Standing
	Races    []Race
}

// ── Source: what a backend must provide ──────────────────────────────────────

// Source is the backend behind the API. dd-pits implements it from an in-memory
// snapshot (a single meeting; the id arg is ignored), the cloud from MySQL (many
// meetings, tenant-scoped by request Host).
type Source interface {
	// Meeting returns the view for meeting id. id=="" means the active/current
	// meeting. ok=false when nothing is loaded / the id is unknown.
	Meeting(r *http.Request, id string) (mv *Meeting, ok bool)
	// CurrentMeeting returns the active meeting's id + name (ok=false if none).
	CurrentMeeting(r *http.Request) (id, name string, ok bool)
	// Subscribe returns a channel that signals on every change (for the SSE
	// streams) and a cancel func to release it.
	Subscribe(r *http.Request) (events <-chan struct{}, cancel func())
}

// ── Routing ──────────────────────────────────────────────────────────────────

// Handler returns a ServeMux with every API route registered.
func Handler(src Source) *http.ServeMux {
	mux := http.NewServeMux()
	Register(mux, src)
	return mux
}

// Register adds every API route to mux. The host owns "/", health, etc.
func Register(mux *http.ServeMux, src Source) {
	// The active meeting's id + name — so a client can discover it then use it.
	mux.HandleFunc("/current-meeting.json", func(w http.ResponseWriter, r *http.Request) {
		id, name, ok := src.CurrentMeeting(r)
		if !ok {
			writeRawJSON(w, `{"loaded":false}`)
			return
		}
		writeJSON(w, r, map[string]any{"loaded": true, "MeetingID": id, "Name": name})
	})
	// Meeting metadata only.
	mux.HandleFunc("/meeting.json", func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"loaded":false}`)
			return
		}
		var pending, running, ended int
		for _, rc := range mv.Races {
			switch rc.State {
			case "pending":
				pending++
			case "running":
				running++
			case "ended":
				ended++
			}
		}
		writeJSON(w, r, map[string]any{
			"loaded": true, "MeetingID": mv.MeetingID, "Name": mv.Name,
			"IsTT": mv.IsTT, "Window": mv.Window, "RaceCount": len(mv.Races),
			"Pending": pending, "Running": running, "Ended": ended,
			"UpcomingTotal": mv.UpcomingTotal, "PilotCount": len(mv.Board),
			"HasCurrent": mv.Current != nil,
		})
	})
	// Full meeting view.
	mux.HandleFunc("/state.json", func(w http.ResponseWriter, r *http.Request) {
		if mv, ok := resolveMeeting(src, r); ok {
			writeJSON(w, r, mv)
		} else {
			writeRawJSON(w, `{"waiting":true}`)
		}
	})
	// The current race.
	mux.HandleFunc("/current.json", func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"waiting":true}`)
			return
		}
		if mv.Current == nil {
			writeRawJSON(w, `{"current":null}`)
			return
		}
		writeJSON(w, r, mv.Current)
	})
	// The next on-deck race.
	mux.HandleFunc("/next.json", func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"waiting":true}`)
			return
		}
		if len(mv.Upcoming) == 0 {
			writeRawJSON(w, `{"next":null}`)
			return
		}
		writeJSON(w, r, mv.Upcoming[0])
	})
	// All races, filterable.
	mux.HandleFunc("/races.json", func(w http.ResponseWriter, r *http.Request) {
		if mv, ok := resolveMeeting(src, r); ok {
			writeJSON(w, r, FilterRaces(mv.Races, r))
		} else {
			writeRawJSON(w, `{"waiting":true}`)
		}
	})
	// A single race by ?id= OR ?round=&number=.
	mux.HandleFunc("/race.json", func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"waiting":true}`)
			return
		}
		q := r.URL.Query()
		id := strings.TrimSpace(q.Get("id"))
		hasCoord := q.Get("round") != "" && q.Get("number") != ""
		if id == "" && !hasCoord {
			writeJSONError(w, http.StatusBadRequest, "provide ?id, or ?round= and ?number=")
			return
		}
		if id != "" {
			for i := range mv.Races {
				if mv.Races[i].ID == id {
					writeJSON(w, r, mv.Races[i])
					return
				}
			}
			writeJSONError(w, http.StatusNotFound, "race not found: "+id)
			return
		}
		round, number := qInt(r, "round", -1), qInt(r, "number", -1)
		for i := range mv.Races {
			if mv.Races[i].Round == round && mv.Races[i].Number == number {
				writeJSON(w, r, mv.Races[i])
				return
			}
		}
		writeJSONError(w, http.StatusNotFound, "race not found: round "+strconv.Itoa(round)+" race "+strconv.Itoa(number))
	})
	// All rounds (summary).
	mux.HandleFunc("/rounds.json", func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"waiting":true}`)
			return
		}
		rounds := BuildRounds(mv)
		out := make([]RoundSummary, 0, len(rounds))
		for _, rd := range rounds {
			out = append(out, summariseRound(rd))
		}
		writeJSON(w, r, out)
	})
	// A single round with its races.
	mux.HandleFunc("/round.json", func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"waiting":true}`)
			return
		}
		if r.URL.Query().Get("round") == "" {
			writeJSONError(w, http.StatusBadRequest, "missing ?round")
			return
		}
		want := qInt(r, "round", -1)
		for _, rd := range BuildRounds(mv) {
			if rd.Round == want {
				writeJSON(w, r, rd)
				return
			}
		}
		writeJSONError(w, http.StatusNotFound, "round not found: "+strconv.Itoa(want))
	})
	// Event-wide leaderboard (alias /leaderboard.json).
	board := func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"waiting":true}`)
			return
		}
		b := mv.Board
		if limit := qInt(r, "limit", 0); limit > 0 && len(b) > limit {
			b = b[:limit]
		}
		writeJSON(w, r, b)
	}
	mux.HandleFunc("/board.json", board)
	mux.HandleFunc("/leaderboard.json", board)
	// The meeting's VTX channel set (the frequency grid), frequency order — so a
	// client can show every channel incl. ones no pilot is on this heat.
	mux.HandleFunc("/channels.json", func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"waiting":true}`)
			return
		}
		writeJSON(w, r, mv.Channels)
	})
	// Pilots in the meeting.
	mux.HandleFunc("/pilots.json", func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"waiting":true}`)
			return
		}
		q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		out := make([]Standing, 0, len(mv.Board))
		for _, st := range mv.Board {
			if q != "" && !strings.Contains(strings.ToLower(st.Name), q) {
				continue
			}
			out = append(out, st)
		}
		if limit := qInt(r, "limit", 0); limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		writeJSON(w, r, out)
	})
	// One pilot's meeting-wide detail.
	mux.HandleFunc("/pilot.json", func(w http.ResponseWriter, r *http.Request) {
		mv, ok := resolveMeeting(src, r)
		if !ok {
			writeRawJSON(w, `{"waiting":true}`)
			return
		}
		name := r.URL.Query().Get("name")
		if strings.TrimSpace(name) == "" {
			writeJSONError(w, http.StatusBadRequest, "missing ?name")
			return
		}
		pv, found := BuildPilotView(mv, name)
		if !found {
			writeJSONError(w, http.StatusNotFound, "pilot not found: "+name)
			return
		}
		writeJSON(w, r, pv)
	})
	// Live update stream. ?data=current|state pushes the payload per change.
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("data")))
		payload := func() string {
			if mode != "current" && mode != "state" {
				return "tick"
			}
			mv, ok := resolveMeeting(src, r)
			var v any = map[string]bool{"waiting": true}
			switch {
			case !ok:
			case mode == "state":
				v = mv
			case mv.Current != nil:
				v = mv.Current
			default:
				v = map[string]any{"current": nil}
			}
			b, _ := json.Marshal(v)
			return string(b)
		}
		serveSSE(w, r, src, func(bool) (string, bool) { return payload(), true })
	})
	// Edge-triggered current-race stream: pushes the current race JSON only when
	// the current heat changes (different heat, or running→ended) — not per lap.
	mux.HandleFunc("/current.stream", func(w http.ResponseWriter, r *http.Request) {
		last := ""
		serveSSE(w, r, src, func(initial bool) (string, bool) {
			sig, data := currentSig(src, r)
			if !initial && sig == last {
				return "", false
			}
			last = sig
			return data, true
		})
	})
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		writeRawJSON(w, specWithBase(r))
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, swaggerUIPage)
	})
}

// ── Internals ────────────────────────────────────────────────────────────────

// specWithBase returns the OpenAPI spec with its server URL set to the path this
// API is mounted under, so Swagger UI "Try it out" hits the right URL on both the
// local agent (mounted at "/") and the cloud (mounted at "/dronedash/api"). The
// mount path is recovered from the request target (RequestURI), which survives
// http.StripPrefix — r.URL.Path does not.
func specWithBase(r *http.Request) string {
	reqPath := r.RequestURI
	if i := strings.IndexByte(reqPath, '?'); i >= 0 {
		reqPath = reqPath[:i]
	}
	base := strings.TrimSuffix(reqPath, "/openapi.json")
	if base == "" {
		base = "/"
	}
	return strings.Replace(openapiSpec, "__BASE__", base, 1)
}

// resolveMeeting reads the optional meeting selector (?meeting= or ?event=) and
// loads it; an empty/absent selector resolves the active meeting.
func resolveMeeting(src Source, r *http.Request) (*Meeting, bool) {
	id := r.URL.Query().Get("meeting")
	if id == "" {
		id = r.URL.Query().Get("event")
	}
	return src.Meeting(r, strings.TrimSpace(id))
}

// currentSig returns a change-signature + JSON body for the current race. The
// signature folds the current race id + state, so it changes when a different
// heat becomes current OR the current heat transitions — but NOT on lap ticks.
func currentSig(src Source, r *http.Request) (sig, data string) {
	mv, ok := resolveMeeting(src, r)
	switch {
	case !ok:
		return "waiting", `{"waiting":true}`
	case mv.Current == nil:
		return "current:null", `{"current":null}`
	default:
		b, _ := json.Marshal(mv.Current)
		return "race:" + mv.Current.ID + "|" + mv.Current.State, string(b)
	}
}

// serveSSE runs the standard SSE loop: an initial frame, a frame per change for
// which next() asks to send, and a named `ping` keepalive every 15s.
func serveSSE(w http.ResponseWriter, r *http.Request, src Source, next func(initial bool) (data string, send bool)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeFrame := func(data string) bool {
		if _, err := io.WriteString(w, "data: "+data+"\n\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	ch, cancel := src.Subscribe(r)
	defer cancel()
	io.WriteString(w, "retry: 2000\n\n")
	if data, send := next(true); send {
		if !writeFrame(data) {
			return
		}
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if data, send := next(false); send {
				if !writeFrame(data) {
					return
				}
			}
		case <-keepalive.C:
			io.WriteString(w, "event: ping\ndata: {}\n\n")
			flusher.Flush()
		}
	}
}

// ── Query / filter helpers ───────────────────────────────────────────────────

// FilterRaces applies ?state= ?round= ?bracket= ?id= ?limit= to a race slice.
func FilterRaces(races []Race, r *http.Request) []Race {
	states := qSet(r, "state")
	round := qInt(r, "round", -1)
	bracket := qBoolPtr(r, "bracket")
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	out := make([]Race, 0, len(races))
	for _, rc := range races {
		if len(states) > 0 && !states[rc.State] {
			continue
		}
		if round >= 0 && rc.Round != round {
			continue
		}
		if bracket != nil && rc.Bracket != *bracket {
			continue
		}
		if id != "" && rc.ID != id {
			continue
		}
		out = append(out, rc)
	}
	if limit := qInt(r, "limit", 0); limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// BuildRounds groups a meeting's races by round number (ascending).
func BuildRounds(mv *Meeting) []RoundDetail {
	byNum := map[int]*RoundDetail{}
	order := []int{}
	for _, rc := range mv.Races {
		rv := byNum[rc.Round]
		if rv == nil {
			rv = &RoundDetail{Round: rc.Round, RoundName: rc.RoundName}
			byNum[rc.Round] = rv
			order = append(order, rc.Round)
		}
		if rv.RoundName == "" {
			rv.RoundName = rc.RoundName
		}
		switch rc.State {
		case "pending":
			rv.Pending++
		case "running":
			rv.Running++
		case "ended":
			rv.Ended++
		}
		rv.Races = append(rv.Races, rc)
		rv.RaceCount++
	}
	sort.Ints(order)
	out := make([]RoundDetail, 0, len(order))
	for _, n := range order {
		out = append(out, *byNum[n])
	}
	return out
}

func summariseRound(rd RoundDetail) RoundSummary {
	ids := make([]string, 0, len(rd.Races))
	for _, rc := range rd.Races {
		ids = append(ids, rc.ID)
	}
	return RoundSummary{
		Round: rd.Round, RoundName: rd.RoundName, RaceCount: rd.RaceCount,
		Pending: rd.Pending, Running: rd.Running, Ended: rd.Ended, RaceIDs: ids,
	}
}

// BuildPilotView returns a pilot's standing + every race they appear in (each
// race trimmed to just this pilot). Name match is case-insensitive.
func BuildPilotView(mv *Meeting, name string) (*PilotView, bool) {
	target := strings.ToLower(strings.TrimSpace(name))
	if target == "" {
		return nil, false
	}
	pv := &PilotView{}
	for i := range mv.Board {
		if strings.ToLower(mv.Board[i].Name) == target {
			st := mv.Board[i]
			pv.Standing = &st
			pv.Name = st.Name
			break
		}
	}
	for _, rc := range mv.Races {
		for _, p := range rc.Pilots {
			if strings.ToLower(p.Name) == target {
				clone := rc
				clone.Pilots = []Pilot{p}
				pv.Races = append(pv.Races, clone)
				if pv.Name == "" {
					pv.Name = p.Name
				}
				break
			}
		}
	}
	if pv.Name == "" {
		return nil, false
	}
	return pv, true
}

// writeJSON encodes v with permissive CORS and optional ?pretty=1 indentation.
func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	enc := json.NewEncoder(w)
	if truthy(r.URL.Query().Get("pretty")) {
		enc.SetIndent("", "  ")
	}
	enc.Encode(v)
}

func writeRawJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	io.WriteString(w, body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	io.WriteString(w, `{"error":`+strconv.Quote(msg)+`}`)
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func qInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func qBoolPtr(r *http.Request, key string) *bool {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil
	}
	b := truthy(v)
	return &b
}

func qSet(r *http.Request, key string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range r.URL.Query()[key] {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
				out[p] = true
			}
		}
	}
	return out
}

// WithCORSPreflight answers cross-origin OPTIONS preflight for every route.
func WithCORSPreflight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

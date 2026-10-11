package main

// Tying the meeting to its YouTube stream, locally.
//
// A club streams the day and then wants two things from it: a ▶ Watch link on
// every heat that opens the stream at the moment that heat started, and a
// timestamped chapter list to paste into the video's description. Both come
// from one fact — when the broadcast began — because every heat's start time is
// already in the timing data.
//
// The cloud does this against dd_events/dd_races. Here there is no database, so
// the anchor lives in the settings file and everything else is derived: a heat's
// offset is its start minus the stream's start, and nothing needs storing per
// heat unless an operator nudges one by hand.
//
// SCOPE: the CURRENT meeting. A venue laptop is pointed at one event on a race
// day, and carrying a history of past meetings' stream anchors would mean
// keeping a database of meetings — which is the thing local mode exists to
// avoid. Changing meeting clears the anchor.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/dronedash"
)

// localVideo is the meeting's stream anchor.
type localVideo struct {
	// MeetingID is what the anchor belongs to. A different meeting means the
	// anchor is meaningless, and silently applying it would put every ▶ Watch
	// link at the wrong moment of somebody else's video.
	MeetingID string `json:"meeting_id,omitempty"`

	// StreamURL is the YouTube broadcast.
	StreamURL string `json:"stream_url,omitempty"`

	// StreamStartedAt is when the broadcast began, in UNIX seconds on the
	// VENUE's clock — the same clock the heat start times are on. Keeping both
	// in one clock is what lets the offset be a plain subtraction; converting
	// to a "real" time would need the venue↔server offset the cloud carries and
	// would be wrong by however far this laptop's clock has drifted.
	StreamStartedAt int64 `json:"stream_started_at,omitempty"`

	// PerRace are hand-set overrides, keyed by race id. An operator who has
	// eyeballed one heat in the video wins over the derived offset.
	PerRace map[string]string `json:"per_race,omitempty"`

	// CommentaryDelaySecs holds the generated commentary back this long when
	// played over the stream, because a broadcast usually carries pre-race
	// footage before the racing starts.
	CommentaryDelaySecs int `json:"commentary_delay_secs,omitempty"`
}

// videoConfig / SetVideo are the guarded accessors.
func (c *localConfig) VideoConfig() localVideo {
	if c == nil {
		return localVideo{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Video
}

func (c *localConfig) SetVideo(v localVideo) {
	c.mu.Lock()
	c.Video = v
	c.mu.Unlock()
}

// SetRaceVideo records a hand-set link for one heat, or clears it when empty.
func (c *localConfig) SetRaceVideo(raceID, url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Video.PerRace == nil {
		c.Video.PerRace = map[string]string{}
	}
	if strings.TrimSpace(url) == "" {
		delete(c.Video.PerRace, raceID)
		return
	}
	c.Video.PerRace[raceID] = strings.TrimSpace(url)
}

// watchURLFor returns the ▶ Watch link for a heat, or "".
//
// An explicit per-heat link always wins: it means somebody looked at the video
// and corrected it, which is better evidence than arithmetic.
func (v localVideo) watchURLFor(raceID string, raceStartUnix int64) string {
	if u, ok := v.PerRace[raceID]; ok && u != "" {
		return u
	}
	if v.StreamURL == "" || v.StreamStartedAt == 0 || raceStartUnix == 0 {
		return ""
	}
	off := int(raceStartUnix - v.StreamStartedAt)
	if off < 0 {
		// The heat ran before the stream began — there is nothing to link to,
		// and clamping to 0 would silently point at the wrong moment.
		return ""
	}
	u, err := dronedash.StreamLinkAt(v.StreamURL, off)
	if err != nil {
		return ""
	}
	return u
}

// offsetFor is the heat's position in the stream, in seconds, or -1.
func (v localVideo) offsetFor(raceID string, raceStartUnix int64) int {
	if u, ok := v.PerRace[raceID]; ok && u != "" {
		return dronedash.ParseStreamOffset(u)
	}
	if v.StreamURL == "" || v.StreamStartedAt == 0 || raceStartUnix == 0 {
		return -1
	}
	off := int(raceStartUnix - v.StreamStartedAt)
	if off < 0 {
		return -1
	}
	return off
}

// anchorFrom sets the stream start from one heat the operator has located in
// the video: if THIS heat is at 1:12:30, the stream began that long before it.
//
// One anchor fills the whole field, which is the point — eyeballing every heat
// is the job this removes.
func (v *localVideo) anchorFrom(raceStartUnix int64, offsetSecs int) {
	v.StreamStartedAt = raceStartUnix - int64(offsetSecs)
}

// ── chapters ────────────────────────────────────────────────────────────────

// chapterLine is one row of a YouTube description's chapter list.
type chapterLine struct {
	Offset int
	Label  string
}

// String renders "HH:MM:SS Label", the form YouTube parses into chapters.
func (c chapterLine) String() string {
	return fmt.Sprintf("%s %s", hhmmss(c.Offset), c.Label)
}

func hhmmss(secs int) string {
	if secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d", secs/3600, (secs%3600)/60, secs%60)
}

// buildChapters turns the meeting into a chapter list.
//
// The leading 00:00:00 line is REQUIRED by YouTube: without a chapter at zero
// it ignores the list entirely and shows none of them. That is the single most
// common reason a pasted list "does nothing".
//
// Pure, so the format can be tested without a meeting or a network.
func buildChapters(v localVideo, races []ddapi.Race) []chapterLine {
	out := []chapterLine{{Offset: 0, Label: "Stream Start"}}

	type row struct {
		off   int
		label string
	}
	var rows []row
	for _, r := range races {
		start := raceStartUnix(r)
		off := v.offsetFor(r.ID, start)
		if off < 0 {
			continue // before the stream, or not anchored
		}
		rows = append(rows, row{off: off, label: chapterLabel(r)})
	}
	// By position in the VIDEO, not by race number: a rerun heat happens later
	// in the stream than its number suggests, and a chapter list out of order
	// is rejected by YouTube.
	sort.Slice(rows, func(i, j int) bool { return rows[i].off < rows[j].off })

	for _, r := range rows {
		if r.off == 0 {
			continue // would collide with the required Stream Start line
		}
		out = append(out, chapterLine{Offset: r.off, Label: r.label})
	}
	return out
}

// chapterLabel names a heat the way the dashboard does, so the chapter list and
// the race history agree — someone scrubbing the video is looking for the same
// words they saw on screen.
func chapterLabel(r ddapi.Race) string {
	if t := strings.TrimSpace(r.TypeLabel); t != "" {
		return fmt.Sprintf("%s %d-%d", t, r.Round, r.Number)
	}
	if r.IsTT {
		return fmt.Sprintf("Time Trial %d-%d", r.Round, r.Number)
	}
	return fmt.Sprintf("Race %d-%d", r.Round, r.Number)
}

// chaptersText renders the list for pasting into a video description.
func chaptersText(lines []chapterLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// raceStartUnix is a heat's start on the venue clock, or 0.
func raceStartUnix(r ddapi.Race) int64 {
	if r.StartMs <= 0 {
		return 0
	}
	return r.StartMs / 1000
}

// ── probing the stream's real start ─────────────────────────────────────────

// probeStreamStart asks YouTube when the broadcast actually began.
//
// This is what removes the eyeballing entirely: a live stream (and the VOD it
// becomes) carries its true start instant, so the anchor can be computed rather
// than found by scrubbing.
//
// A plain uploaded video carries no such instant, and that is reported as a
// clear message rather than an error — it is a normal thing to try.
func probeStreamStart(url, apiKey string) (int64, bool, error) {
	if strings.TrimSpace(url) == "" {
		return 0, false, fmt.Errorf("paste the stream's YouTube address first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ss, err := dronedash.FetchStreamStart(ctx, url, apiKey)
	if err != nil {
		return 0, false, err
	}
	if ss.Start.IsZero() {
		return 0, false, fmt.Errorf(
			"that looks like an uploaded video rather than a stream — it carries no start time, " +
				"so anchor it by finding one heat in the video instead")
	}
	return ss.Start.Unix(), ss.IsLive, nil
}

// videoStateFile is where generated commentary manifests live, beside the
// settings so a venue backs them up together.
func commentaryDir(cfgPath string) string {
	return strings.TrimSuffix(cfgPath, ".json") + "-commentary"
}

// ── generated commentary, stored locally ────────────────────────────────────

// localCommentary is one heat's generated commentary.
type localCommentary struct {
	RaceID   string              `json:"race_id"`
	Status   string              `json:"status"` // generating | ready | failed
	Error    string              `json:"error,omitempty"`
	Segments []commentarySegment `json:"segments,omitempty"`
	MadeAt   int64               `json:"made_at,omitempty"`
}

// commentarySegment is one clip and when it belongs in the race.
type commentarySegment struct {
	AtMs int64  `json:"at_ms"` // ms after the heat started
	File string `json:"file"`  // basename, served from the commentary dir
	Text string `json:"text,omitempty"`
}

// commentaryStore holds the generated commentary for this meeting.
//
// In memory with a file per heat: a race day is a few dozen heats, and losing
// them on a restart would mean regenerating audio that costs money and minutes.
type commentaryStore struct {
	mu   sync.RWMutex
	dir  string
	byID map[string]*localCommentary
}

func newCommentaryStore(dir string) *commentaryStore {
	s := &commentaryStore{dir: dir, byID: map[string]*localCommentary{}}
	s.load()
	return s
}

// load re-reads what was generated before a restart.
func (s *commentaryStore) load() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(s.dir + "/" + e.Name())
		if err != nil {
			continue
		}
		var lc localCommentary
		if json.Unmarshal(data, &lc) != nil || lc.RaceID == "" {
			continue
		}
		// A run interrupted by a restart is dead, not pending: nothing is
		// generating any more, and leaving it "generating" makes the page poll
		// for something that will never arrive.
		if lc.Status == "generating" {
			lc.Status = "failed"
			lc.Error = "interrupted — dd-pits restarted while it was being made"
		}
		s.byID[lc.RaceID] = &lc
	}
}

func (s *commentaryStore) get(raceID string) (*localCommentary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.byID[raceID]
	return c, ok
}

func (s *commentaryStore) put(c *localCommentary) {
	s.mu.Lock()
	s.byID[c.RaceID] = c
	s.mu.Unlock()
	s.save(c)
}

func (s *commentaryStore) save(c *localCommentary) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.dir+"/"+safeRaceFile(c.RaceID)+".json", data, 0o644)
}

// safeRaceFile keeps a race id usable as a filename. Race ids come from
// FPVTrackside and are not guaranteed to be path-safe.
func safeRaceFile(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

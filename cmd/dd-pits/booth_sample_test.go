package main

// Live-gated "how would the booth have sounded" harness: replays one heat of
// a REAL historical meeting (fetched from a cloud DroneDash read-API) through
// the actual booth pipeline — same beats, same prompts, same voices — and
// stitches the calls into a single broadcast-sample WAV.
//
//	RUN_NET_TESTS=1 \
//	BOOTH_SAMPLE_API=https://aufpv.org.au/dronedash/api \
//	BOOTH_SAMPLE_WAV=/tmp/booth-sample.wav \
//	GEMINI_API_KEY=… (or GEMINI_PROJECT_ID=…) \
//	go test ./cmd/dd-pits/ -run TestBoothHistoricalBroadcast -v
//
// Optional: BOOTH_SAMPLE_RACE=<race id> picks a heat (default: the last ended
// heat with pilots); BOOTH_SAMPLE_PACE=21s spaces the TTS calls (free tier).

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	boothpkg "github.com/bob9/dd-pits/internal/booth"

	"github.com/bob9/dd-pits/internal/gemini"
)

func fetchJSON(t *testing.T, url string, out interface{}) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func TestBoothHistoricalBroadcast(t *testing.T) {
	if os.Getenv("RUN_NET_TESTS") != "1" {
		t.Skip("set RUN_NET_TESTS=1 to run the historical booth sample")
	}
	api := strings.TrimRight(os.Getenv("BOOTH_SAMPLE_API"), "/")
	wavPath := os.Getenv("BOOTH_SAMPLE_WAV")
	if api == "" || wavPath == "" {
		t.Skip("set BOOTH_SAMPLE_API and BOOTH_SAMPLE_WAV")
	}
	cfg := gemini.Config{
		APIKey:          os.Getenv("GEMINI_API_KEY"),
		ProjectID:       os.Getenv("GEMINI_PROJECT_ID"),
		Location:        os.Getenv("GEMINI_LOCATION"),
		CredentialsFile: os.Getenv("GEMINI_CREDENTIALS_FILE"),
		Model:           os.Getenv("GEMINI_MODEL"),
	}
	if cfg.APIKey == "" && cfg.ProjectID == "" {
		t.Skip("GEMINI_API_KEY or GEMINI_PROJECT_ID not set")
	}
	gem, err := gemini.New(cfg)
	if err != nil {
		t.Fatalf("gemini: %v", err)
	}
	pace := 2 * time.Second
	if p, err := time.ParseDuration(os.Getenv("BOOTH_SAMPLE_PACE")); err == nil {
		pace = p
	}

	// Assemble a meetingView from the read-API (prod's meeting.json may be an
	// older build; races.json + board.json + current-meeting.json always work).
	var meta struct{ MeetingID, Name string }
	fetchJSON(t, api+"/current-meeting.json", &meta)
	mv := &meetingView{Name: meta.Name}
	fetchJSON(t, api+"/races.json", &mv.Races)
	fetchJSON(t, api+"/board.json", &mv.Board)

	// Pick the heat: BOOTH_SAMPLE_RACE, else the last ended heat with pilots.
	wantID := os.Getenv("BOOTH_SAMPLE_RACE")
	var race *vmRace
	for i := range mv.Races {
		r := &mv.Races[i]
		if wantID != "" {
			if r.ID == wantID {
				race = r
			}
			continue
		}
		if r.State == "ended" && len(r.Pilots) > 0 {
			race = r
		}
	}
	if race == nil {
		t.Fatalf("no suitable heat found (meeting %q, %d races)", mv.Name, len(mv.Races))
	}
	// Up-next context: the heats that followed this one in the running order.
	for i := range mv.Races {
		if mv.Races[i].StartMs > race.StartMs && len(mv.Races[i].Pilots) > 0 && len(mv.Upcoming) < 3 {
			mv.Upcoming = append(mv.Upcoming, mv.Races[i])
		}
	}
	t.Logf("meeting: %s — replaying %q (%s) with %d pilots", mv.Name, race.Label, race.TypeLabel, len(race.Pilots))

	// Mid-race view for the progress beat: half the laps, no final positions.
	mid := *race
	mid.Pilots = append([]vmPilot(nil), race.Pilots...)
	for i, p := range mid.Pilots {
		half := append([]float64(nil), p.Laps[:len(p.Laps)/2]...)
		mid.Pilots[i].Laps = half
		mid.Pilots[i].Position = 0
		mid.Pilots[i].Total = 0
	}
	mid.State = "running"

	beats := []boothBeat{
		{kind: "staged", priority: 3, emotion: "calm", context: boothpkg.MeetingContext(mv) + boothpkg.RaceContext(*race, mv, true)},
		{kind: "launch", priority: 4, emotion: "excited", context: boothpkg.MeetingContext(mv) + boothpkg.RaceContext(*race, mv, true)},
		{kind: "holeshot", priority: 4, emotion: "excited", context: boothpkg.RaceContext(mid, mv, false)},
		{kind: "progress", priority: 2, emotion: "tense", context: boothpkg.RaceContext(mid, mv, false)},
		{kind: "finish", priority: 5, emotion: "big-finish", context: boothpkg.MeetingContext(mv) + boothpkg.RaceContext(*race, mv, false) + boothpkg.UpNextContext(mv)},
	}

	b := newBooth(gem)
	ctx := context.Background()
	for i, beat := range beats {
		if i > 0 {
			time.Sleep(pace)
		}
		if err := b.speak(ctx, beat, 0); err != nil {
			t.Fatalf("%s beat: %v", beat.kind, err)
		}
	}

	// Stitch the aired cues (mixer queue, in order) into one broadcast WAV
	// with a beat of silence between calls.
	b.mixer.mu.Lock()
	cues := append([]*boothCue(nil), b.mixer.queue...)
	b.mixer.mu.Unlock()
	if len(cues) == 0 {
		t.Fatal("no cues generated")
	}
	gap := make([]byte, boothSampleRate*2*12/10) // 1.2s
	var pcm []byte
	for i, c := range cues {
		if i > 0 {
			pcm = append(pcm, gap...)
		}
		if c.Rate != boothSampleRate {
			c.PCM = resamplePCM16(c.PCM, c.Rate, boothSampleRate)
		}
		pcm = append(pcm, c.PCM...)
		t.Logf("[%s]\n%s", beats[i].kind, c.Text)
	}
	if err := os.WriteFile(wavPath, wavBytes(pcm, boothSampleRate), 0644); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	fmt.Printf("broadcast sample: %s (%.1fs)\n", wavPath, float64(len(pcm))/float64(boothSampleRate*2))
}

// wavBytes wraps raw 16-bit LE mono PCM in a finite WAV container.
func wavBytes(pcm []byte, rate int) []byte {
	if rate <= 0 {
		rate = boothSampleRate
	}
	buf := make([]byte, 44+len(pcm))
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+len(pcm)))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], 1)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(rate*2))
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(len(pcm)))
	copy(buf[44:], pcm)
	return buf
}

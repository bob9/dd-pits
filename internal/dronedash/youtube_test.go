package dronedash

import (
	"strings"
	"testing"
	"time"
)

func TestParseYouTubeID(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=WyWvvYX5hFA&t=7162s": "WyWvvYX5hFA",
		"https://youtu.be/WyWvvYX5hFA?t=10":                   "WyWvvYX5hFA",
		"https://www.youtube.com/live/WyWvvYX5hFA":            "WyWvvYX5hFA",
		"not a url": "",
	}
	for in, want := range cases {
		if got := ParseYouTubeID(in); got != want {
			t.Errorf("ParseYouTubeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseChapters(t *testing.T) {
	text := `00:00:00 Stream Start
00:06:38 Time Trial 1-2 | Bola 13.41 | Clog wog 15.60 | bob9 18.91
00:34:55 Time Trial 2-3 | Red2rotor 15.82
01:05:18 Time Trial 4-2 | Bola🔥 11.75 | bob9 14.02 | Clog wog 14.35
03:53:32 Race 1-1 | Mirella 1st | Enjenir 2nd | Gashy 3rd
04:34:01 Race 3-2 | Red2rotor 1st | Lucid 2nd | bob9 3rd | Clog wog DNF
05:25:22 Race 8-1 | Mirella 1st | Willman DNF

Pilot Performance Leaderboard (Best Single Lap):
1. Mirella: 10.94s (at 01:46:15 - TT 6-3-Lap 3)`

	got := parseChapters(text)
	if len(got) != 6 {
		t.Fatalf("parsed %d entries, want 6: %+v", len(got), got)
	}
	// 00:06:38 -> 398s, round 1 race 2, pilots Bola/Clog wog/bob9
	e := got[0]
	if e.OffsetSeconds != 398 || e.RoundNumber != 1 || e.RaceNumber != 2 {
		t.Errorf("entry0 = %+v", e)
	}
	wantPilots := []string{"Bola", "Clog wog", "bob9"}
	if len(e.Pilots) != 3 || e.Pilots[0] != "Bola" || e.Pilots[1] != "Clog wog" || e.Pilots[2] != "bob9" {
		t.Errorf("entry0 pilots = %v, want %v", e.Pilots, wantPilots)
	}
	// "bob9" keeps its trailing digit; "Bola🔥" loses the flame.
	if got[2].Pilots[0] != "Bola" {
		t.Errorf("flame strip failed: %q", got[2].Pilots[0])
	}
	// Race 1-1 (offset 03:53:32 = 14012s)
	r := got[3]
	if r.OffsetSeconds != 14012 || r.RoundNumber != 1 || r.RaceNumber != 1 {
		t.Errorf("race entry = %+v", r)
	}
	if got[5].RoundNumber != 8 || got[5].RaceNumber != 1 {
		t.Errorf("last race entry = %+v", got[5])
	}
}

func TestParseStreamStart(t *testing.T) {
	// Fragment shape as embedded in the watch page's playerMicroformatRenderer.
	vod := `..."liveBroadcastDetails":{"isLiveNow":false,"startTimestamp":"2026-07-11T23:03:11+00:00","endTimestamp":"2026-07-12T05:41:02+00:00"}...`
	ss, err := parseStreamStart(vod)
	if err != nil {
		t.Fatalf("vod: %v", err)
	}
	if ss.IsLive {
		t.Error("vod: IsLive = true, want false")
	}
	if want := time.Date(2026, 7, 11, 23, 3, 11, 0, time.UTC).Unix(); ss.Start.Unix() != want {
		t.Errorf("vod: start = %d, want %d", ss.Start.Unix(), want)
	}

	live := `..."liveBroadcastDetails":{"isLiveNow":true,"startTimestamp":"2026-07-11T23:03:11+00:00"}...`
	ss, err = parseStreamStart(live)
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	if !ss.IsLive {
		t.Error("live: IsLive = false, want true")
	}

	// A plain upload has no liveBroadcastDetails at all.
	if _, err := parseStreamStart(`..."videoDetails":{"videoId":"abc"}...`); err == nil {
		t.Error("upload: want error for missing startTimestamp")
	}
	if _, err := parseStreamStart(`"startTimestamp":"not-a-time"`); err == nil {
		t.Error("bad time: want parse error")
	}
}

func TestParseStreamStartBotWall(t *testing.T) {
	// A bot-walled / restricted response has a playabilityStatus but no
	// liveBroadcastDetails — the error must surface YouTube's stated reason,
	// not claim the video is a plain upload.
	body := `{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Sign in to confirm you're not a bot"}}`
	_, err := parseStreamStart(body)
	if err == nil {
		t.Fatal("want error")
	}
	if got := err.Error(); !strings.Contains(got, "LOGIN_REQUIRED") || !strings.Contains(got, "not a bot") {
		t.Errorf("error %q should carry the playability status + reason", got)
	}
	// UNPLAYABLE but metadata present (the MWEB client shape) still parses.
	ok := `{"playabilityStatus":{"status":"UNPLAYABLE"},"microformat":{"playerMicroformatRenderer":{"liveBroadcastDetails":{"isLiveNow":false,"startTimestamp":"2026-07-10T23:01:55+00:00"}}}}`
	ss, err := parseStreamStart(ok)
	if err != nil {
		t.Fatalf("unplayable-with-metadata: %v", err)
	}
	if ss.Start.IsZero() {
		t.Error("unplayable-with-metadata: zero start")
	}
}

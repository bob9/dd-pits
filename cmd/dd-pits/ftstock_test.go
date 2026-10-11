package main

import (
	"encoding/json"
	"testing"
)

// STOCK FPVTrackside's RemoteNotifier — armed by NotificationURL alone, no
// fork — PUTs PascalCase DetectionDetails with no type discriminator and a
// CUMULATIVE race time. The receiver must recognise the shape and the
// announcer must difference consecutive crossings into lap durations.
func TestStockDetectionDecodesAndIsRecognised(t *testing.T) {
	// Verbatim shape from ExternalData/RemoteNotifier.cs (Formatting.Indented).
	body := `{
  "TimingSystemIndex": 0,
  "LapNumber": 2,
  "ChannelBand": "Race",
  "ChannelNumber": 1,
  "Frequency": 5658,
  "Time": 24.31,
  "IsLapEnd": true,
  "IsRaceEnd": false,
  "PilotName": "Bola",
  "Position": 1,
  "DateTime": "2026/08/01 19:03:07"
}`
	var e ftEvent
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatal(err)
	}
	if !e.stockLapEnd() {
		t.Fatalf("a stock lap detection must be recognised: %+v", e)
	}
	if e.PilotName != "Bola" || e.LapNumber != 2 || e.TimeSecs != 24.31 {
		t.Fatalf("stock fields mis-decoded: %+v", e)
	}

	// The fork's dialect is NOT mistaken for stock — it carries a Type.
	fork := ftEvent{Type: "DetectionExt", IsLapEnd: true, PilotName: "Bola", TimeSecs: 5}
	if fork.stockLapEnd() {
		t.Fatal("a fork event must never take the stock path")
	}
	// A stock RaceState has no pilot; a PilotCrashedOut names its pilot "Name",
	// not "PilotName" — neither can match.
	if (ftEvent{IsLapEnd: true, TimeSecs: 3}).stockLapEnd() {
		t.Fatal("a pilotless event must not be treated as a lap")
	}
}

// Stock Time is cumulative, so a lap's duration is the difference against the
// pilot's previous crossing — with the holeshot as lap 1's baseline, silence
// (not a call) when the baseline is missing, and a silent resync when a rerun
// makes time go backwards.
func TestStockLapSecondsDifferencesCumulativeTime(t *testing.T) {
	ra := newRaceAnnouncerAt(&SoundSet{}, newBoothMixer(), nil, "")

	// Holeshot (lap 0): records the baseline, never a lap call.
	if _, ok := ra.stockLapSeconds("r1", "Bola", 0, 2.81); ok {
		t.Fatal("a holeshot is not a lap call")
	}
	// Lap 1: cumulative 8.74 − holeshot 2.81 = 5.93.
	secs, ok := ra.stockLapSeconds("r1", "Bola", 1, 8.74)
	if !ok || secs < 5.92 || secs > 5.94 {
		t.Fatalf("lap 1 duration wrong: %v %v", secs, ok)
	}
	// Lap 2 differences against lap 1's crossing.
	secs, ok = ra.stockLapSeconds("r1", "Bola", 2, 14.0)
	if !ok || secs < 5.25 || secs > 5.27 {
		t.Fatalf("lap 2 duration wrong: %v %v", secs, ok)
	}
	// Another pilot has their own baseline.
	if _, ok := ra.stockLapSeconds("r1", "bob9", 1, 9.0); ok {
		t.Fatal("a lap with no baseline must stay silent — the poll covers it")
	}
	if secs, ok := ra.stockLapSeconds("r1", "bob9", 2, 15.5); !ok || secs != 6.5 {
		t.Fatalf("the silent lap must still have set the baseline: %v %v", secs, ok)
	}
	// A rerun: time goes backwards — resync silently, never announce a negative.
	if _, ok := ra.stockLapSeconds("r1", "Bola", 1, 3.0); ok {
		t.Fatal("time going backwards must resync, not announce")
	}
	if secs, ok := ra.stockLapSeconds("r1", "Bola", 2, 9.1); !ok || secs < 6.09 || secs > 6.11 {
		t.Fatalf("after the resync the next lap computes: %v %v", secs, ok)
	}
}

// The crash-out button never reflects in mid-race timing data (the DNF flag
// lands only at results), so the push is the ONLY live signal for it. Stock
// sends PilotCrashedOut — pilot under "Name", not "PilotName" — and FT only
// fires it while the heat runs, so arriving at all means the moment is live.
func TestStockCrashedOutIsRecognisedAndClaimedOnce(t *testing.T) {
	body := `{
  "Name": "Bola",
  "Phonetic": "",
  "ChannelBand": "Race",
  "ChannelNumber": 1,
  "Frequency": 5658,
  "CrashedOut": true,
  "ManuallySet": true,
  "DateTime": "2026/08/01 19:03:40"
}`
	var e ftEvent
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatal(err)
	}
	if !e.stockCrashedOut() {
		t.Fatalf("a stock crash-out must be recognised: %+v", e)
	}
	if e.stockLapEnd() {
		t.Fatal("a crash-out is not a lap")
	}
	// A lap detection must never read as a crash-out (different name field).
	lap := ftEvent{IsLapEnd: true, PilotName: "Bola", TimeSecs: 8.7}
	if lap.stockCrashedOut() {
		t.Fatal("a lap detection must not read as a crash-out")
	}

	// Push and poll share the once-per-heat claim: whoever sees it first
	// speaks, the other stays quiet — and a new heat resets it.
	rc := NewRaceCaller(&SoundSet{})
	if !rc.claimOut("r1", "Bola") {
		t.Fatal("first claim must win")
	}
	if rc.claimOut("r1", "Bola") {
		t.Fatal("second claim must stay quiet — the call was made")
	}
	if !rc.claimOut("r2", "Bola") {
		t.Fatal("a new heat is a new call")
	}
}

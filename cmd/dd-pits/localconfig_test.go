package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/elrs"
)

// A missing file is a venue's FIRST RUN, not a failure. Refusing to start
// because nothing is configured yet would be a poor introduction, and an
// operator would have no way to configure it — the pages that write the file
// are served by the process that would not start.
func TestMissingLocalConfigIsNotAnError(t *testing.T) {
	cfg, err := loadLocalConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("a missing file must not be an error: %v", err)
	}
	if cfg == nil || len(cfg.PilotList()) != 0 {
		t.Fatal("expected an empty config")
	}
}

// A MALFORMED file is different: silently treating it as empty would strand an
// operator wondering why the pilots they typed are being ignored.
func TestMalformedLocalConfigIsReported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(p, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLocalConfig(p); err == nil {
		t.Fatal("a malformed settings file must be reported, not silently ignored")
	}
}

// The bind phrase is the whole point: it is what a pilot knows, and the goggle
// UID is derived from it exactly as the ExpressLRS firmware does. An operator
// should never have to compute a hash to put a pilot on the OSD.
func TestBindPhraseBecomesTheGoggleUID(t *testing.T) {
	p := localPilot{Callsign: "bob9", BindPhrase: "bob9 rebels", OSDEnabled: true}
	want := elrs.UIDFromPhrase("bob9 rebels").String()
	if got := p.resolvedUID(); got != want {
		t.Errorf("resolvedUID() = %q, want the firmware's derivation %q", got, want)
	}

	// An explicitly-set UID wins, for a pilot who knows theirs and not the phrase.
	p.UID = "AABBCCDDEEFF"
	if got := p.resolvedUID(); got != "aabbccddeeff" {
		t.Errorf("an explicit UID must win, got %q", got)
	}
}

// The local file has to produce EXACTLY the shape the cloud endpoint returns,
// because that is what makes the OSD engine indifferent to where its config
// came from — the whole reason offline goggles are a small change rather than
// a parallel implementation.
func TestLocalConfigProducesTheCloudShape(t *testing.T) {
	cfg, _ := loadLocalConfig(filepath.Join(t.TempDir(), "c.json"))
	cfg.SetPilot(localPilot{Callsign: "bob9", BindPhrase: "bob9 rebels", OSDEnabled: true})
	cfg.SetPilot(localPilot{Callsign: "fizz", BindPhrase: "fizz fpv", OSDEnabled: false})
	cfg.SetPilot(localPilot{Callsign: "nouid"}) // no phrase, no id

	out := cfg.ELRSConfig()

	if _, ok := out.Pilots["BOB9"]; !ok {
		t.Error("an enabled pilot must appear, keyed by UPPERCASE callsign")
	}
	if _, ok := out.Pilots["FIZZ"]; ok {
		t.Error("a pilot with the OSD switched off must not receive messages")
	}
	// Opted-out is NOT the same as never-set-up: the engine needs to tell them
	// apart so callsign-derived-UID fallback still skips an opted-out pilot.
	if len(out.OptOut) != 1 || out.OptOut[0] != "FIZZ" {
		t.Errorf("an opted-out pilot must be listed, got %v", out.OptOut)
	}
	if _, ok := out.Pilots["NOUID"]; ok {
		t.Error("a pilot with no phrase and no id cannot be addressed and must be dropped")
	}
}

// Saving and reloading has to round-trip, because the file IS the venue's
// configuration — it outlives the process and any one release.
func TestLocalConfigRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	cfg, _ := loadLocalConfig(path)
	cfg.SetPilot(localPilot{Callsign: "Wizz", BindPhrase: "wizz fpv", OSDEnabled: true})
	cfg.SetBooth(localBooth{Engine: "qwen", QwenKey: "sk-test", Mode: "solo"})
	cfg.SetCalls(localCalls{Source: "trackside"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	back, err := loadLocalConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.PilotList()) != 1 || back.PilotList()[0].BindPhrase != "wizz fpv" {
		t.Error("pilots did not survive the round trip")
	}
	if b := back.BoothConfig(); b.Engine != "qwen" || b.QwenKey != "sk-test" {
		t.Errorf("booth settings did not survive: %+v", b)
	}
	if back.CallsConfig().Source != "trackside" {
		t.Error("the race-call choice did not survive")
	}
}

// The most important rule in the whole feature: a venue with NO voice must keep
// the race day it already had. dd-pits taking over the audio is an improvement,
// not a requirement, and stepping in with nothing to speak with would replace
// working FPVTrackside audio with silence.
func TestCallsFallBackToTracksideWithoutAVoice(t *testing.T) {
	if got := callsSource(localCalls{Source: "ddpits"}, false); got != "trackside" {
		t.Errorf("with no voice the calls must stay with FPVTrackside, got %q", got)
	}
	if got := callsSource(localCalls{}, true); got != "ddpits" {
		t.Errorf("with a voice and no preference dd-pits speaks, got %q", got)
	}
	if got := callsSource(localCalls{Source: "trackside"}, true); got != "trackside" {
		t.Errorf("an explicit choice must be honoured even with a voice, got %q", got)
	}
}

// The operator-facing sentence has to say WHY, because "FPVTrackside is
// speaking" is confusing on its own — it reads as a fault rather than a choice.
func TestCallsSourceIsExplained(t *testing.T) {
	noVoice := describeCallsSource("trackside", false)
	if !strings.Contains(noVoice, "no voice is configured") {
		t.Errorf("with no voice the reason must be stated: %q", noVoice)
	}
	chosen := describeCallsSource("trackside", true)
	if !strings.Contains(chosen, "by choice") {
		t.Errorf("a deliberate choice must read as one: %q", chosen)
	}
}

// A local channel label has to be parsed the same way the queued path does, or
// a pilot ends up on the wrong video frequency mid-heat.
func TestChannelLabelSplitting(t *testing.T) {
	for _, c := range []struct {
		in      string
		band    string
		channel int
		bad     bool
	}{
		{in: "R7", band: "R", channel: 7},
		{in: "f2", band: "F", channel: 2},
		{in: "L1", band: "L", channel: 1},
		{in: "R", bad: true},
		{in: "RX", bad: true},
		{in: "", bad: true},
	} {
		band, ch, err := splitChannelLabel(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q should be rejected, not guessed at", c.in)
			}
			continue
		}
		if err != nil || band != c.band || ch != c.channel {
			t.Errorf("splitChannelLabel(%q) = %q,%d,%v", c.in, band, ch, err)
		}
	}
}

// The transport MODE has to be stored, not inferred from the hostname and port.
//
// The two fields cannot express the choice unambiguously: a netpack plus a
// NAMED port silently means "ignore the netpack", while a netpack plus "auto"
// means "netpack, then USB". Those read identically in a form and behave
// completely differently, so an operator picking "both" must not be able to
// turn it into "USB only" by typing a port name.
func TestBackpackTransportResolvesFromTheChosenMode(t *testing.T) {
	for _, c := range []struct {
		name            string
		in              localELRS
		netpack, serial string
	}{
		{"network only", localELRS{Mode: "netpack", Netpack: "np.local"}, "np.local", ""},
		{"usb only", localELRS{Mode: "usb", Serial: "/dev/cu.usb1"}, "", "/dev/cu.usb1"},
		{"usb with no port named scans", localELRS{Mode: "usb"}, "", "auto"},
		// The one that matters: "both" must force "auto" whatever is typed,
		// because a named port is tried INSTEAD of the netpack, not after it.
		{"both forces a scan", localELRS{Mode: "both", Netpack: "np.local", Serial: "/dev/cu.usb1"}, "np.local", "auto"},
		// A file written before the mode existed keeps meaning what it said.
		{"legacy file is untouched", localELRS{Netpack: "np.local", Serial: "auto"}, "np.local", "auto"},
	} {
		np, ser := c.in.transport()
		if np != c.netpack || ser != c.serial {
			t.Errorf("%s: got netpack=%q serial=%q, want %q / %q", c.name, np, ser, c.netpack, c.serial)
		}
	}
}

// A folder only counts as an installation when it actually holds a profile with
// a Sounds.xml. The point of detection is to find a USABLE install, not a
// directory with a promising name — offering an empty one would hand the
// operator the exact silent failure the page exists to prevent.
func TestTracksideDetectionRequiresRealFiles(t *testing.T) {
	root := t.TempDir()

	// Right name, nothing in it.
	empty := filepath.Join(root, "FPVTrackside")
	if err := os.MkdirAll(filepath.Join(empty, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if inspectTracksideDir(empty) != nil {
		t.Error("a folder with no profiles is not an installation")
	}

	// A real one, with two profiles and a log.
	real := filepath.Join(root, "Real")
	for _, p := range []string{"Profile 1", "Club Night"} {
		if err := os.MkdirAll(filepath.Join(real, "data", p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(real, "data", p, "Sounds.xml"), []byte("<x/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(real, "log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "log", "Log.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	in := inspectTracksideDir(real)
	if in == nil {
		t.Fatal("an installation with profiles and a log must be found")
	}
	if len(in.Profiles) != 2 {
		t.Errorf("both profiles must be offered, got %v", in.Profiles)
	}
	if !in.HasLog {
		t.Error("the log must be reported, since relayed calls depend on it")
	}

	// A profile folder with no Sounds.xml is not selectable: choosing it would
	// leave dd-pits with no wording at all.
	bare := filepath.Join(real, "data", "Empty Profile")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := listTracksideProfiles(real); len(got) != 2 {
		t.Errorf("a profile with no Sounds.xml must not be offered, got %v", got)
	}
}

// The venue's language must reach BOTH halves of the booth: the script is
// WRITTEN in it and the voice SPEAKS it. Setting only one produces English
// words read with a foreign direction, or foreign words read in English — both
// heard as broken rather than as a language choice.
func TestLanguageReachesScriptAndVoice(t *testing.T) {
	st := boothpkg.Style{Language: "Dutch", Accent: "Brabants"}

	clause := st.LanguageClause()
	if !strings.Contains(clause, "Dutch") {
		t.Errorf("the script prompt must name the language: %q", clause)
	}
	// Callsigns are names, not words: translating "Wizz" would make the
	// commentary unusable for the people it is about.
	if !strings.Contains(strings.ToLower(clause), "never translate") {
		t.Errorf("callsigns must be protected from translation: %q", clause)
	}
	// The accent is a regional VARIETY, not English with an accent pasted on.
	if !strings.Contains(clause, "Brabants") {
		t.Errorf("the accent must reach the script: %q", clause)
	}

	// And the voice direction, which is the half that was historically missed —
	// a club whose voice characteristics said "Australian" had its Dutch read
	// with an Australian direction.
	if v := st.VoiceClause(); !strings.Contains(v, "Dutch") {
		t.Errorf("the language must reach the VOICE too: %q", v)
	}
}

// An accent with no language is meaningless — it is a variety OF something —
// and must not leak into the prompt on its own.
func TestAccentWithoutALanguageIsIgnored(t *testing.T) {
	st := boothpkg.Style{Accent: "Brabants"}
	if c := st.LanguageClause(); c != "" {
		t.Errorf("an accent alone is not a language choice: %q", c)
	}
}

// Auditioning a voice in English says nothing about how it calls a race in
// another language — the rhythm, the number handling and the pronunciation all
// change. Testing in English and discovering that at a meeting is exactly the
// failure the audition exists to prevent.
func TestVoiceSampleFollowsTheLanguage(t *testing.T) {
	en := voiceSampleFor("")
	es := voiceSampleFor("Spanish")
	if es == en {
		t.Error("Spanish must be auditioned in Spanish, not in English")
	}

	// Asserted as INVARIANTS rather than exact wording, so improving the line
	// does not fail the test for the wrong reason — which is what happened when
	// the sample moved from a lap time to a finish call.
	//
	// What every sample must carry: a callsign and a figure. Reading those
	// cleanly at speed is the thing the voice is being judged on, and a sample
	// without them tells an operator nothing.
	for lang, sample := range map[string]string{"English": en, "Spanish": es} {
		if !strings.Contains(strings.ToLower(sample), "bob") {
			t.Errorf("the %s sample must name a pilot: %q", lang, sample)
		}
		if !strings.ContainsAny(sample, "0123456789") &&
			!strings.Contains(strings.ToLower(sample), "once") &&
			!strings.Contains(strings.ToLower(sample), "eleven") {
			t.Errorf("the %s sample must carry a lap figure: %q", lang, sample)
		}
	}

	// A language with no written sample still auditions, in English, rather
	// than failing — an honest half-test beats no test.
	if got := voiceSampleFor("Welsh"); got != en {
		t.Errorf("an unwritten language should fall back to the English call, got %q", got)
	}
}

// Absent MUST mean on, for both phases and beats.
//
// A settings file written before these toggles existed — or by a club that
// never opened the page — must not silence part of the broadcast. Defaulting a
// missing value to "off" would quietly remove commentary from venues that never
// asked for that, and the symptom (a booth that says less than it used to) is
// almost impossible to attribute to a config default.
func TestUnsetTogglesLeaveTheBroadcastIntact(t *testing.T) {
	var bc localBooth // nothing configured at all

	for _, p := range []*bool{bc.PhasePre, bc.PhaseIn, bc.PhasePost} {
		if !phaseOn(p) {
			t.Error("an unset phase must be ON")
		}
	}
	for _, kind := range boothpkg.BeatKinds {
		if !bc.beatOn(kind) {
			t.Errorf("an unset beat must be ON: %s", kind)
		}
	}

	// Once the form posts, every box is explicit — so an UNTICKED one sticks
	// rather than reading as "never configured" and turning itself back on.
	off := false
	bc.PhaseIn = &off
	bc.Beats = map[string]bool{"progress": false, "finish": true}
	if phaseOn(bc.PhaseIn) {
		t.Error("an explicitly disabled phase must stay off")
	}
	if bc.beatOn("progress") {
		t.Error("an explicitly disabled beat must stay off")
	}
	if !bc.beatOn("finish") {
		t.Error("an explicitly enabled beat must stay on")
	}
}

// Every beat has to belong to exactly one phase, or a toggle silently controls
// nothing and a beat becomes unreachable from the page.
func TestEveryBeatBelongsToAPhase(t *testing.T) {
	seen := map[string]int{}
	for _, kind := range boothpkg.BeatKinds {
		phase := boothpkg.BeatPhase(kind)
		switch phase {
		case "pre", "in", "post":
			seen[phase]++
		default:
			t.Errorf("beat %q has an unknown phase %q — it would be unreachable from the page", kind, phase)
		}
	}
	for _, p := range []string{"pre", "in", "post"} {
		if seen[p] == 0 {
			t.Errorf("phase %q has no beats — its toggle would do nothing", p)
		}
	}
}

package main

import (
	"os"
	"testing"
	"time"
)

// TestPackFileNameMatchesTheGenerator pins the naming against real files from a
// pack FPVTrackside produced. The filename embeds a hash of the key, so any
// difference in normalisation makes every lookup miss — and the pack would
// silently do nothing rather than fail loudly.
func TestPackFileNameMatchesTheGenerator(t *testing.T) {
	for key, want := range map[string]string{
		"1.00":       "1_00_1b11b266.wav",
		"bob9":       "bob9_17af5517.wav",
		"lap":        "lap_54607cea.wav",
		"in":         "in_41387a9e.wav",
		"1st":        "1st_35d9a56b.wav",
		"dave on r8": "dave_on_r8_bc8af762.wav",
	} {
		if got := packFileName(key); got != want {
			t.Errorf("packFileName(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestPackNormaliseKeepsDecimalsAndDropsPunctuation(t *testing.T) {
	// The decimal point is meaningful — a lap time is one clip — while other
	// punctuation is not part of the spoken key.
	for in, want := range map[string]string{
		"  Bob9  ":          "bob9",
		"21.34":             "21.34",
		"Hurry up, 'bob9'!": "hurry up bob9",
		// The trailing stop SURVIVES, and must: the C# generator keeps '.'
		// anywhere (it cannot tell a decimal point from a full stop), and the
		// filename embeds a hash of this exact string. Diverging here to look
		// tidier would make every lookup miss.
		"Race   over.": "race over.",
	} {
		if got := packNormalise(in); got != want {
			t.Errorf("packNormalise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrimSilenceKeepsTheSpeech(t *testing.T) {
	// Leading/trailing quiet must go — it is what makes concatenated words
	// sound like a list — but the speech between must survive intact.
	// Half a second of silence either side — comfortably more than the 15ms
	// guard pad, which is what a real clip's lead-in looks like.
	const quietSamples = boothSampleRate / 2
	const loudSamples = boothSampleRate / 4

	pcm := make([]byte, 0, (quietSamples*2+loudSamples)*2)
	quiet := make([]byte, quietSamples*2)
	loud := make([]byte, 0, loudSamples*2)
	for i := 0; i < loudSamples; i++ {
		loud = append(loud, 0x00, 0x40) // ~16384
	}
	pcm = append(pcm, quiet...)
	pcm = append(pcm, loud...)
	pcm = append(pcm, quiet...)

	got := trimSilence(pcm)
	if len(got) == 0 {
		t.Fatal("trimmed everything — the speech was removed")
	}
	if len(got) >= len(pcm) {
		t.Fatal("nothing was trimmed")
	}
	if len(got) < len(loud) {
		t.Fatalf("trimmed into the speech: kept %d bytes of %d", len(got), len(loud))
	}
}

func TestTrimSilenceOnAllSilence(t *testing.T) {
	if got := trimSilence(make([]byte, 800)); len(got) != 0 {
		t.Fatalf("silence should trim to nothing, got %d bytes", len(got))
	}
}

func TestDecodeWAVRejectsRubbish(t *testing.T) {
	if _, _, err := decodeWAV([]byte("not a wav at all")); err == nil {
		t.Fatal("a non-WAV must be rejected, not read as audio")
	}
}

func TestRealPackClipDecodes(t *testing.T) {
	dir := defaultVoicePackDir("Profile 1")
	if dir == "" {
		t.Skip("no voice pack")
	}
	raw, err := os.ReadFile(dir + "/1_00_1b11b266.wav")
	if err != nil {
		t.Skip("clip not present")
	}
	pcm, rate, err := decodeWAV(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rate != 24000 {
		t.Errorf("rate = %d, want 24000", rate)
	}
	if len(pcm) == 0 {
		t.Fatal("no samples decoded")
	}
}

// TestPackAssemblyIsImmediate is the guard on the property the pack exists for.
//
// Generating a call locally runs at roughly 1x real time — a two-second call
// takes about two seconds to make — which is why lap times were landing after
// the lap they described. Assembling one from clips must stay in the
// milliseconds; if this ever creeps into tens of milliseconds, something has
// started decoding or generating on the hot path.
func TestPackAssemblyIsImmediate(t *testing.T) {
	dir := defaultVoicePackDir("Profile 1")
	if dir == "" {
		t.Skip("no voice pack on this machine")
	}
	p, err := loadVoicePack(dir)
	if err != nil {
		t.Skip(err)
	}

	calls := []string{
		"bob9 lap 1 in 5.59 in 1st",
		"Holeshot bob9 2.08",
		"Race over",
		"Hurry up bob9",
	}

	for _, c := range calls {
		start := time.Now()
		pcm, ok := p.Render(c)
		took := time.Since(start)
		if !ok {
			t.Logf("%q is not in this pack — falls back to live generation", c)
			continue
		}
		if took > 50*time.Millisecond {
			t.Errorf("%q assembled in %s — the pack exists to make this immediate", c, took)
		}
		if len(pcm) == 0 {
			t.Errorf("%q assembled to no audio", c)
		}
	}
}

// TestPackOnlyServesWholeRecordings is the guard on the decision not to splice.
//
// Stitching word-clips together is audible however carefully the seams are
// crossfaded: each clip was rendered as its own utterance with its own stress,
// so a join lands mid-phrase where a speaker would carry the line through. It
// reads as cut-and-paste because it is.
//
// So the pack answers only for lines it holds as ONE recording; everything else
// is generated fresh.
func TestPackOnlyServesWholeRecordings(t *testing.T) {
	dir := defaultVoicePackDir("Profile 1")
	if dir == "" {
		t.Skip("no voice pack on this machine")
	}
	p, err := loadVoicePack(dir)
	if err != nil {
		t.Skip(err)
	}

	// A line the pack holds whole plays from the pack.
	if _, ok := p.Render("bob9 on R1"); !ok {
		t.Error("a complete recording should be served from the pack")
	}

	// A line it only holds in PIECES must not be assembled — it goes to live
	// generation instead. "bob9" and "1.00" both exist as clips; the sentence
	// joining them does not.
	if !p.has("bob9") || !p.has("1.00") {
		t.Skip("this pack lacks the fragments the test relies on")
	}
	if _, ok := p.Render("bob9 1.00"); ok {
		t.Error("a call was assembled from separate clips — that is the splicing this removed")
	}
}

package qwentts

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"
)

func TestIsClonedVoice(t *testing.T) {
	for _, v := range []string{"Ethan", "Cherry", "Radio Gol", "", "qwen-tts-flash"} {
		if IsClonedVoice(v) {
			t.Errorf("%q wrongly detected as cloned", v)
		}
	}
	for _, v := range []string{
		"qwen-tts-vc-racecommentator-voice-20260726124844815-297d",
		"  qwen-tts-vc-x-voice-1  ",
	} {
		if !IsClonedVoice(v) {
			t.Errorf("%q not detected as cloned", v)
		}
	}
}

// A cloned voice must validate even though it is in no static catalogue, and
// must survive ResolveVoice rather than being swapped for a prebuilt default.
func TestClonedVoiceValidatesAndResolves(t *testing.T) {
	id := ClonedVoicePrefix + "racecommentator-voice-123"
	if !ValidVoice(id) {
		t.Fatalf("cloned id %q rejected by ValidVoice", id)
	}
	if got := ResolveVoice(id, DefaultVoicePBP); got != id {
		t.Errorf("ResolveVoice(%q) = %q — a cloned voice must not fall back", id, got)
	}
	// A genuinely unknown name still falls back.
	if got := ResolveVoice("NotAVoice", DefaultVoicePBP); got != DefaultVoicePBP {
		t.Errorf("unknown voice = %q, want the fallback", got)
	}
}

func TestClonedVoiceName(t *testing.T) {
	cases := map[string]string{
		"qwen-tts-vc-racecommentator-voice-20260726124844815-297d": "racecommentator",
		"qwen-tts-vc-ircF-voice-20260726121418122-8362":            "ircF",
		"unexpected-shape": "unexpected-shape",
	}
	for id, want := range cases {
		if got := ClonedVoiceName(id); got != want {
			t.Errorf("ClonedVoiceName(%q) = %q, want %q", id, got, want)
		}
	}
}

// The cloning and flash models reject each other's voices, so this selection is
// correctness rather than preference.
func TestModelSelectionPerVoiceKind(t *testing.T) {
	c, err := New(Config{APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	if c.vcModel != DefaultVCModel {
		t.Errorf("vcModel = %q, want %q", c.vcModel, DefaultVCModel)
	}
	if c.model == c.vcModel || c.instructModel == c.vcModel {
		t.Error("the cloning model must be distinct from the flash models")
	}
}

// buildWAV wraps pcm in a RIFF/WAVE container. declaredSize overrides the data
// chunk length so DashScope's streaming placeholder can be reproduced.
func buildWAV(pcm []byte, declaredSize int) []byte {
	var out bytes.Buffer
	out.WriteString("RIFF")
	binary.Write(&out, binary.LittleEndian, uint32(0x7FFFFFBF))
	out.WriteString("WAVE")
	out.WriteString("fmt ")
	binary.Write(&out, binary.LittleEndian, uint32(16))
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint32(SampleRate))
	binary.Write(&out, binary.LittleEndian, uint32(SampleRate*2))
	binary.Write(&out, binary.LittleEndian, uint16(2))
	binary.Write(&out, binary.LittleEndian, uint16(16))
	size := declaredSize
	if size < 0 {
		size = len(pcm)
	}
	out.WriteString("data")
	binary.Write(&out, binary.LittleEndian, uint32(size))
	out.Write(pcm)
	return out.Bytes()
}

// The flash models stream bare PCM; the cloning model wraps its first chunk in
// a RIFF header. Appending that verbatim would play 44 bytes of header as
// samples — an audible tick opening every cloned segment.
func TestStripWAVHeader(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}

	if got := stripWAVHeader(buildWAV(pcm, -1)); !bytes.Equal(got, pcm) {
		t.Errorf("well-formed WAV: got %v, want %v", got, pcm)
	}
	// DashScope streams with a 0x7FFFFF.. placeholder length; trusting it would
	// slice past the buffer.
	if got := stripWAVHeader(buildWAV(pcm, 0x7FFFFF9B)); !bytes.Equal(got, pcm) {
		t.Errorf("placeholder size: got %v, want %v", got, pcm)
	}
	// Bare PCM must pass through untouched — the flash path must not regress.
	bare := []byte{9, 9, 9, 9}
	if got := stripWAVHeader(bare); !bytes.Equal(got, bare) {
		t.Errorf("bare PCM altered: got %v", got)
	}
	if got := stripWAVHeader(nil); len(got) != 0 {
		t.Errorf("nil input: got %v", got)
	}
}

// TestClonedVoiceLive renders a real cloned voice end to end, proving the model
// switch and the header strip work against the live API.
// Opt-in: RUN_NET_TESTS=1 QWEN_API_KEY=… go test -run ClonedVoiceLive
func TestClonedVoiceLive(t *testing.T) {
	if os.Getenv("RUN_NET_TESTS") != "1" || os.Getenv("QWEN_API_KEY") == "" {
		t.Skip("set RUN_NET_TESTS=1 and QWEN_API_KEY to run live tests")
	}
	c, err := New(Config{APIKey: os.Getenv("QWEN_API_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cloned, err := c.ListVoices(ctx)
	if err != nil {
		t.Fatalf("ListVoices: %v", err)
	}
	if len(cloned) == 0 {
		t.Skip("no cloned voices enrolled on this account")
	}
	v := cloned[0]
	t.Logf("cloned voice %q (%s)", v.Name(), v.ID)

	pcm, rate, err := c.Speak(ctx, "And they are away, into gate one!", v.ID,
		"Shout with excitement, like a race commentator.", "English")
	if err != nil {
		t.Fatalf("Speak with cloned voice: %v", err)
	}
	if rate != SampleRate {
		t.Errorf("rate = %d, want %d", rate, SampleRate)
	}
	if len(pcm) < SampleRate {
		t.Errorf("suspiciously short audio: %d bytes", len(pcm))
	}
	// The header must be gone: real speech never opens with the ASCII "RIFF".
	if len(pcm) >= 4 && bytes.Equal(pcm[0:4], []byte("RIFF")) {
		t.Error("WAV header leaked into the PCM stream")
	}
}

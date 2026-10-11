package main

// Proving the speakers, without involving the voice.
//
// This exists because its absence cost a day. A tester reported "no commentary
// audio"; the voice test on the settings page worked perfectly — and plays in
// the BROWSER. So the one control that looked like it proved the audio path
// proved the opposite half of it, and the search went to the model, the API key
// and the prompt before reaching the speakers.
//
// A tone needs no model, no key and no network, so what it proves is exactly the
// mixer, the device and nothing else. If the tone plays and speech does not, the
// fault is generation; if the tone does not play, no amount of looking at the
// voice will help.

import (
	"encoding/json"
	"math"
	"net/http"
)

// audioTestSeconds is how long the tone runs — long enough to be unmistakably
// heard and to be interrupted by a cut, short enough not to be annoying.
const audioTestSeconds = 1.5

// tonePCM builds a 440Hz sine at the bus rate, with a short fade at each end.
//
// The fades matter: a square-edged start and stop produces a click that sounds
// exactly like the broken audio this is meant to rule out.
func tonePCM(seconds float64) []byte {
	n := int(float64(boothSampleRate) * seconds)
	out := make([]byte, n*2)
	fade := boothSampleRate / 50 // 20ms
	for i := 0; i < n; i++ {
		amp := 0.28
		if i < fade {
			amp *= float64(i) / float64(fade)
		} else if rem := n - i; rem < fade {
			amp *= float64(rem) / float64(fade)
		}
		v := int16(amp * 32767 * math.Sin(2*math.Pi*440*float64(i)/float64(boothSampleRate)))
		out[i*2] = byte(v)
		out[i*2+1] = byte(v >> 8)
	}
	return out
}

// audioTest plays a tone through whatever is currently playing the bus.
//
// Deliberately goes through the MIXER rather than straight to a device: the
// question being answered is "can I hear dd-pits", and dd-pits is heard through
// the mixer. A test that bypassed it could pass while every call was inaudible.
func (s *liveServer) audioTest(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	if s.mixer == nil {
		jsonError(w, http.StatusServiceUnavailable,
			"there is no audio bus — switch on commentary or race calls first")
		return
	}
	// Marked as a race call, not commentary: a test the operator asked for
	// should not be ducked out of the way by the very machinery it is testing.
	s.mixer.enqueue(&boothCue{
		Kind:     "audio_test",
		Text:     "audio test tone",
		callKey:  SoundKey("AudioTest"),
		priority: callPriorityRaceState,
		PCM:      tonePCM(audioTestSeconds),
		Rate:     boothSampleRate,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"seconds": audioTestSeconds,
		"where":   s.audioWhere(),
	})
}

// audioWhere names what is playing the bus, so the answer says where to listen.
func (s *liveServer) audioWhere() string {
	if s.audioPlaying != nil && s.audioPlaying() {
		if goPlayerAvailable() {
			return "this machine's speakers"
		}
		return "this machine's speakers (via the audio sidecar)"
	}
	return "/booth.wav only — local playback is off, so nothing will come out of this machine"
}

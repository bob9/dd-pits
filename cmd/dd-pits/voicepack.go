package main

// Race calls assembled from PRE-GENERATED clips, instead of generated live.
//
// This is the difference between a call landing on the lap and landing after
// it. Generating speech locally runs at roughly 1x real time on this hardware —
// measured at audio 1.8s / gen 1.75s, 2.3s / 2.57s — so a lap call cannot be
// produced in less than about its own length however it is scheduled. Streaming
// it as it generates does not help either: with no margin over playback the cue
// runs dry mid-word, which is heard as choppiness.
//
// A pack sidesteps the problem entirely. Every word a meeting says is a small,
// closed vocabulary — the pilots, "lap", "in", the ordinals, and every lap time
// to two decimals — so it can all be rendered ONCE, ahead of time, and a call
// becomes a few file reads and a concatenation. Milliseconds, not seconds.
//
// The pack format is FPVTrackside's, deliberately: one already exists on the
// venue machine in the operator's cloned voice, ~7,000 clips, and regenerating
// it here would cost hours and produce something identical.

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// voicePack resolves a call into clips and joins them.
type voicePack struct {
	dir string

	mu    sync.RWMutex
	files map[string]string // normalised key -> filename
	pcm   map[string][]byte // key -> decoded samples
}

// loadVoicePack indexes a pack directory.
//
// The index is built by REVERSING the naming scheme rather than by reading a
// manifest: the filename carries a hash of the key, so a key can be tested for
// presence without knowing what else the pack contains. That keeps this
// independent of how the pack was generated.
func loadVoicePack(dir string) (*voicePack, error) {
	if dir == "" {
		return nil, fmt.Errorf("no voice pack directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read voice pack: %w", err)
	}

	p := &voicePack{
		dir:   dir,
		files: make(map[string]string, len(entries)),
		pcm:   map[string][]byte{},
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".wav") {
			p.files[e.Name()] = e.Name()
		}
	}
	if len(p.files) == 0 {
		return nil, fmt.Errorf("voice pack is empty: %s", dir)
	}
	return p, nil
}

// Count reports how many clips the pack holds.
func (p *voicePack) Count() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.files)
}

// has reports whether the pack can say a key.
func (p *voicePack) has(key string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.files[packFileName(key)]
	return ok
}

// Render returns a call ONLY when the pack holds it as a single recording.
//
// Deliberately not assembled from fragments any more. Stitching word-clips
// together is audible however carefully the seams are crossfaded — each clip
// was rendered as its own utterance with its own stress and intonation, so the
// join lands mid-phrase where a speaker would carry the line through. It reads
// as cut-and-paste, because it is.
//
// So the pack is used for what it is genuinely good at: the lines a meeting
// repeats verbatim, which exist as one recording and play instantly. Anything
// else — a lap time, whose numbers are new every lap — is generated fresh, and
// costs about the length of the call to produce.
//
// That is the trade, stated plainly: fixed calls are instant, novel calls take
// a couple of seconds, and nothing is spliced.
func (p *voicePack) Render(text string) ([]byte, bool) {
	if p == nil {
		return nil, false
	}
	key := packNormalise(text)
	if key == "" || !p.has(key) {
		return nil, false
	}
	pcm, err := p.clip(key)
	if err != nil || len(pcm) == 0 {
		return nil, false
	}
	return pcm, true
}

// clip loads and caches a fragment's samples.
func (p *voicePack) clip(key string) ([]byte, error) {
	p.mu.RLock()
	if pcm, ok := p.pcm[key]; ok {
		p.mu.RUnlock()
		return pcm, nil
	}
	name, ok := p.files[packFileName(key)]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no clip for %q", key)
	}

	raw, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil {
		return nil, err
	}
	pcm, rate, err := decodeWAV(raw)
	if err != nil {
		return nil, err
	}
	if rate != boothSampleRate {
		pcm = resamplePCM16(pcm, rate, boothSampleRate)
	}
	pcm = trimSilence(pcm)

	p.mu.Lock()
	p.pcm[key] = pcm
	p.mu.Unlock()
	return pcm, nil
}

// packNormalise matches the generator's key normalisation exactly.
//
// It has to be exact: the filename embeds a hash of this output, so any
// difference makes every lookup miss and the pack silently does nothing.
func packNormalise(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(c) || unicode.IsDigit(c) || c == '.':
			b.WriteRune(c)
		case unicode.IsSpace(c):
			out := b.String()
			if len(out) > 0 && !strings.HasSuffix(out, " ") {
				b.WriteRune(' ')
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// packFileName reproduces the generator's filename for a key: a readable stem
// plus an FNV-1a hash, so two different keys cannot collide on one file.
func packFileName(key string) string {
	norm := packNormalise(key)

	var stem strings.Builder
	for _, c := range norm {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			stem.WriteRune(c)
		} else {
			stem.WriteRune('_')
		}
	}
	s := stem.String()
	if len(s) > 40 {
		s = s[:40]
	}

	var hash uint32 = 2166136261
	for _, c := range norm {
		hash = (hash ^ uint32(c)) * 16777619
	}
	return fmt.Sprintf("%s_%08x.wav", s, hash)
}

// decodeWAV extracts 16-bit PCM and its sample rate.
func decodeWAV(raw []byte) ([]byte, int, error) {
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a WAV file")
	}

	// Walk the chunks rather than assuming a 44-byte header: a writer may
	// include LIST/fact chunks, and a fixed offset would read metadata as audio.
	rate := 0
	for off := 12; off+8 <= len(raw); {
		id := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := off + 8
		if size < 0 || body+size > len(raw) {
			size = len(raw) - body // truncated: take what is there
		}
		switch id {
		case "fmt ":
			if size >= 16 {
				rate = int(binary.LittleEndian.Uint32(raw[body+4 : body+8]))
			}
		case "data":
			if rate == 0 {
				return nil, 0, fmt.Errorf("data before fmt")
			}
			return raw[body : body+size], rate, nil
		}
		off = body + size
		if size%2 == 1 {
			off++ // chunks are word-aligned
		}
	}
	return nil, 0, fmt.Errorf("no data chunk")
}

// silenceFloor is the amplitude below which a sample is treated as silence.
// Generated speech has a low noise floor rather than true zeros.
const silenceFloor = 300

// trimSilence removes the lead-in and tail from a clip.
//
// This is what stops assembled calls sounding assembled. Each clip is rendered
// as its own utterance and carries the model's natural pause at both ends —
// a fifth of a second or more. Concatenated raw, "bob9 lap 3" gains a gap
// between every word and reads like a list rather than a phrase.
func trimSilence(pcm []byte) []byte {
	n := len(pcm) / 2
	if n == 0 {
		return pcm
	}

	sample := func(i int) int {
		v := int(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		if v < 0 {
			return -v
		}
		return v
	}

	start := 0
	for start < n && sample(start) < silenceFloor {
		start++
	}
	end := n
	for end > start && sample(end-1) < silenceFloor {
		end--
	}
	if start >= end {
		return nil
	}

	// A few milliseconds either side, so a plosive is not clipped and words do
	// not run into each other.
	const pad = boothSampleRate * 15 / 1000
	if start > pad {
		start -= pad
	} else {
		start = 0
	}
	if end+pad < n {
		end += pad
	} else {
		end = n
	}
	return pcm[start*2 : end*2]
}

// defaultVoicePackDir finds the pack FPVTrackside generated for a profile.
//
// Reused rather than regenerated: one already exists on the venue machine in
// the operator's own cloned voice, and making a second copy would cost hours to
// produce something identical.
// voicePackDirFor returns the pack belonging to a SPECIFIC voice, or "".
//
// Picking the biggest pack was fine while a venue had one voice. With two it
// produces a broadcast in two different people: the countdown and "race over"
// come from the pack in one voice, while a lap time not in the pack is
// generated fresh in another. Reported exactly that way — "starting on the tone
// is the wrong voice".
//
// A pack is only usable when it was made in the voice now being spoken.
func voicePackDirFor(profile, voice string) string {
	voice = strings.TrimSpace(voice)
	if voice == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if profile == "" {
		profile = "Profile 1"
	}
	dir := filepath.Join(home, "Documents", "FPVTrackside", "data", profile, "AIVoice", voice)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	return dir
}

func defaultVoicePackDir(profile string) string {
	if profile == "" {
		profile = "Profile 1"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	base := filepath.Join(home, "Documents", "FPVTrackside", "data", profile, "AIVoice")

	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	// A pack per voice; take the one with the most clips, which is the one that
	// finished generating.
	best, bestCount := "", 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		if len(files) > bestCount {
			best, bestCount = dir, len(files)
		}
	}
	return best
}

// ── growing a pack as calls are used ────────────────────────────────────────
//
// Pre-rendering a whole pack for a new voice means thousands of clips and hours
// of waiting, and most of them would never be played: a meeting uses the
// phrases for the pilots actually present. So the pack GROWS instead — each
// call is generated once, kept, and instant every time after.
//
// The first heat with a new voice is therefore slower than the rest, which is
// the right place for the cost to land.

// newVoicePackAt opens or creates a pack directory for one voice.
func newVoicePackAt(dir string) (*voicePack, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p, err := loadVoicePack(dir)
	if err == nil {
		return p, nil
	}
	// Empty is a valid starting point, not a failure.
	return &voicePack{dir: dir, files: map[string]string{}, pcm: map[string][]byte{}}, nil
}

// save writes a freshly generated call into the pack.
//
// Best-effort throughout: failing to cache a clip must never stop it being
// spoken, and a full disk at a meeting should cost speed rather than audio.
func (p *voicePack) save(key string, pcm []byte, rate int) {
	if p == nil || p.dir == "" || len(pcm) == 0 {
		return
	}
	name := packFileName(key)

	p.mu.Lock()
	_, already := p.files[name]
	p.mu.Unlock()
	if already {
		return
	}

	// Written to a temporary name and moved, so a crash mid-write cannot leave a
	// truncated clip that would play as a click for the rest of the meeting.
	tmp := filepath.Join(p.dir, name+".part")
	if err := os.WriteFile(tmp, wavFromPCM(pcm, rate), 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(p.dir, name)); err != nil {
		_ = os.Remove(tmp)
		return
	}

	p.mu.Lock()
	p.files[name] = name
	p.pcm[key] = pcm // instant for the rest of this session too
	p.mu.Unlock()
}

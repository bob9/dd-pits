package qwentts

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// usageInfo is the token accounting a response reports, for the spend meter.
// usageInfo is the accounting a response reports, for the spend meter.
//
// Qwen TTS is billed per INPUT CHARACTER and reports `{"characters":N}` — it
// does not report tokens at all. Decoding only the token fields is how every
// Qwen audio call came to be metered as zero, making the backend look free
// rather than cheap.
type usageInfo struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	Characters   int `json:"characters"`
}

// sseEvent is one `data:` payload of the streaming response.
type sseEvent struct {
	Output struct {
		Audio struct {
			Data string `json:"data"`
			// Some responses carry the CDN url instead of inline data (the
			// non-streaming shape). Captured so a caller can be told why it
			// got no PCM rather than a bare "no audio".
			URL string `json:"url"`
		} `json:"audio"`
		FinishReason string `json:"finish_reason"`
	} `json:"output"`
	Usage *usageInfo `json:"usage"`
	Code  string     `json:"code"`
	Msg   string     `json:"message"`
}

// parseSSE accumulates the base64 PCM chunks of a DashScope SSE stream.
//
// The wire format is `data:{json}` lines (DashScope omits the space after the
// colon that the SSE spec suggests, so both are accepted) interleaved with
// id:/event:/: comment lines, which are ignored. An error can arrive mid-stream
// with a 200 status, so a payload carrying a code is surfaced as an error
// rather than being silently treated as a short stream.
func parseSSE(r io.Reader) ([]byte, int, *usageInfo, error) {
	var pcm []byte
	var usage *usageInfo
	rate := 0
	sc := bufio.NewScanner(r)
	// Audio chunks are large; the default 64KB token limit would break a
	// stream on the first sizeable one.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue // a heartbeat or a shape we don't model; not fatal
		}
		if ev.Code != "" {
			return nil, 0, nil, fmt.Errorf("qwen-tts: %s: %s", ev.Code, ev.Msg)
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if d := ev.Output.Audio.Data; d != "" {
			chunk, err := base64.StdEncoding.DecodeString(d)
			if err != nil {
				return nil, 0, nil, fmt.Errorf("qwen-tts: decode audio chunk: %w", err)
			}
			// The flash models stream bare PCM, but the voice-cloning model
			// wraps its first chunk in a RIFF/WAVE header. Appended as-is those
			// 44 bytes would be played as samples — an audible tick opening
			// every cloned-voice segment — so strip a header when one is there.
			if len(pcm) == 0 {
				chunk = stripWAVHeader(chunk)
			}
			pcm = append(pcm, chunk...)
			rate = SampleRate
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, nil, fmt.Errorf("qwen-tts: read stream: %w", err)
	}
	return pcm, rate, usage, nil
}

// SpeakerVoice maps a script's speaker label ("Ace") to a Qwen voice ("Ethan").
// Mirrors gemini.SpeakerVoice so the two backends configure the same way.
type SpeakerVoice struct {
	Speaker string
	Voice   string
}

// Turn is one contiguous run of a single speaker's lines: what to say and the
// voice to say it in.
type Turn struct {
	Speaker string
	Voice   string
	Text    string
}

// SplitTurns turns a speaker-tagged script into per-voice turns.
//
// Consecutive lines from the same speaker MERGE into one turn — fewer API
// calls, and more importantly the model gets a whole thought to pace rather
// than a series of disconnected fragments. Text before any recognised tag (and
// any line tagged with an unknown speaker) is attributed to the first voice, so
// a model that forgets to label a line still gets spoken instead of dropped.
//
// Pure — no I/O, no clock — so the mapping is unit-testable and a re-render
// produces the identical turn list.
func SplitTurns(text string, voices []SpeakerVoice) []Turn {
	if len(voices) == 0 {
		return nil
	}
	voiceFor := make(map[string]string, len(voices))
	for _, v := range voices {
		if v.Speaker != "" {
			voiceFor[strings.ToLower(v.Speaker)] = v.Voice
		}
	}
	var turns []Turn
	cur := Turn{Speaker: voices[0].Speaker, Voice: voices[0].Voice}
	var buf []string
	flush := func() {
		body := strings.TrimSpace(strings.Join(buf, " "))
		buf = nil
		if body != "" {
			t := cur
			t.Text = body
			turns = append(turns, t)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		speaker, body := splitSpeakerLine(line)
		if speaker != "" {
			if voice, known := voiceFor[strings.ToLower(speaker)]; known {
				if !strings.EqualFold(speaker, cur.Speaker) {
					flush()
					cur = Turn{Speaker: speaker, Voice: voice}
				}
				line = body
			}
			// Unknown speaker: keep the whole line, tag and all — dropping the
			// label would lose words, and it's better read than silent.
		}
		if line != "" {
			buf = append(buf, line)
		}
	}
	flush()
	return turns
}

// splitSpeakerLine pulls a leading "Name:" label off a line. The label must be
// short and free of sentence punctuation so a line like "Right: he's gone" or a
// timestamp isn't mistaken for a speaker tag.
func splitSpeakerLine(line string) (speaker, rest string) {
	i := strings.Index(line, ":")
	if i <= 0 || i > 24 {
		return "", line
	}
	name := strings.TrimSpace(line[:i])
	if name == "" || strings.ContainsAny(name, ".,!?;\"'()") || strings.Contains(name, " ") {
		return "", line
	}
	return name, strings.TrimSpace(line[i+1:])
}

// TrimInstruction bounds a delivery instruction to the documented cap, cutting
// on a rune boundary so a multi-byte character is never split.
func TrimInstruction(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > MaxInstructionRunes {
		return strings.TrimSpace(string(r[:MaxInstructionRunes]))
	}
	return s
}

// Languages are the languages Qwen3-TTS renders, keyed by the platform's own
// display name (commentaryLanguages) → the API's language_type value.
//
// Deliberately not everything the platform offers: a language absent here (the
// platform also lists Polish, Indonesian, Thai, Vietnamese, Hindi, Arabic,
// Afrikaans and Māori) is simply not sent as a hint, and the model falls back
// to auto-detection from the script — which is a softer failure than sending a
// value the API rejects, but it does mean those languages are better served by
// Gemini.
var Languages = map[string]string{
	"English":          "English",
	"Mandarin Chinese": "Chinese",
	"Chinese":          "Chinese",
	"French":           "French",
	"German":           "German",
	"Russian":          "Russian",
	"Italian":          "Italian",
	"Spanish":          "Spanish",
	"Portuguese":       "Portuguese",
	"Japanese":         "Japanese",
	"Korean":           "Korean",
}

// LanguageType maps a platform language name to the API's language_type,
// returning "" for anything the model doesn't list (send no hint).
func LanguageType(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for k, v := range Languages {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// Voice is one selectable Qwen voice.
type Voice struct {
	Name string
	Note string
}

// Voices is the admin-pickable set for qwen3-tts-flash. A curated subset of the
// ~49 the model offers: the broadcast-plausible ones that speak the platform's
// languages. The dialect-locked voices (Cantonese, Sichuan, …) are left out —
// they're for Chinese dialects, not an Australian race booth.
var Voices = []Voice{
	{"Ethan", "warm male — a natural play-by-play caller"},
	{"Cherry", "bright female"},
	{"Ryan", "energetic male"},
	{"Serena", "measured female — suits the analyst chair"},
	{"Chelsie", "clear female"},
	{"Aiden", "youthful male"},
	{"Vivian", "expressive female"},
	{"Moon", "deep male"},
	{"Kai", "crisp male"},
	{"Nofish", "casual male"},
	{"Jennifer", "polished female"},
	{"Katerina", "rich female"},
	{"Arthur", "authoritative male"},
	{"Vincent", "steady male"},
	{"Bella", "light female"},
	{"Maia", "friendly female"},
	{"Neil", "dry male"},
	{"Stella", "confident female"},
	{"Eldric Sage", "gravelly older male"},
	{"Radio Gol", "high-energy broadcast male"},
}

// Default voices: a male caller and a female analyst, mirroring the Gemini
// pairing's contrast so a tenant switching backends sounds familiar.
const (
	DefaultVoicePBP   = "Ethan"
	DefaultVoiceColor = "Cherry"
)

// ValidVoice reports whether a name is one this platform offers.
//
// A CLONED voice id is always accepted: clones are enrolled at runtime, so they
// are in no static list and are recognised by their id prefix instead.
func ValidVoice(name string) bool {
	if IsClonedVoice(name) {
		return true
	}
	for _, v := range Voices {
		if v.Name == name {
			return true
		}
	}
	return false
}

// ClonedVoiceName recovers the name given at enrollment from a cloned voice id
// (DashScope bakes it in as qwen-tts-vc-{name}-voice-{stamp}), falling back to
// the whole id when it doesn't have that shape.
func ClonedVoiceName(id string) string {
	s := strings.TrimPrefix(strings.TrimSpace(id), ClonedVoicePrefix)
	if i := strings.LastIndex(s, "-voice-"); i > 0 {
		return s[:i]
	}
	return id
}

// ResolveVoice falls back to a default for a blank or unknown voice, so a
// tenant whose stored voice is a Gemini name (Puck/Kore) still gets audio
// instead of an API error the first time they switch backends.
func ResolveVoice(name, fallback string) string {
	if ValidVoice(name) {
		return name
	}
	if ValidVoice(fallback) {
		return fallback
	}
	return DefaultVoicePBP
}

// stripWAVHeader returns the sample data from a RIFF/WAVE payload, or the input
// unchanged when it isn't one (the flash models stream bare PCM).
//
// The declared chunk sizes are NOT trusted: DashScope streams its WAVs and
// writes 0x7FFFFF.. placeholders into the RIFF and data lengths, so any size
// that overruns what is actually present means "to the end of the buffer".
func stripWAVHeader(b []byte) []byte {
	if len(b) < 12 || !bytes.Equal(b[0:4], []byte("RIFF")) || !bytes.Equal(b[8:12], []byte("WAVE")) {
		return b
	}
	for pos := 12; pos+8 <= len(b); {
		id := b[pos : pos+4]
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		payload := pos + 8
		avail := len(b) - payload
		if bytes.Equal(id, []byte("data")) {
			if size < 0 || size > avail {
				size = avail
			}
			return b[payload : payload+size]
		}
		if size < 0 || size > avail {
			break
		}
		pos = payload + size
		if size%2 == 1 { // RIFF chunks pad to an even boundary
			pos++
		}
	}
	return b
}

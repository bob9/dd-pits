package main

// Uploading the recording that IS the local voice.
//
// The local route does not enrol a voice anywhere: the reference recording is
// handed to the model on every single generation, so the file itself is the
// voice. That makes where it lives load-bearing in a way a normal setting is
// not — a path into Downloads or a USB stick means the booth goes silent the
// moment somebody tidies up or unplugs it, mid-meeting.
//
// So it is uploaded and kept BESIDE THE SETTINGS FILE, and travels with it.
// Typing a path still works for anyone who wants it; this is for everyone else.

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// voiceRefMaxBytes caps an upload.
//
// Generous next to what is actually wanted — ten to twenty seconds of speech is
// a couple of megabytes — but a whole meeting's recording is not a reference
// clip, and the model pays for its length on every call.
const voiceRefMaxBytes = 30 << 20

// voiceRefPath is where an uploaded recording is kept: next to the settings, so
// backing up one backs up the other.
func voiceRefPath() string { return voiceRefPathFor("") }

// voiceRefPathFor keeps each recording under its own name, so a club can hold
// several commentators rather than overwriting one file every time.
func voiceRefPathFor(name string) string {
	slug := voiceSlug(name)
	if slug == "" {
		slug = "reference"
	}
	return filepath.Join(localStateDir(), "voice-"+slug+".wav")
}

// voiceSlug makes a filename out of a name the operator typed.
//
// Restrictive on purpose: this becomes a path, and a name is free text.
func voiceSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// wavInfo is what we can tell the operator about a recording without decoding
// it — enough to catch the mistakes that matter.
type wavInfo struct {
	Rate     int
	Channels int
	Bits     int
	Seconds  float64
	Bytes    int64
}

// inspectWAV reads a RIFF header.
//
// Checked because the failure it prevents is silent: the model will happily be
// handed a non-audio file or a 40-minute recording and produce something poor
// rather than an error, and the operator would have no idea which of the two
// went wrong.
func inspectWAV(path string) (wavInfo, error) {
	var w wavInfo
	f, err := os.Open(path)
	if err != nil {
		return w, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return w, err
	}
	w.Bytes = st.Size()

	hdr := make([]byte, 44)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return w, fmt.Errorf("too short to be a wav file")
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return w, fmt.Errorf("not a wav file — save it as WAV, not mp3 or m4a")
	}
	w.Channels = int(binary.LittleEndian.Uint16(hdr[22:24]))
	w.Rate = int(binary.LittleEndian.Uint32(hdr[24:28]))
	w.Bits = int(binary.LittleEndian.Uint16(hdr[34:36]))
	if w.Rate > 0 && w.Channels > 0 && w.Bits > 0 {
		w.Seconds = float64(w.Bytes-44) / float64(w.Rate*w.Channels*w.Bits/8)
	}
	return w, nil
}

// advice turns the header into the one or two things worth saying.
func (w wavInfo) advice() string {
	switch {
	case w.Seconds > 0 && w.Seconds < 5:
		return "That is quite short — ten to twenty seconds gives the model much more to copy."
	case w.Seconds > 40:
		return fmt.Sprintf("At %.0f seconds this is longer than it needs to be, and the model "+
			"re-reads it on EVERY call — so a shorter clip is faster all meeting. "+
			"Ten to twenty seconds is plenty.", w.Seconds)
	case w.Channels > 1:
		return "Stereo works, but mono is what the model wants and halves the work per call."
	}
	return ""
}

// uploadVoiceRef takes the recording and points the settings at it.
func (s *liveServer) uploadVoiceRef(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	if err := r.ParseMultipartForm(voiceRefMaxBytes); err != nil {
		http.Redirect(w, r, "/setup/models?err="+urlEscape("That file was too large."),
			http.StatusSeeOther)
		return
	}
	file, hdr, err := r.FormFile("voice_wav")
	if err != nil {
		http.Redirect(w, r, "/setup/models?err="+urlEscape("Choose a .wav file first."),
			http.StatusSeeOther)
		return
	}
	defer func() { _ = file.Close() }()

	if hdr.Size > voiceRefMaxBytes {
		http.Redirect(w, r, "/setup/models?err="+urlEscape("That file was too large."),
			http.StatusSeeOther)
		return
	}

	// Written to a temporary name first and only moved into place once it has
	// been read as a wav — so a bad upload cannot replace a working voice with
	// something the booth will fail on halfway through a meeting.
	name := strings.TrimSpace(r.FormValue("voice_name"))
	if name == "" {
		name = "My commentator"
	}
	dst := voiceRefPathFor(name)
	tmp := dst + ".incoming"
	out, err := os.Create(tmp)
	if err != nil {
		http.Redirect(w, r, "/setup/models?err="+urlEscape("Could not save it: "+err.Error()),
			http.StatusSeeOther)
		return
	}
	if _, err := io.Copy(out, io.LimitReader(file, voiceRefMaxBytes)); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		http.Redirect(w, r, "/setup/models?err="+urlEscape("Could not save it."), http.StatusSeeOther)
		return
	}
	_ = out.Close()

	info, err := inspectWAV(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		http.Redirect(w, r, "/setup/models?err="+urlEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		http.Redirect(w, r, "/setup/models?err="+urlEscape("Could not put it in place."),
			http.StatusSeeOther)
		return
	}

	if s.cfg != nil {
		bc := s.cfg.BoothConfig()
		text := strings.TrimSpace(r.FormValue("voice_ref_text"))
		// Replace an entry of the same name rather than accumulating duplicates —
		// re-uploading is how someone fixes a bad take.
		found := false
		for i := range bc.VoiceRefs {
			if strings.EqualFold(bc.VoiceRefs[i].Name, name) {
				bc.VoiceRefs[i] = localVoiceRef{Name: name, Path: dst, Text: text}
				found = true
			}
		}
		if !found {
			bc.VoiceRefs = append(bc.VoiceRefs, localVoiceRef{Name: name, Path: dst, Text: text})
		}
		// A newly uploaded voice becomes the active one: uploading it is the ask.
		bc.VoiceRef, bc.VoiceRefText = dst, text
		s.cfg.SetBooth(bc)
		_ = s.cfg.Save()
		if s.applyBoothConfig != nil {
			s.applyBoothConfig()
		}
	}
	msg := fmt.Sprintf("Saved %.1fs of audio.", info.Seconds)
	if a := info.advice(); a != "" {
		msg += " " + a
	}
	http.Redirect(w, r, "/setup/models?saved=1&note="+urlEscape(msg), http.StatusSeeOther)
}

// clearVoiceRef goes back to the model's own voice.
func (s *liveServer) clearVoiceRef(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	if s.cfg != nil {
		bc := s.cfg.BoothConfig()
		// The stored copy is removed too: leaving a stale recording next to the
		// settings is how somebody later wonders whose voice that is.
		if bc.VoiceRef == voiceRefPath() {
			_ = os.Remove(bc.VoiceRef)
		}
		bc.VoiceRef, bc.VoiceRefText = "", ""
		s.cfg.SetBooth(bc)
		_ = s.cfg.Save()
		if s.applyBoothConfig != nil {
			s.applyBoothConfig()
		}
	}
	http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
}

// voiceRefStatus describes the current recording for the page.
func voiceRefStatus(bc localBooth) string {
	p := strings.TrimSpace(bc.VoiceRef)
	if p == "" {
		return `<span class="mut">no recording — the local voice uses its own default</span>`
	}
	info, err := inspectWAV(p)
	if err != nil {
		// Said plainly: the file is read on EVERY generation, so a missing one
		// silences the booth rather than degrading it.
		return fmt.Sprintf(`<span style="color:var(--warn,#c88)">⚠ %s cannot be read (%s) —
 the local voice needs it on every call, so commentary will fail until it is fixed.</span>`,
			htmlEsc(filepath.Base(p)), htmlEsc(err.Error()))
	}
	return fmt.Sprintf(`<span style="color:var(--ok,#7c5)">✓ %.1fs of audio</span>
 <span class="mut">(%d Hz, %s, %.1f MB) — %s</span>`,
		info.Seconds, info.Rate,
		map[bool]string{true: "mono", false: "stereo"}[info.Channels <= 1],
		float64(info.Bytes)/(1<<20), htmlEsc(filepath.Base(p)))
}

func htmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

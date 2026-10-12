package main

// The debug page — the log, in the browser.
//
// dd-pits writes everything useful to the log, and for a venue that started it
// by double-clicking, that log is in a Terminal window behind whatever else is
// on screen. Diagnosing a remote report meant asking someone to find that
// window, scroll back far enough to catch the startup lines, select all, and
// paste — and the startup lines are the ones that matter most, so they are
// exactly the ones lost to scrollback.
//
// A page they can navigate to, with a button that copies the lot, removes all
// of that.
//
// IT IS REDACTED, and that is not decoration: the whole point is that this text
// gets pasted into a chat window to send to someone else. An API key appearing
// in an error message would be handed over with it.

import (
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// logRingSize is how many lines to keep.
//
// Enough to cover a meeting's worth of beats and still hold the startup lines
// at the top, which are the first thing worth reading.
const logRingSize = 800

// logRing keeps the most recent log lines in memory.
//
// It is an io.Writer installed ALONGSIDE stderr, not instead of it: the
// Terminal window keeps working exactly as before, so nothing that already
// relies on it changes.
type logRing struct {
	mu    sync.RWMutex
	lines []string
	// partial holds a write that did not end in a newline, so a line split
	// across two Write calls is not stored as two lines.
	partial string
}

func newLogRing() *logRing {
	return &logRing{lines: make([]string, 0, logRingSize)}
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	text := r.partial + string(p)
	r.partial = ""
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			r.partial = text
			break
		}
		r.append(text[:i])
		text = text[i+1:]
	}
	return len(p), nil
}

// append adds one line, dropping the oldest once full.
func (r *logRing) append(line string) {
	if line == "" {
		return
	}
	if len(r.lines) >= logRingSize {
		copy(r.lines, r.lines[1:])
		r.lines = r.lines[:len(r.lines)-1]
	}
	r.lines = append(r.lines, line)
}

// Lines returns a copy, oldest first, with secrets removed.
func (r *logRing) Lines() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.lines)+1)
	for _, l := range r.lines {
		out = append(out, redactSecrets(l))
	}
	if r.partial != "" {
		out = append(out, redactSecrets(r.partial))
	}
	return out
}

// Text is the whole log as one blob, ready to paste.
func (r *logRing) Text() string {
	return strings.Join(r.Lines(), "\n")
}

// ── redaction ───────────────────────────────────────────────────────────────

// secretPatterns are the credential shapes that can plausibly reach a log line.
//
// Nothing deliberately logs a key, but errors quote what they were given: a
// rejected request can carry its own URL or payload, and that is enough. Since
// this page exists to be copied into a chat window, the shapes are stripped
// wherever they appear rather than trusting every future log call to be careful.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{8,}`),         // Qwen / OpenAI-style
	regexp.MustCompile(`AIza[A-Za-z0-9_\-]{10,}`),       // Google / Gemini
	regexp.MustCompile(`ddi_[A-Za-z0-9_\-]{8,}`),        // platform ingest tokens
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`), // Authorization headers
	// A key passed as a query parameter, which is how it would reach a URL in
	// an error string.
	regexp.MustCompile(`(?i)([?&](?:key|api_?key|token|password)=)[^&\s"']+`),
}

// redactSecrets removes credentials from a line, keeping enough shape that it
// is still obvious WHAT was redacted — "the key is missing" and "the key is
// wrong" are different faults and the line has to still tell them apart.
func redactSecrets(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			// Keep a parameter name (group 1) so ?key=… stays readable.
			if i := strings.IndexAny(m, "=?&"); i >= 0 && strings.ContainsAny(m, "=") {
				if loc := strings.LastIndex(m, "="); loc >= 0 {
					return m[:loc+1] + "[redacted]"
				}
			}
			if len(m) >= 4 {
				return m[:4] + "[redacted]"
			}
			return "[redacted]"
		})
	}
	return s
}

// ── the page ────────────────────────────────────────────────────────────────

// setupDebug renders the log with a copy button.
//
// Localhost-only. The dashboard is deliberately open to the venue network so
// pilots can watch it, but a log is diagnostics rather than spectator content
// and names paths, hosts and settings, so it follows the same rule as changing
// a setting.
func (s *liveServer) setupDebug(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	var lines []string
	if s.logs != nil {
		lines = s.logs.Lines()
	}

	// Newest last, matching the Terminal, so "what just happened" is where the
	// eye already goes.
	body := strings.Join(lines, "\n")
	if strings.TrimSpace(body) == "" {
		body = "(nothing logged yet — leave dd-pits running for a minute and reload)"
	}

	writeSetup(w, r, "Debug", fmt.Sprintf(`
<div class="card">
 <h2>Debug log</h2>
 <p class="mut">The same lines dd-pits prints to its Terminal window, kept here so
 they can be read and sent without hunting through scrollback. The last %d lines.
 API keys are removed automatically, so this is safe to paste to someone.</p>

 <p>
  <button type="button" id="cp"
    style="padding:.45rem .8rem;border-radius:8px;border:0;font-weight:600;
           background:var(--accent,#b6e925);color:#111;cursor:pointer">&#128203; Copy all</button>
  <a href="/setup/debug.txt" download="dd-pits-log.txt"
     style="margin-left:.5rem">download as a file</a>
  <label style="display:inline-flex;align-items:center;gap:.35rem;margin-left:.9rem">
   <input type="checkbox" id="au" checked style="width:auto"> <span class="mut">auto-refresh</span></label>
 </p>

 <pre id="lg" style="max-height:70vh;overflow:auto;background:var(--bg2,#111);
   padding:.7rem;border-radius:8px;font-size:.78rem;line-height:1.45;
   white-space:pre-wrap;word-break:break-word">%s</pre>

 <h3>The audio path</h3>
 %s

 <h3>What it has actually generated</h3>
 <p class="mut" style="font-size:.86rem">Every line the booth produced recently, as a
 playable file. This is the one that settles it: if these play here, commentary is being
 made correctly and the only question left is where the sound is going.</p>
 %s

 <h3>What to look for</h3>
 <ul class="mut" style="font-size:.86rem">
  <li><code>writing commentary&hellip;</code> and <code>voicing commentary&hellip;</code> near the top —
      commentary needs BOTH. One missing is the whole fault.</li>
  <li><code>airing a &hellip; line</code> — a cue actually played. If these appear and you hear
      nothing, the audio is fine and only the routing is wrong: open
      <a href="/booth.wav" target="_blank" rel="noreferrer">/booth.wav</a>.</li>
  <li><code>beat failed</code> — it tried and could not. The reason is on the same line.</li>
  <li><code>audio out:</code> — only appears if the speakers option was on at startup.
      No such line means that setting never took effect.</li>
 </ul>
</div>
<script>
(function(){
 var pre=document.getElementById('lg'), cp=document.getElementById('cp'), au=document.getElementById('au');
 cp.addEventListener('click', function(){
   navigator.clipboard.writeText(pre.textContent).then(function(){
     cp.textContent='✓ Copied'; setTimeout(function(){ cp.innerHTML='&#128203; Copy all'; }, 1500);
   }).catch(function(){
     // Clipboard access can be refused; selecting it is still useful.
     var rg=document.createRange(); rg.selectNodeContents(pre);
     var sel=window.getSelection(); sel.removeAllRanges(); sel.addRange(rg);
     cp.textContent='select and copy';
   });
 });
 setInterval(function(){
   if(!au.checked) return;
   // Only auto-scroll when already at the bottom, so reading back through the
   // log is not yanked away every few seconds.
   var atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 40;
   fetch('/setup/debug.txt').then(function(r){ return r.text(); }).then(function(t){
     if(t && t !== pre.textContent){
       pre.textContent=t;
       if(atBottom) pre.scrollTop=pre.scrollHeight;
     }
   }).catch(function(){});
 }, 4000);
 pre.scrollTop=pre.scrollHeight;
})();
</script>`, logRingSize, template.HTMLEscapeString(body), s.audioPathHTML(), tapeHTML(s.tape)))
}

// setupDebugText is the plain-text form, for the download link, the
// auto-refresh, and anyone who would rather curl it.
func (s *liveServer) setupDebugText(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.logs == nil {
		fmt.Fprintln(w, "(no log buffer)")
		return
	}
	fmt.Fprintf(w, "dd-pits %s (%s) — log captured %s\n\n",
		version, commit, time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintln(w, s.logs.Text())
}

// ── the tape: what the booth actually produced ───────────────────────────────
//
// The debug page can show the log, but a log line saying a cue aired is still
// only a claim. Keeping the audio itself and offering it as a WAV settles the
// question that matters — "is it generating anything?" — on its own, without
// needing the speaker path to work at all.
//
// That is the exact split that cost a venue an afternoon: the commentary was
// being generated perfectly and going nowhere, and nothing on screen could tell
// that apart from not being generated.
//
// In memory, capped, and never persisted: it is a diagnostic view of the last
// few minutes, not an archive.

const (
	boothTapeMaxItems = 24
	// ~24s of 24kHz 16-bit mono is a little over 1MB, so this holds roughly the
	// last twenty calls. Capped by BYTES as well as count because a long
	// meeting-talk line is many times the size of a short lap call.
	boothTapeMaxBytes = 48 << 20
)

// boothTake is one cue that went to air.
type boothTake struct {
	ID   int
	Kind string
	Text string
	At   time.Time
	PCM  []byte
}

// Secs is the clip's length.
func (t *boothTake) Secs() float64 {
	return float64(len(t.PCM)) / float64(boothSampleRate*2)
}

type boothTape struct {
	mu    sync.RWMutex
	items []*boothTake
	next  int
	bytes int
}

func newBoothTape() *boothTape { return &boothTape{} }

// add records a cue. Called on the way to the bus, so what is on the tape is
// what was actually sent — not what was generated and then dropped by a gate.
func (t *boothTape) add(kind, text string, pcm []byte) {
	if t == nil || len(pcm) == 0 {
		return
	}
	// Copied: the cue's buffer belongs to the mixer, which fades and truncates
	// it in place when a call is cut short.
	buf := make([]byte, len(pcm))
	copy(buf, pcm)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	t.items = append(t.items, &boothTake{
		ID: t.next, Kind: kind, Text: text, At: time.Now(), PCM: buf,
	})
	t.bytes += len(buf)
	for len(t.items) > boothTapeMaxItems || (t.bytes > boothTapeMaxBytes && len(t.items) > 1) {
		t.bytes -= len(t.items[0].PCM)
		t.items = t.items[1:]
	}
}

// List returns the takes, newest first — the interesting one is the last thing
// that happened.
func (t *boothTape) List() []*boothTake {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*boothTake, 0, len(t.items))
	for i := len(t.items) - 1; i >= 0; i-- {
		out = append(out, t.items[i])
	}
	return out
}

func (t *boothTape) find(id int) *boothTake {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, it := range t.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

// debugAudio serves one take as a playable WAV.
func (s *liveServer) debugAudio(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/setup/debug/audio/"), ".wav")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	take := s.tape.find(id)
	if take == nil {
		// Expired off the ring rather than never existing — worth saying so,
		// because "not found" on a link that worked a minute ago reads as a bug.
		http.Error(w, "that clip has scrolled off the debug tape (only the last "+
			strconv.Itoa(boothTapeMaxItems)+" are kept)", http.StatusGone)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(wavFromPCM(take.PCM, boothSampleRate))
}

// audioDeviceOptions renders the output-device picker.
//
// Falls back to a TEXT input when the devices cannot be listed: on a machine
// without the audio sidecar's dependencies there is nothing to enumerate, and an
// empty dropdown would remove the operator's ability to set a name at all — the
// setting is still worth saving for a machine where it will work.
func audioDeviceOptions(s *liveServer, current string) string {
	var devs []audioDevice
	var def string
	var err error
	if s != nil && s.audioOutputs != nil {
		devs, def, err = s.audioOutputs()
	} else {
		err = fmt.Errorf("no audio bus")
	}
	if err != nil || len(devs) == 0 {
		return fmt.Sprintf(
			`<input type="text" name="audio_device" value="%s" placeholder="leave blank for the system default">`+
				`<span class="mut" style="font-size:.78rem">Could not list this machine's devices, so type a name.</span>`,
			template.HTMLEscapeString(current))
	}

	var b strings.Builder
	b.WriteString(`<select name="audio_device">`)
	sel := ""
	if current == "" {
		sel = " selected"
	}
	fmt.Fprintf(&b, `<option value=""%s>System default`, sel)
	if def != "" {
		fmt.Fprintf(&b, ` (%s)`, template.HTMLEscapeString(def))
	}
	b.WriteString(`</option>`)

	found := false
	for _, d := range devs {
		sel := ""
		if d.Name == current {
			sel, found = " selected", true
		}
		fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`,
			template.HTMLEscapeString(d.Name), sel, template.HTMLEscapeString(d.Name))
	}
	// A device saved earlier that is not plugged in now must stay selected —
	// silently dropping to the default would mean unplugging a speaker
	// permanently reset the setting.
	if !found && current != "" {
		fmt.Fprintf(&b, `<option value="%s" selected>%s (not connected)</option>`,
			template.HTMLEscapeString(current), template.HTMLEscapeString(current))
	}
	b.WriteString(`</select>`)
	return b.String()
}

// tapeHTML lists the recent takes, each playable and downloadable.
func tapeHTML(t *boothTape) string {
	takes := t.List()
	if len(takes) == 0 {
		return `<p class="mut">Nothing generated yet. The booth has to be switched on, and it
 speaks between and during heats — give it a minute with FPVTrackside running.</p>`
	}
	var b strings.Builder
	b.WriteString(`<table style="width:100%;font-size:.84rem;border-collapse:collapse">`)
	b.WriteString(`<tr style="text-align:left"><th>When</th><th>Beat</th><th>Length</th>` +
		`<th>Listen</th><th>What it said</th></tr>`)
	for _, tk := range takes {
		fmt.Fprintf(&b, `<tr style="border-top:1px solid var(--line)">`+
			`<td style="white-space:nowrap;vertical-align:top">%s</td>`+
			`<td style="white-space:nowrap;vertical-align:top"><code>%s</code></td>`+
			`<td style="white-space:nowrap;vertical-align:top">%.1fs</td>`+
			`<td style="vertical-align:top;white-space:nowrap">`+
			`<audio controls preload="none" src="/setup/debug/audio/%d.wav" style="height:32px;max-width:220px"></audio><br>`+
			`<a href="/setup/debug/audio/%d.wav" download="%s-%d.wav" style="font-size:.78rem">download</a></td>`+
			`<td style="vertical-align:top">%s</td></tr>`,
			tk.At.Format("15:04:05"),
			template.HTMLEscapeString(tk.Kind),
			tk.Secs(), tk.ID, tk.ID,
			template.HTMLEscapeString(tk.Kind), tk.ID,
			template.HTMLEscapeString(tk.Text))
	}
	b.WriteString(`</table>`)
	return b.String()
}

// audioPathHTML shows each link in the chain from "words written" to "sound in
// the room", so a break is visible rather than deduced.
//
// Every one of these was something someone had to be talked through over chat:
// is a writer configured, is a voice configured, is anything actually LISTENING,
// and can this machine use its own speakers. Four questions, one panel.
func (s *liveServer) audioPathHTML() string {
	type step struct {
		label, state, detail string
		ok                   bool
	}
	var steps []step

	// 1. Is there a bus at all?
	bus := s.mixer != nil
	steps = append(steps, step{"Audio bus", boolWord(bus, "running", "not running"),
		"Created when commentary or race calls are switched on.", bus})

	// 2. Is anything listening? THE question — a cue can be produced perfectly
	// and go nowhere, which sounds exactly like nothing being produced.
	n := 0
	if s.mixer != nil {
		n = s.mixer.clientCount()
	}
	listenDetail := "A browser tab on /booth.wav, OBS as a Media Source, or this laptop's speakers."
	if n == 0 && bus {
		listenDetail = "Nothing is listening, so nothing will be heard even when it is working. " +
			`<a href="/booth.wav" target="_blank" rel="noreferrer"><strong>Open /booth.wav</strong></a>.`
	}
	steps = append(steps, step{"Listeners", fmt.Sprintf("%d", n), listenDetail, n > 0})

	// 3. This machine's own speakers.
	playing := s.audioPlaying != nil && s.audioPlaying()
	spkDetail := "Optional — only for monitoring in the room."
	if !playing {
		if ready, why := speakerOutputReady(""); !ready {
			spkDetail = why + " Not needed for OBS or a browser tab."
		} else {
			spkDetail = "Available — tick it on the Commentary page."
		}
	}
	steps = append(steps, step{"This laptop's speakers",
		boolWord(playing, "playing", "off"), spkDetail, playing})

	var b strings.Builder
	b.WriteString(`<table style="width:100%;font-size:.86rem;border-collapse:collapse">`)
	for _, st := range steps {
		mark, colour := "✗", "var(--warn,#c88)"
		if st.ok {
			mark, colour = "✓", "var(--ok,#7c5)"
		}
		fmt.Fprintf(&b, `<tr style="border-top:1px solid var(--line)">`+
			`<td style="width:1.6rem;color:%s;font-weight:700;vertical-align:top">%s</td>`+
			`<td style="white-space:nowrap;vertical-align:top"><strong>%s</strong></td>`+
			`<td style="white-space:nowrap;vertical-align:top">%s</td>`+
			`<td class="mut" style="vertical-align:top">%s</td></tr>`,
			colour, mark,
			template.HTMLEscapeString(st.label),
			template.HTMLEscapeString(st.state),
			st.detail) // detail may carry a deliberate link
	}
	b.WriteString(`</table>`)
	return b.String()
}

func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

package main

// The video page: anchor the stream, get a link on every heat, and a chapter
// list to paste into the description.
//
// The whole feature turns on one number — when the broadcast started — and
// there are two ways to learn it. Ask YouTube (a live stream and its VOD both
// carry the real instant), or find ONE heat in the video and say where it is.
// Everything else is arithmetic against the timing data, which already knows
// when every heat began.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/dronedash"
)

func (s *liveServer) registerVideoPages(mux *http.ServeMux) {
	mux.HandleFunc("/setup/video", s.setupVideo)
	mux.HandleFunc("/setup/video/probe", s.setupVideoProbe)
	mux.HandleFunc("/commentary/", s.serveCommentary)
	mux.HandleFunc("/watch", s.watchPlayer)
	mux.HandleFunc("/setup/models", s.setupModels)
	mux.HandleFunc("/setup/models/status", s.installStatus)
	mux.HandleFunc("/setup/models/test-voice", s.testLocalVoice)
	mux.HandleFunc("/setup/models/test-script", s.testScript)
	mux.HandleFunc("/setup/models/voice-upload", s.uploadVoiceRef)
	mux.HandleFunc("/setup/models/voice-clear", s.clearVoiceRef)
	mux.HandleFunc("/setup/models/load-state", s.writerLoadJSON)
	mux.HandleFunc("/setup/models/speak", s.speakText)
	mux.HandleFunc("/setup/restart", s.restartNow)
	mux.HandleFunc("/setup/audio-test", s.audioTest)
	mux.HandleFunc("/setup/debug", s.setupDebug)
	mux.HandleFunc("/setup/debug.txt", s.setupDebugText)
	mux.HandleFunc("/setup/debug/audio/", s.debugAudio)

	// FPVTrackside's pushed events (ExtensionMode -> NotificationURL).
	//
	// Deliberately NOT under /setup: those paths refuse writes from anything but
	// localhost, and while FPVTrackside is local today, a venue running it on the
	// timing laptop and dd-pits on another machine is an ordinary arrangement.
	// Rejecting its events with a 403 would look exactly like the feature being
	// broken.
	if s.ftEvents != nil {
		mux.Handle("/fpvtrackside/events", s.ftEvents)
	}
}

// finishedRaces returns the meeting's heats that have actually run, newest
// first — the order an operator wants them while a meeting is under way.
func (s *liveServer) finishedRaces() []ddapi.Race {
	mv, _, ok := s.meeting()
	if !ok || mv == nil {
		return nil
	}
	var out []ddapi.Race
	for _, r := range mv.Races {
		if r.StartMs > 0 {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartMs > out[j].StartMs })
	return out
}

func (s *liveServer) setupVideo(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	msg := ""
	v := cfg.VideoConfig()

	mv, _, haveMeeting := s.meeting()
	if haveMeeting && mv != nil && v.MeetingID != "" && v.MeetingID != mv.MeetingID {
		// A different meeting: the anchor belongs to another video and applying
		// it would put every link at the wrong moment of somebody else's
		// stream. Cleared rather than kept, and said out loud.
		v = localVideo{}
		cfg.SetVideo(v)
		_ = cfg.Save()
		msg = "New meeting — the previous stream link was cleared."
	}

	if r.Method == http.MethodPost {
		if !localSetupAllowed(r) {
			setupDenied(w)
			return
		}
		_ = r.ParseForm()
		switch r.FormValue("action") {

		case "anchor":
			// One heat, located in the video, places all of them.
			raceID := r.FormValue("race_id")
			off, ok := parseClock(r.FormValue("at"))
			switch {
			case !ok:
				msg = "Give the time as 1:12:30, 12:30 or a number of seconds."
			default:
				var start int64
				for _, rr := range s.finishedRaces() {
					if rr.ID == raceID {
						start = raceStartUnix(rr)
					}
				}
				if start == 0 {
					msg = "Pick a heat that has already run."
					break
				}
				v.StreamURL = strings.TrimSpace(r.FormValue("stream_url"))
				v.anchorFrom(start, off)
				if haveMeeting && mv != nil {
					v.MeetingID = mv.MeetingID
				}
				cfg.SetVideo(v)
				if err := cfg.Save(); err != nil {
					msg = "Could not save: " + err.Error()
				} else {
					msg = "Anchored — every heat now has a link."
				}
			}

		case "delay":
			n, _ := strconv.Atoi(r.FormValue("commentary_delay"))
			if n < 0 {
				n = 0
			}
			if n > 120 {
				n = 120
			}
			v.CommentaryDelaySecs = n
			cfg.SetVideo(v)
			_ = cfg.Save()
			msg = fmt.Sprintf("Commentary will start %ds into the video.", n)

		case "override":
			cfg.SetRaceVideo(r.FormValue("race_id"), r.FormValue("url"))
			_ = cfg.Save()
			v = cfg.VideoConfig()
			msg = "Saved that heat's link."

		case "commentary":
			if s.generateCommentary == nil {
				msg = "Commentary is not available — set a writer and a voice on the Commentary page."
			} else {
				s.generateCommentary(r.FormValue("race_id"))
				msg = "Writing the commentary — it takes a couple of minutes."
			}

		case "clear":
			cfg.SetVideo(localVideo{})
			_ = cfg.Save()
			v = localVideo{}
			msg = "Cleared."
		}
	}

	races := s.finishedRaces()
	b := &strings.Builder{}
	if msg != "" {
		fmt.Fprintf(b, `<div class="card"><p class="ok">%s</p></div>`, esc(msg))
	}

	if !haveMeeting {
		b.WriteString(`<div class="card"><h2>No meeting yet</h2>
<p class="mut">Open your event in FPVTrackside and this page will fill in.</p></div>`)
		writeSetup(w, "Video", b.String())
		return
	}

	// ── the stream ──────────────────────────────────────────────────────────
	anchored := v.StreamStartedAt > 0 && v.StreamURL != ""
	state := `<span class="warn">not set up</span>`
	if anchored {
		state = `<span class="ok">every heat has a link</span>`
	}
	fmt.Fprintf(b, `<div class="card"><h2>The stream</h2>
<p class="mut">One number makes this work: when the broadcast started. Everything else is worked
out from the timing data, which already knows when each heat began.</p>
<table><tr><th>Right now</th><td>%s</td></tr></table>

<form method="post" style="margin-top:.8rem">
<input type="hidden" name="action" value="anchor">
<label>YouTube address of the stream</label>
<div style="display:flex;gap:.4rem">
 <input type="text" name="stream_url" id="streamURL" value="%s" placeholder="https://www.youtube.com/watch?v=...">
 <button type="button" class="sec" id="probeBtn" style="flex:0 0 auto">🔍 Detect</button>
</div>
<p class="mut" style="font-size:.82rem;margin:.3rem 0 0">Detect asks YouTube when the broadcast
actually began — it works on a live stream and on the video it becomes. A plain upload carries no
start time, so anchor it by hand below.</p>

<div class="row" style="margin-top:.8rem">
 <div><label>…or find one heat in the video</label><select name="race_id">%s</select></div>
 <div><label>and give its time</label><input type="text" name="at" placeholder="1:12:30"></div>
</div>
<p style="margin-top:.7rem"><button>Anchor</button></p>
</form></div>`, state, esc(v.StreamURL), raceOptions(races))

	// ── the delay ───────────────────────────────────────────────────────────
	fmt.Fprintf(b, `<div class="card"><h2>Commentary start delay</h2>
<p class="mut">A broadcast usually carries some pre-race footage before the racing starts, so the
generated commentary is held back this long when played over the video. It shifts the whole track,
so every call stays lined up.</p>
<form method="post"><input type="hidden" name="action" value="delay">
<div class="row"><div><label>Seconds</label>
 <input type="text" name="commentary_delay" value="%d" style="max-width:6rem"></div></div>
<p style="margin-top:.6rem"><button>Save</button></p></form></div>`, v.CommentaryDelaySecs)

	// ── the heats ───────────────────────────────────────────────────────────
	b.WriteString(`<div class="card"><h2>Heats</h2>
<p class="mut">▶ Watch opens the stream at that heat. 🎙 writes commentary for it — a couple of
minutes each, and it is kept, so it only has to be done once.</p>
<table><tr><th>Heat</th><th>In the video</th><th>Commentary</th><th></th></tr>`)
	for _, rr := range races {
		off := v.offsetFor(rr.ID, raceStartUnix(rr))
		at := `<span class="mut">—</span>`
		if off >= 0 {
			at = fmt.Sprintf(`<a href="/watch?race=%s" class="ok">▶ %s</a>`, esc(rr.ID), hhmmss(off))
		}
		fmt.Fprintf(b, `<tr><td>%s</td><td>%s</td><td>%s</td>
<td><form method="post" style="margin:0"><input type="hidden" name="action" value="commentary">
<input type="hidden" name="race_id" value="%s"><button class="sec">🎙</button></form></td></tr>`,
			esc(chapterLabel(rr)), at, s.commentaryState(rr.ID), esc(rr.ID))
	}
	b.WriteString(`</table></div>`)

	// ── chapters ────────────────────────────────────────────────────────────
	if anchored {
		lines := buildChapters(v, races)
		fmt.Fprintf(b, `<div class="card"><h2>YouTube chapters</h2>
<p class="mut">Paste this into the video's description and YouTube turns it into clickable chapters.
The <code>00:00:00</code> line is required — without it YouTube ignores the whole list.</p>
<textarea id="chapters" rows="%d" style="font-family:ui-monospace,monospace;font-size:.85rem">%s</textarea>
<p style="margin-top:.6rem"><button type="button" class="sec" id="copyChapters">Copy</button>
 <span class="mut" id="copiedNote"></span></p></div>`,
			min(len(lines)+1, 20), esc(chaptersText(lines)))
	}

	b.WriteString(videoPageScript)
	writeSetup(w, "Video", b.String())
}

// commentaryState is the per-heat status cell.
func (s *liveServer) commentaryState(raceID string) string {
	if s.commentary == nil {
		return `<span class="mut">—</span>`
	}
	c, ok := s.commentary.get(raceID)
	if !ok {
		return `<span class="mut">not made</span>`
	}
	switch c.Status {
	case "ready":
		return fmt.Sprintf(`<span class="ok">ready — %d lines</span>`, len(c.Segments))
	case "generating":
		return `<span class="mut">writing…</span>`
	default:
		return fmt.Sprintf(`<span class="warn">failed</span> <span class="mut">%s</span>`, esc(c.Error))
	}
}

// raceOptions lists heats for the anchor picker.
func raceOptions(races []ddapi.Race) string {
	b := &strings.Builder{}
	for _, r := range races {
		fmt.Fprintf(b, `<option value="%s">%s</option>`, esc(r.ID), esc(chapterLabel(r)))
	}
	return b.String()
}

// parseClock reads "1:12:30", "12:30" or "4350".
func parseClock(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	parts := strings.Split(s, ":")
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + n
	}
	return total, true
}

// setupVideoProbe asks YouTube when the broadcast began.
func (s *liveServer) setupVideoProbe(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	_ = r.ParseForm()
	url := strings.TrimSpace(r.FormValue("url"))

	start, live, err := probeStreamStart(url, s.youtubeKey)
	if err != nil {
		// 200 with an explanation, never a 5xx: this fails for ordinary reasons
		// (an upload rather than a stream, YouTube bot-walling a datacenter IP)
		// and an error page hides the message that says which.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":false,"error":%q}`, err.Error())
		return
	}

	cfg := s.cfg
	v := cfg.VideoConfig()
	v.StreamURL = url
	v.StreamStartedAt = start
	if mv, _, ok := s.meeting(); ok && mv != nil {
		v.MeetingID = mv.MeetingID
	}
	cfg.SetVideo(v)
	_ = cfg.Save()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"live":%t}`, live)
}

// serveCommentary serves a heat's manifest and its audio clips.
func (s *liveServer) serveCommentary(w http.ResponseWriter, r *http.Request) {
	if s.commentary == nil {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/commentary/")

	// A clip.
	if strings.HasSuffix(rest, ".wav") {
		// Basename only: the id comes from a URL, and a path in it must never
		// be able to reach outside the commentary directory.
		name := rest[strings.LastIndex(rest, "/")+1:]
		http.ServeFile(w, r, s.commentary.dir+"/"+name)
		return
	}

	raceID := strings.TrimSuffix(rest, ".json")
	c, ok := s.commentary.get(raceID)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"none"}`)
		return
	}
	out := struct {
		*localCommentary
		LeadInMs int64 `json:"lead_in_ms"`
	}{c, int64(s.cfg.VideoConfig().CommentaryDelaySecs) * 1000}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// videoPageScript wires Detect and Copy.
const videoPageScript = `
<script>
(function(){
 var probe=document.getElementById('probeBtn');
 if(probe) probe.addEventListener('click', function(){
   var u=document.getElementById('streamURL');
   if(!u||!u.value){ alert('Paste the stream address first.'); return; }
   var was=probe.textContent; probe.textContent='…'; probe.disabled=true;
   var body=new URLSearchParams(); body.set('url', u.value);
   fetch('/setup/video/probe',{method:'POST',body:body})
     .then(function(r){ return r.json(); })
     .then(function(j){
       if(!j.ok){ alert(j.error); return; }
       location.reload();
     })
     .catch(function(e){ alert(String(e)); })
     .finally(function(){ probe.textContent=was; probe.disabled=false; });
 });

 var copy=document.getElementById('copyChapters');
 if(copy) copy.addEventListener('click', function(){
   var t=document.getElementById('chapters');
   if(!t) return;
   t.select();
   navigator.clipboard.writeText(t.value).then(function(){
     document.getElementById('copiedNote').textContent='copied — paste it into the video description';
   });
 });
})();
</script>`

// dronedashUnused keeps the import honest if the link helpers move.
var _ = dronedash.ParseYouTubeID

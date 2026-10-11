package main

// The race director's own controls, for the sounds no amount of polling can
// detect.
//
// Most of FPVTrackside's sounds are REACTIONS — a lap was recorded, a heat
// ended — and dd-pits can see all of those in the data it already polls. A
// handful are not. "Hurry Up" is bound to Ctrl+H, "Announce Race" and the
// video-issue prompts are operator actions, and a keypress inside another
// program is invisible from outside it.
//
// So they get a page here instead. The race director opens it on the venue
// laptop or a phone on the same network and presses the button, and it comes
// out of the same voice and the same bus as everything else. That also makes
// them usable from ANYWHERE at the field, which the keyboard shortcut never
// was.

import (
	"fmt"
	"net/http"
	"strings"
)

// manualSound is a call only a person can decide to make.
type manualSound struct {
	Key    SoundKey
	Label  string
	Detail string

	// NeedsPilot marks a call about a specific pilot; the page offers the
	// field, and "everyone" is what FPVTrackside itself passes for the
	// all-pilots form.
	NeedsPilot bool
}

// manualSounds are the operator-triggered calls, in the order a race director
// is likely to want them.
var manualSounds = []manualSound{
	{Key: SoundHurryUp, Label: "Hurry Up", Detail: "Tell a pilot — or everyone — to get moving.", NeedsPilot: true},
	{Key: SoundRaceAnnounce, Label: "Announce the next race", Detail: "Who is up, and on what."},
	{Key: SoundInTheHole, Label: "In the hole", Detail: "The heat after next."},
	{Key: SoundNameTest, Label: "Say a pilot's name", Detail: "Check a callsign sounds right.", NeedsPilot: true},
}

// serveRaceControl renders the button page.
func (ra *raceAnnouncer) serveRaceControl(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		ra.handleRaceControlPost(w, r)
		return
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8">
` + headBrand + `
<title>Race calls — dd-pits</title>
<style>
 body{font:16px/1.45 -apple-system,BlinkMacSystemFont,"SF Pro Text",Inter,system-ui,sans-serif;-webkit-font-smoothing:antialiased;color:#f2f4f7;margin:0;padding:20px 16px 40px;
  background:radial-gradient(700px 400px at 100% -10%,rgba(198,255,61,.12),transparent 60%),#07080a;min-height:100vh}
 .wrap{max-width:640px;margin:0 auto}
 .brand{display:flex;align-items:center;gap:.55rem;margin-bottom:18px;color:#f2f4f7;text-decoration:none;font-weight:750;letter-spacing:-.02em}
 .brand img{width:28px;height:28px;border-radius:7px;box-shadow:0 0 16px rgba(198,255,61,.3)}
 .brand .dd{background:linear-gradient(90deg,#c6ff3d,#3de0ff);-webkit-background-clip:text;background-clip:text;color:transparent}
 h1{font-size:30px;letter-spacing:-.035em;margin:0 0 4px}
 p.sub{color:#8e96a3;margin:0 0 22px;font-size:15px}
 form{background:linear-gradient(180deg,rgba(255,255,255,.04),rgba(255,255,255,.01)),rgba(22,24,30,.8);border:1px solid rgba(255,255,255,.09);border-radius:20px;padding:16px;margin-bottom:12px;
  box-shadow:0 18px 40px -24px rgba(0,0,0,.9)}
 .lbl{font-weight:700;font-size:17px;margin-bottom:2px;letter-spacing:-.01em}
 .det{color:#8e96a3;font-size:14px;margin-bottom:12px}
 input{background:rgba(0,0,0,.45);border:1px solid rgba(255,255,255,.12);color:#f2f4f7;border-radius:12px;padding:12px;font-size:16px;width:100%;box-sizing:border-box;margin-bottom:10px}
 input:focus{outline:none;border-color:#c6ff3d;box-shadow:0 0 0 3px rgba(198,255,61,.18)}
 button{background:#c6ff3d;border:0;color:#07080a;font-weight:800;border-radius:999px;padding:14px 18px;font-size:16px;width:100%;cursor:pointer;transition:transform .1s}
 button:active{transform:scale(.98)}
 .off{opacity:.45}
 .off button{background:#2a2d33;color:#8e96a3}
 .note{color:#8e96a3;font-size:13px;margin-top:18px;line-height:1.55}
</style>
<div class="wrap">
<a class="brand" href="/"><img src="/assets/icon-256.webp" alt=""><span><span class="dd">dd</span>-pits</span></a>
<h1>Race calls</h1>
<p class="sub">Spoken in the meeting voice, on the same bus as the automatic calls.</p>
`)

	for _, ms := range manualSounds {
		_, enabled := ra.sounds.Template(ms.Key)

		cls := ""
		if !enabled {
			cls = ` class="off"`
		}

		fmt.Fprintf(&b, `<form method="post"%s><div class="lbl">%s</div><div class="det">%s</div>`,
			cls, html(ms.Label), html(ms.Detail))
		fmt.Fprintf(&b, `<input type="hidden" name="key" value="%s">`, html(string(ms.Key)))

		if ms.NeedsPilot {
			b.WriteString(`<input name="pilot" placeholder="pilot (blank = everyone)" autocomplete="off">`)
		}

		if enabled {
			b.WriteString(`<button type="submit">Say it</button>`)
		} else {
			b.WriteString(`<button type="submit" disabled>Switched off in FPVTrackside</button>`)
		}
		b.WriteString(`</form>`)
	}

	// Says WHERE the wording comes from, so nobody goes looking for it here.
	b.WriteString(`<p class="note">The words for each call come from FPVTrackside's own sound
settings — change them there and this page follows. A call switched off there stays
off here too.</p></div>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func (ra *raceAnnouncer) handleRaceControlPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	key := SoundKey(r.FormValue("key"))
	template, ok := ra.sounds.Template(key)
	if !ok {
		// Switched off in FPVTrackside, so it stays silent here — the page is a
		// remote for their settings, not a way around them.
		http.Redirect(w, r, "/calls", http.StatusSeeOther)
		return
	}

	pilot := strings.TrimSpace(r.FormValue("pilot"))
	if pilot == "" {
		// What FPVTrackside itself passes for the all-pilots form.
		pilot = "everyone"
	}

	text := Fill(template, map[string]string{
		"pilot":  pilot,
		"pilots": pilot,
	})

	if text != "" {
		ra.mu.Lock()
		ra.seq++
		seq := ra.seq
		ra.mu.Unlock()

		// A call the director asked for outranks colour, exactly like an
		// automatic one — they pressed the button because it matters now.
		go ra.speak(RaceCall{Key: key, Text: text, Priority: callPriorityRaceState}, seq)
	}

	http.Redirect(w, r, "/calls", http.StatusSeeOther)
}

// html escapes text for the page.
func html(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

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
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Race calls</title>
<style>
 body{font:16px -apple-system,system-ui,sans-serif;background:#111;color:#eee;margin:0;padding:20px;max-width:640px}
 h1{font-size:20px;margin:0 0 4px}
 p.sub{color:#888;margin:0 0 20px;font-size:14px}
 form{background:#1c1c1c;border:1px solid #2a2a2a;border-radius:10px;padding:14px;margin-bottom:12px}
 .lbl{font-weight:600;margin-bottom:2px}
 .det{color:#888;font-size:13px;margin-bottom:10px}
 input{background:#111;border:1px solid #333;color:#eee;border-radius:7px;padding:10px;font-size:16px;width:100%;box-sizing:border-box;margin-bottom:8px}
 button{background:#2d7;border:0;color:#052;font-weight:700;border-radius:7px;padding:12px 18px;font-size:16px;width:100%;cursor:pointer}
 button:active{opacity:.75}
 .off{opacity:.45}
 .off button{background:#333;color:#888}
 .note{color:#777;font-size:13px;margin-top:18px;line-height:1.5}
</style>
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
off here too.</p>`)

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

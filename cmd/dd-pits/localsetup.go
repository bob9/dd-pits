package main

// The pages a venue configures itself with, served by the agent.
//
// Everything here exists so a club with no platform account can run a complete
// race day: goggle pilots, the OSD, the commentary booth, and who speaks the
// race calls. It is the local counterpart of the website's admin pages, and
// deliberately narrower — it configures the MEETING, not a membership system.
//
// SECURITY, because this is the part that is easy to get wrong. The local
// server binds to :8088 so phones in the pits can watch the dashboard, which
// means anyone on the venue wifi can reach it. Watching is harmless; CHANGING
// a pilot's goggle channel mid-heat is not. So every page in this file is
// gated by localSetupAllowed: same-machine only by default, with an explicit
// opt-in for a race director who wants to configure from a phone.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	htmlesc "html"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/elrs"
	"github.com/bob9/dd-pits/internal/qwentts"
	"github.com/bob9/dd-pits/internal/tts"
)

// setupPIN, when set, allows configuring from another machine on the venue
// network. Empty (the default) means same-machine only.
var setupPIN string

// setupBaseURL is where this agent is actually reachable, so every message that
// tells an operator where to go names the port they are really on rather than
// the default. Set once at startup by setSetupBase.
var setupBaseURL = "http://localhost:8088"

// setSetupBase records the serving address as an operator would type it.
func setSetupBase(addr string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	setupBaseURL = "http://" + net.JoinHostPort(host, port)
}

// localSetupAllowed reports whether this request may CHANGE anything.
//
// Loopback is trusted because it means the operator is on the timing laptop —
// the machine that is already running the meeting. Anything else needs the PIN,
// which the operator has to set deliberately.
func localSetupAllowed(r *http.Request) bool {
	if demoMode {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	if setupPIN == "" {
		return false
	}
	// Accepted from a form field or a query param so a phone can be given a
	// bookmarkable URL.
	return r.FormValue("pin") == setupPIN
}

// setupDenied renders the refusal, naming the fix rather than just saying no.
func setupDenied(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	if demoMode {
		fmt.Fprint(w, setupPage("Live demo", `<div class="card"><h2>This is a read-only demo</h2>
<p>You are looking at a real dd-pits, fed by a simulated race meeting. Everything you can see works
exactly as it does at a venue, but nothing here can be changed, saved or installed.</p>
<p>On your own laptop, this page does what it says. <a href="https://github.com/bob9/dd-pits/releases">Get dd-pits</a>
— it is free — and run it next to FPVTrackside.</p>
<p><a href="/"><button>Back to the live dashboard</button></a> <a href="/overlays"><button class="sec">See the OBS overlays</button></a></p></div>`))
		return
	}
	fmt.Fprint(w, setupPage("Not from here", `
<p>Setup can only be changed <strong>from the timing laptop itself</strong>
(<code>`+esc(setupBaseURL)+`/setup</code>).</p>
<p>This page is open to everyone on the venue network so pilots can watch the
dashboard — which is why changing a pilot's goggle channel from it is not
allowed. To configure from a phone, start the agent with
<code>-setup-pin 1234</code> and add <code>?pin=1234</code> to the address.</p>`))
}

// esc is html escaping, short because this file is mostly markup.
func esc(s string) string { return htmlesc.EscapeString(s) }

// setupPage wraps content in the shared shell. Deliberately plain: this is a
// tool used once at the start of a meeting, on a laptop, by someone who wants
// it to be obvious rather than pretty.
func setupPage(title, body string) string {
	banner := ""
	if b, ok := setupBanners[title]; ok {
		banner = `<section class="banner" style="background-image:linear-gradient(90deg,rgba(7,8,10,.92) 15%,rgba(7,8,10,.35) 70%,rgba(7,8,10,.15)),url(/assets/` + b.img + `)">
<div><p class="eyebrow">` + esc(b.eyebrow) + `</p><h1>` + esc(title) + `</h1><p>` + esc(b.line) + `</p></div></section>`
	}
	return `<!doctype html><html><head><meta charset="utf-8">
` + headBrand + `
<title>` + esc(title) + ` — dd-pits setup</title>
<style>
 :root{--bg:#07080a;--fg:#f2f4f7;--mut:#8e96a3;--line:rgba(255,255,255,.09);--card:rgba(22,24,30,.72);--acc:#c6ff3d;--accent:#c6ff3d;--cyan:#3de0ff;--ok:#c6ff3d}
 *{box-sizing:border-box}
 body{margin:0;color:var(--fg);font:15px/1.55 -apple-system,BlinkMacSystemFont,"SF Pro Text","Inter","Segoe UI",sans-serif;-webkit-font-smoothing:antialiased;
  background:radial-gradient(1000px 520px at 90% -10%,rgba(198,255,61,.10),transparent 60%),radial-gradient(800px 480px at -10% 0,rgba(61,224,255,.07),transparent 60%),var(--bg);background-attachment:fixed;min-height:100vh}
 a{color:var(--acc)}
 header{position:sticky;top:0;z-index:10;padding:.65rem 1.2rem;border-bottom:1px solid var(--line);display:flex;gap:1rem;align-items:center;flex-wrap:wrap;
  background:rgba(9,10,13,.66);backdrop-filter:saturate(180%) blur(20px);-webkit-backdrop-filter:saturate(180%) blur(20px)}
 header .brand{display:flex;align-items:center;gap:.6rem;text-decoration:none;color:var(--fg);font-weight:750;font-size:1.05rem;letter-spacing:-.02em}
 header .brand img{width:30px;height:30px;border-radius:8px;box-shadow:0 0 18px rgba(198,255,61,.3)}
 header .brand .dd{background:linear-gradient(90deg,var(--acc),var(--cyan));-webkit-background-clip:text;background-clip:text;color:transparent}
 header .brand small{color:var(--mut);font-weight:600;font-size:.78rem;padding:.12rem .5rem;border:1px solid var(--line);border-radius:999px}
 nav{display:flex;gap:.15rem;flex-wrap:wrap}
 nav a{color:#c3c9d2;text-decoration:none;font-size:.86rem;font-weight:600;padding:.34rem .72rem;border-radius:999px;transition:background .15s,color .15s}
 nav a:hover{background:rgba(255,255,255,.07);color:#fff}
 nav a.on{background:rgba(198,255,61,.14);color:var(--acc)}
 nav a.bmc{color:#ffdd00;background:rgba(255,221,0,.08);border:1px solid rgba(255,221,0,.22)}
 main{max-width:60rem;margin:0 auto;padding:1.4rem 1.2rem 1rem}
 .banner{border-radius:26px;min-height:190px;background-size:cover;background-position:center;display:flex;align-items:flex-end;padding:1.6rem 1.8rem;margin-bottom:1.3rem;
  border:1px solid var(--line);box-shadow:0 30px 60px -30px rgba(0,0,0,.9)}
 .banner h1{margin:.1rem 0 .3rem;font-size:clamp(1.9rem,4.4vw,2.8rem);letter-spacing:-.04em;line-height:1.05}
 .banner p{margin:0;color:#c9ced6;max-width:30rem}
 .banner .eyebrow{color:var(--acc);font-weight:700;font-size:.74rem;letter-spacing:.14em;text-transform:uppercase}
 .card{background:linear-gradient(180deg,rgba(255,255,255,.035),rgba(255,255,255,.01)),var(--card);border:1px solid var(--line);border-radius:22px;padding:1.2rem 1.35rem;margin-bottom:1rem;
  box-shadow:0 18px 50px -24px rgba(0,0,0,.85);backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px)}
 h2{font-size:1.15rem;margin:0 0 .65rem;letter-spacing:-.02em}
 h3{letter-spacing:-.01em}
 p{margin:.45rem 0}
 .mut{color:var(--mut);font-size:.9rem}
 table{width:100%;border-collapse:collapse;font-size:.93rem}
 th,td{text-align:left;padding:.55rem .5rem;border-bottom:1px solid rgba(255,255,255,.06)}
 th{color:var(--mut);font-weight:650;font-size:.72rem;letter-spacing:.08em;text-transform:uppercase}
 input[type=text],input[type=password],input[type=number],select,textarea{background:rgba(0,0,0,.45);color:var(--fg);border:1px solid rgba(255,255,255,.12);border-radius:12px;
  padding:.6rem .75rem;font:inherit;width:100%;transition:border-color .15s,box-shadow .15s}
 input:focus,select:focus,textarea:focus{outline:none;border-color:var(--acc);box-shadow:0 0 0 3px rgba(198,255,61,.18)}
 label{display:block;font-size:.85rem;color:var(--mut);margin:.65rem 0 .25rem}
 button{background:var(--acc);color:#07080a;border:0;border-radius:999px;padding:.6rem 1.15rem;font:inherit;font-weight:700;cursor:pointer;transition:transform .12s,box-shadow .15s}
 button:hover{transform:translateY(-1px);box-shadow:0 8px 24px -8px rgba(198,255,61,.55)}
 button.sec{background:rgba(255,255,255,.08);color:#e6e9ee;border:1px solid var(--line)}
 button.sec:hover{box-shadow:none;background:rgba(255,255,255,.12)}
 .row{display:flex;gap:.9rem;flex-wrap:wrap}
 .row>*{flex:1;min-width:11rem}
 code{background:rgba(0,0,0,.5);border:1px solid var(--line);padding:.1rem .4rem;border-radius:6px;font-size:.86em}
 .ok{color:var(--acc)}.warn{color:#ffb340}
 footer{max-width:60rem;margin:0 auto;padding:.5rem 1.2rem 2.5rem;color:var(--mut);font-size:.86rem}
 footer a{color:#ffdd00}
 @media(max-width:640px){header{padding:.6rem .8rem}.banner{min-height:150px;padding:1.2rem;border-radius:20px}main{padding:1rem .8rem}}
</style></head><body>
<header><a class="brand" href="/"><img src="/assets/icon-256.webp" alt=""><span><span class="dd">dd</span>-pits</span><small>setup</small></a>
<nav>
 <a href="/setup">Overview</a>
 <a href="/setup/pilots">Goggle pilots</a>
 <a href="/setup/osd">OSD screen</a>
 <a href="/setup/video">Video</a>
 <a href="/setup/backpack">Backpack</a>
 <a href="/setup/trackside">FPVTrackside</a>
 <a href="/setup/booth">Commentary</a>
 <a href="/setup/calls">Race calls</a>
 <a href="/goggles">Goggle control</a>
 <a href="/overlays">OBS overlays</a>
 <a href="/setup/models">AI models</a>
 <a href="/setup/debug">Debug</a>
 <a href="/setup/help">Help</a>
 <a class="bmc" href="` + supportURL + `" target="_blank" rel="noopener">☕ Support dd-pits</a>
</nav></header>
<main>` + demoBanner() + banner + body + `</main>
<footer>dd-pits is free and open source, made by <a href="https://github.com/bob9" target="_blank" rel="noopener">bob9</a>. If it makes your race days easier,
<a href="` + supportURL + `" target="_blank" rel="noopener">buy the developer a coffee ☕</a>.</footer>
<script>for(const a of document.querySelectorAll('nav a'))if(a.getAttribute('href')===location.pathname)a.classList.add('on')</script>
</body></html>`
}

// setupBanners gives each setup page its artwork and a one-line summary.
var setupBanners = map[string]struct{ img, eyebrow, line string }{
	"Overview":       {"desk.webp", "Your venue", "What dd-pits is reading, and what this venue is running."},
	"Goggle pilots":  {"goggles.webp", "Goggles", "One bind phrase per pilot: lap times, DVR names and the clock in their goggles."},
	"OSD screen":     {"goggles.webp", "Goggles", "Choose what pilots see on their goggle screen."},
	"Backpack":       {"netpack.webp", "ELRS", "Connect an ELRS netpack on the network, or a USB backpack."},
	"Goggle control": {"director.webp", "Race director", "Change a pilot's video channel or send a message to their goggles."},
	"Commentary":     {"booth.webp", "Commentary booth", "AI play-by-play and colour, in a voice you choose."},
	"Race calls":     {"director.webp", "Race calls", "Decide who speaks the lap times: dd-pits or FPVTrackside."},
	"AI models":      {"offline.webp", "Runs on this machine", "Install local voice and writing models — no internet needed on the day."},
	"Video":          {"stream.webp", "After the race", "Turn your YouTube stream into Watch links and chapters."},
	"Watch":          {"stream.webp", "After the race", "Watch any heat, with commentary over the video."},
	"FPVTrackside":   {"desk.webp", "Timing", "Where FPVTrackside lives and what dd-pits reads from it."},
	"Debug":          {"desk.webp", "Debug", "The log, in the browser — send it when something goes wrong."},
	"Help":           {"hero.webp", "Getting started", "Everything a race director needs, in five minutes."},
	"OBS overlays":   {"stream.webp", "On stream", "Broadcast graphics that follow the meeting on their own. Paste a URL into OBS."},
}

// reloadELRSConfig re-reads the goggle config after an edit, when ELRS is on.
func (s *liveServer) reloadELRSConfig() {
	if s.reloadELRS != nil {
		s.reloadELRS()
	}
}

// setGoggleChannel changes a pilot's video channel from the timing desk.
func (s *liveServer) setGoggleChannel(callsign, channel string) error {
	if s.setChannel == nil {
		return fmt.Errorf("the ELRS backpack is not enabled — start dd-pits with -elrs")
	}
	if strings.TrimSpace(callsign) == "" || strings.TrimSpace(channel) == "" {
		return fmt.Errorf("pick a pilot and a channel")
	}
	return s.setChannel(callsign, channel)
}

// registerLocalSetup mounts the pages. Called only when the agent is serving.
func (s *liveServer) registerLocalSetup(mux *http.ServeMux) {
	if s.cfg == nil {
		return
	}
	mux.HandleFunc("/setup", s.setupOverview)
	mux.HandleFunc("/setup/help", s.setupHelp)
	mux.HandleFunc("/setup/pilots", s.setupPilots)
	mux.HandleFunc("/setup/osd", s.setupOSD)
	mux.HandleFunc("/setup/backpack", s.setupBackpack)
	mux.HandleFunc("/setup/trackside", s.setupTrackside)
	mux.HandleFunc("/setup/pick-folder", s.setupPickFolder)
	mux.HandleFunc("/setup/booth", s.setupBooth)
	mux.HandleFunc("/setup/voice-test", s.setupVoiceTest)
	mux.HandleFunc("/setup/calls", s.setupCalls)
	mux.HandleFunc("/setup/calls/ft-status", func(w http.ResponseWriter, r *http.Request) {
		cc := s.cfg.CallsConfig()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"needed":%v,"done":%v,"running":%v}`,
			cc.FTRestartNeeded, cc.FTRestartDone, ftRunning())
	})
	mux.HandleFunc("/goggles", s.setupGoggles)
}

func (s *liveServer) setupOverview(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	pilots := cfg.PilotList()
	ready := 0
	for _, p := range pilots {
		if p.resolvedUID() != "" && p.OSDEnabled {
			ready++
		}
	}
	booth := cfg.BoothConfig()
	source := callsSource(cfg.CallsConfig(), s.hasVoice)

	engine := "off"
	switch booth.Engine {
	case "local":
		engine = "local model (this machine)"
	case "qwen":
		engine = "Qwen3-TTS (over the internet)"
	}

	b := &strings.Builder{}
	fmt.Fprintf(b, `<div class="card"><h2>What this venue is running</h2>
<table>
<tr><th>Timing</th><td>FPVTrackside at <code>%s</code></td></tr>
<tr><th>Goggle pilots</th><td>%d configured, %d with the OSD on — <a href="/setup/pilots">edit</a></td></tr>
<tr><th>Race calls</th><td>%s</td></tr>
<tr><th>Commentary</th><td>%s — <a href="/setup/booth">edit</a></td></tr>
<tr><th>Settings file</th><td><code>%s</code></td></tr>
</table></div>`,
		esc(s.tracksideURL), len(pilots), ready,
		esc(describeCallsSource(source, s.hasVoice)), esc(engine), esc(cfg.Path()))

	b.WriteString(`<div class="card"><h2>Race day</h2>
<p>These all work with no internet:</p>
<p class="mut">
<a href="/">Dashboard</a> ·
<a href="/dronedash/races">Race history</a> ·
<a href="/live">Live view</a> ·
<a href="/overlay">OBS overlay</a> ·
<a href="/calls">Manual calls</a> ·
<a href="/booth">Commentary status</a> ·
<a href="/docs">JSON API</a>
</p></div>`)

	if ready == 0 {
		b.WriteString(`<div class="card"><h2>Start here</h2>
<p>No goggle pilots are set up yet. Adding a pilot's <strong>bind phrase</strong> turns on
their lap OSD, DVR file naming and goggle clock sync — the phrase is all that is needed,
the goggle ID is worked out from it.</p>
<p><a href="/setup/pilots"><button>Add a pilot</button></a></p></div>`)
	}
	writeSetup(w, "Overview", b.String())
}

func writeSetup(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, setupPage(title, body))
}

// ── goggle pilots ────────────────────────────────────────────────────────────

func (s *liveServer) setupPilots(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	msg := ""

	if r.Method == http.MethodPost {
		if !localSetupAllowed(r) {
			setupDenied(w)
			return
		}
		_ = r.ParseForm()
		switch r.FormValue("action") {
		case "remove":
			cfg.RemovePilot(r.FormValue("callsign"))
			msg = "Removed " + r.FormValue("callsign")
		default:
			call := strings.TrimSpace(r.FormValue("callsign"))
			if call == "" {
				msg = "A callsign is required — it is how a pilot in FPVTrackside is matched to their goggles."
				break
			}
			p := localPilot{
				Callsign:   call,
				BindPhrase: strings.TrimSpace(r.FormValue("bind_phrase")),
				UID:        strings.TrimSpace(r.FormValue("uid")),
				OSDEnabled: r.FormValue("osd_enabled") == "1",
			}
			if p.resolvedUID() == "" {
				msg = "Give " + call + " a bind phrase (or a 12-character goggle ID) — without one there is no way to address their goggles."
				break
			}
			cfg.SetPilot(p)
			msg = "Saved " + call
		}
		if err := cfg.Save(); err != nil {
			msg = "Could not save: " + err.Error()
		}
		s.reloadELRSConfig()
	}

	b := &strings.Builder{}
	if msg != "" {
		fmt.Fprintf(b, `<div class="card"><p class="ok">%s</p></div>`, esc(msg))
	}

	b.WriteString(`<div class="card"><h2>Goggle pilots</h2>
<p class="mut">The callsign must match the one in FPVTrackside. The bind phrase is the one
the pilot typed into their goggles — the goggle ID is derived from it exactly as the
ExpressLRS firmware does, so there is nothing to look up.</p>
<table><tr><th>Callsign</th><th>Bind phrase</th><th>Goggle ID</th><th>OSD</th><th></th></tr>`)

	for _, p := range cfg.PilotList() {
		uid := p.resolvedUID()
		state := `<span class="warn">off</span>`
		if p.OSDEnabled {
			state = `<span class="ok">on</span>`
		}
		phrase := p.BindPhrase
		if phrase == "" {
			phrase = `<span class="mut">(ID set directly)</span>`
		} else {
			phrase = esc(phrase)
		}
		fmt.Fprintf(b, `<tr><td><strong>%s</strong></td><td>%s</td><td><code>%s</code></td><td>%s</td>
<td><form method="post" style="margin:0"><input type="hidden" name="action" value="remove">
<input type="hidden" name="callsign" value="%s"><button class="sec">Remove</button></form></td></tr>`,
			esc(p.Callsign), phrase, esc(uid), state, esc(p.Callsign))
	}
	b.WriteString(`</table></div>`)

	b.WriteString(`<div class="card"><h2>Add or update a pilot</h2>
<form method="post">
<div class="row">
 <div><label>Callsign (as in FPVTrackside)</label><input type="text" name="callsign" required></div>
 <div><label>Bind phrase</label><input type="text" name="bind_phrase" placeholder="e.g. bob9 rebels"></div>
</div>
<label>Or the goggle ID directly (12 hex characters — only if you know it)</label>
<input type="text" name="uid" placeholder="optional">
<label><input type="checkbox" name="osd_enabled" value="1" checked style="width:auto"> Send OSD messages to this pilot</label>
<p style="margin-top:.8rem"><button>Save pilot</button></p>
</form></div>`)
	writeSetup(w, "Goggle pilots", b.String())
}

// ── OSD screen ───────────────────────────────────────────────────────────────

func (s *liveServer) setupOSD(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	msg := ""

	// Which screen is being edited: a pilot's own, or the shared default that
	// every pilot without one falls back to.
	who := strings.TrimSpace(r.FormValue("pilot"))

	if r.Method == http.MethodPost {
		if !localSetupAllowed(r) {
			setupDenied(w)
			return
		}
		_ = r.ParseForm()

		// "Use the shared screen" clears a pilot's own layout rather than
		// storing a copy of the default — otherwise a later change to the
		// default would silently skip everyone who had ever opened their page.
		if who != "" && r.FormValue("use_default") == "1" {
			cfg.mu.Lock()
			for i := range cfg.Pilots {
				if strings.EqualFold(cfg.Pilots[i].Callsign, who) {
					cfg.Pilots[i].Layout = nil
				}
			}
			cfg.mu.Unlock()
			if err := cfg.Save(); err != nil {
				msg = "Could not save: " + err.Error()
			} else {
				msg = who + " now uses the shared screen."
			}
			s.reloadELRSConfig()
		} else {
			layout := elrs.Layout{Version: elrs.LayoutVersion, Elements: map[string]elrs.ElementPos{}}
			for _, m := range elrs.ElementMetas {
				row, _ := strconv.Atoi(r.FormValue("row_" + m.Slug))
				col, _ := strconv.Atoi(r.FormValue("col_" + m.Slug))
				hold, _ := strconv.Atoi(r.FormValue("hold_" + m.Slug))
				laps, _ := strconv.Atoi(r.FormValue("laps_" + m.Slug))
				layout.Elements[m.Slug] = elrs.ElementPos{
					Enabled:  r.FormValue("on_"+m.Slug) == "1",
					Row:      row,
					Col:      col,
					Center:   r.FormValue("center_"+m.Slug) == "1",
					HoldSecs: hold,
					NumLaps:  laps,
				}
			}
			clean, verr := elrs.ValidateLayout(layout)
			if verr != nil {
				msg = "That layout is not valid: " + verr.Error()
			} else {
				cfg.mu.Lock()
				if who == "" {
					cfg.DefaultLayout = &clean
				} else {
					for i := range cfg.Pilots {
						if strings.EqualFold(cfg.Pilots[i].Callsign, who) {
							l := clean
							cfg.Pilots[i].Layout = &l
						}
					}
				}
				cfg.mu.Unlock()
				if err := cfg.Save(); err != nil {
					msg = "Could not save: " + err.Error()
				} else {
					msg = "Saved — goggles get the new screen on the next heat."
				}
				s.reloadELRSConfig()
			}
		}
	}

	// The layout being shown: the pilot's own, else the shared default.
	cfg.mu.RLock()
	cur := cfg.DefaultLayout
	own := false
	if who != "" {
		for _, p := range cfg.Pilots {
			if strings.EqualFold(p.Callsign, who) && p.Layout != nil {
				cur = p.Layout
				own = true
			}
		}
	}
	cfg.mu.RUnlock()
	if cur == nil {
		def := elrs.DefaultLayout()
		cur = &def
	}

	b := &strings.Builder{}
	if msg != "" {
		fmt.Fprintf(b, `<div class="card"><p class="ok">%s</p></div>`, esc(msg))
	}

	// Whose screen ─────────────────────────────────────────────────────────
	b.WriteString(`<div class="card"><h2>Whose screen</h2><p class="mut">`)
	if who == "" {
		b.WriteString(`<strong>Shared screen</strong> — used by every pilot who has not been given their own.`)
	} else if own {
		fmt.Fprintf(b, `<strong>%s</strong> has their own screen.`, esc(who))
	} else {
		fmt.Fprintf(b, `<strong>%s</strong> is using the shared screen. Saving here gives them their own.`, esc(who))
	}
	b.WriteString(`</p><p>`)
	sharedOn := ""
	if who == "" {
		sharedOn = ` class="on"`
	}
	fmt.Fprintf(b, `<a href="/setup/osd"%s>Shared screen</a> `, sharedOn)
	for _, p := range cfg.PilotList() {
		if p.resolvedUID() == "" {
			continue
		}
		mark := ""
		if p.Layout != nil {
			mark = " ✎"
		}
		on := ""
		if strings.EqualFold(p.Callsign, who) {
			on = ` class="on"`
		}
		fmt.Fprintf(b, `· <a href="/setup/osd?pilot=%s"%s>%s%s</a> `,
			esc(p.Callsign), on, esc(p.Callsign), mark)
	}
	b.WriteString(`</p><p class="mut">✎ marks a pilot with their own screen.</p></div>`)

	// The editor ───────────────────────────────────────────────────────────
	fmt.Fprintf(b, `<form method="post" id="osdForm">
<input type="hidden" name="pilot" value="%s">
<div class="card"><h2>Goggle screen</h2>
<p class="mut">Drag an element to move it. The grid is the real goggle screen — %d columns by %d rows.
Centred elements only move up and down, because the goggles centre them horizontally themselves.</p>
<p style="margin-top:.7rem">
 <button>Save screen</button>`, esc(who), elrs.OSDCols, elrs.OSDRows)
	if who != "" {
		b.WriteString(` <button name="use_default" value="1" class="sec">Use the shared screen instead</button>`)
	}
	b.WriteString(`</p></div>`)

	// Per-element controls. Kept as a table as well as the drag surface: it is
	// the only way to set a hold time, and it is what still works if the drag
	// interface fails or the operator prefers typing.
	b.WriteString(`<div class="card"><h2>Elements</h2>
<table><tr><th>Show</th><th>Element</th><th>Row</th><th>Col</th><th>Centre</th><th>Hold (s)</th><th>Laps</th></tr>`)
	for _, m := range elrs.ElementMetas {
		el := cur.Elements[m.Slug]
		chk, cen := "", ""
		if el.Enabled {
			chk = " checked"
		}
		if el.Center {
			cen = " checked"
		}
		laps := ""
		if m.Slug == elrs.ElemRecentLaps {
			laps = fmt.Sprintf(`<input type="text" name="laps_%s" value="%d" style="width:3.5rem" data-osd="laps" data-slug="%s">`,
				m.Slug, el.NumLaps, m.Slug)
		}
		fmt.Fprintf(b, `<tr data-slug="%s">
<td><input type="checkbox" name="on_%s" value="1"%s data-osd="on" data-slug="%s"></td>
<td>%s<div class="mut"><code>%s</code></div></td>
<td><input type="text" name="row_%s" value="%d" style="width:3.5rem" data-osd="row" data-slug="%s"></td>
<td><input type="text" name="col_%s" value="%d" style="width:3.5rem" data-osd="col" data-slug="%s"></td>
<td><input type="checkbox" name="center_%s" value="1"%s data-osd="center" data-slug="%s"></td>
<td><input type="text" name="hold_%s" value="%d" style="width:3.5rem"></td>
<td>%s</td></tr>`,
			m.Slug,
			m.Slug, chk, m.Slug,
			esc(m.Label), esc(m.Preview),
			m.Slug, el.Row, m.Slug,
			m.Slug, el.Col, m.Slug,
			m.Slug, cen, m.Slug,
			m.Slug, el.HoldSecs,
			laps)
	}
	b.WriteString(`</table></div></form>`)

	// The drag surface is built from the same data the table holds, and writes
	// back into those inputs — so the form is still the single source of truth
	// and a save works identically whether the operator dragged or typed.
	b.WriteString(osdEditorScript(cur))
	writeSetup(w, "OSD screen", b.String())
}

// osdEditorScript renders the goggle screen the operator drags elements around
// on.
//
// Visually it is the platform's editor: a 25:9 stage (which is exactly the
// goggles' 50x18 character grid), phosphor-green monospace on a dark radial
// background, scanlines and an optional dot grid. That is not decoration — an
// operator positioning a lap counter needs to see what the pilot will see, and
// a preview that looks like a form does not tell them whether two elements
// collide.
//
// It is driven BY the form inputs rather than a parallel model: dragging writes
// row/col back into the same fields the table shows, so there is one source of
// truth and no way for the picture and the saved layout to disagree. With the
// script disabled the table alone is still a complete editor.
func osdEditorScript(cur *elrs.Layout) string {
	// Tagged lowercase because the script reads m.slug / m.preview. Without
	// the tags Go marshals "Slug"/"Preview", every lookup in the browser is
	// undefined, and the stage renders empty with no error anywhere — which is
	// exactly what happened.
	type item struct {
		Slug    string `json:"slug"`
		Label   string `json:"label"`
		Preview string `json:"preview"`
		Rows    int    `json:"rows"`
		MaxW    int    `json:"maxW"`
	}
	items := []item{}
	for _, m := range elrs.ElementMetas {
		items = append(items, item{Slug: m.Slug, Label: m.Label, Preview: m.Preview, Rows: m.Rows, MaxW: m.MaxW})
	}
	meta, _ := json.Marshal(items)

	return fmt.Sprintf(`
<style>
 .osd-stage{position:relative;width:100%%;aspect-ratio:%d/%d;
   background:radial-gradient(ellipse at center,#101710 0%%,#060a06 85%%);
   border-radius:14px;border:3px solid #1e293b;overflow:hidden;touch-action:none;user-select:none;
   box-shadow:inset 0 0 60px rgba(0,0,0,.8),0 6px 24px rgba(0,0,0,.35)}
 .osd-scanlines{position:absolute;inset:0;pointer-events:none;opacity:.18;
   background:repeating-linear-gradient(0deg,transparent 0 2px,rgba(255,255,255,.08) 2px 3px)}
 .osd-grid{position:absolute;inset:0;pointer-events:none;opacity:.25;
   background-image:radial-gradient(circle,#3f6f3f 1px,transparent 1px)}
 .osd-el{position:absolute;font-family:"Courier New",ui-monospace,monospace;font-weight:700;
   color:#e7f5e7;text-shadow:0 0 4px rgba(120,255,120,.35),1px 1px 0 #000;
   white-space:pre;cursor:grab;line-height:1;border-radius:3px;display:flex;align-items:center}
 .osd-el.dragging{cursor:grabbing;background:rgba(37,99,235,.25);outline:1px dashed #60a5fa}
 .osd-el.overlap{outline:1px solid #f87171;background:rgba(248,113,113,.18)}
 .osd-el.disabled{display:none}
 .osd-badge{position:absolute;background:#0f172a;color:#fff;font-size:.72rem;font-weight:700;
   padding:.15rem .45rem;border-radius:6px;pointer-events:none;z-index:10;transform:translate(-50%%,-140%%)}
 .osd-hint{color:var(--mut);font-size:.82rem;margin:.5rem 0 0;display:flex;gap:.9rem;align-items:center;flex-wrap:wrap}
 .osd-hint label{display:inline-flex;align-items:center;gap:.3rem;margin:0;cursor:pointer}
 .osd-hint input{width:auto}
</style>
<div class="osd-stage" id="osdStage">
  <div class="osd-grid" id="osdGrid" hidden></div>
  <div class="osd-scanlines"></div>
</div>
<div class="osd-hint">
  <span>Drag an element to move it. A centred element only moves up and down.</span>
  <label><input type="checkbox" id="osdGridToggle"> show grid</label>
  <span id="osdReadout" class="mut"></span>
</div>
<script>
(function(){
 var META = %s, COLS = %d, ROWS = %d;
 var stage = document.getElementById('osdStage');
 if(!stage) return;
 var grid = document.getElementById('osdGrid');
 var readout = document.getElementById('osdReadout');

 function inp(kind,slug){ return document.querySelector('[data-osd="'+kind+'"][data-slug="'+slug+'"]'); }
 function num(kind,slug){ var e=inp(kind,slug); return e?(parseInt(e.value,10)||0):0; }
 function on(slug){ var e=inp('on',slug); return !!(e&&e.checked); }
 function centred(slug){ var e=inp('center',slug); return !!(e&&e.checked); }
 function cell(){ var r=stage.getBoundingClientRect(); return {w:r.width/COLS, h:r.height/ROWS, r:r}; }

 var nodes={}, badge=null;
 META.forEach(function(m){
   var d=document.createElement('div');
   d.className='osd-el'; d.dataset.slug=m.slug;
   d.textContent=m.preview;
   stage.appendChild(d); nodes[m.slug]=d;
   d.addEventListener('pointerdown', function(ev){ startDrag(ev,m); });
 });

 function place(){
   var cs=cell();
   // Character-cell sizing, so the text on screen is the width it will really
   // occupy on the goggles rather than whatever the browser font gives.
   var fs=Math.max(7, Math.min(cs.h*0.92, cs.w*1.7));
   grid.style.backgroundSize = cs.w+'px '+cs.h+'px';

   var boxes=[];
   META.forEach(function(m){
     var d=nodes[m.slug];
     if(!on(m.slug)){ d.classList.add('disabled'); return; }
     d.classList.remove('disabled');
     var row=num('row',m.slug), col=num('col',m.slug);
     var w=(m.preview.length)*cs.w, h=cs.h*(m.rows||1);
     d.style.fontSize=fs+'px';
     d.style.height=h+'px';
     d.style.top=(row*cs.h)+'px';
     if(centred(m.slug)){ d.style.left='50%%'; d.style.transform='translateX(-50%%)';
                          col=Math.max(0,Math.round((COLS-m.preview.length)/2)); }
     else { d.style.left=(col*cs.w)+'px'; d.style.transform='none'; }
     boxes.push({slug:m.slug,row:row,rows:(m.rows||1),c0:col,c1:col+m.preview.length});
   });

   // Overlap is worth SHOWING rather than preventing: two elements on one row
   // is legal and sometimes deliberate, but it is almost always a mistake and
   // invisible until a pilot is in the air.
   boxes.forEach(function(a){ nodes[a.slug].classList.remove('overlap'); });
   for(var i=0;i<boxes.length;i++) for(var j=i+1;j<boxes.length;j++){
     var a=boxes[i],b=boxes[j];
     var rowsHit = !(a.row+a.rows<=b.row || b.row+b.rows<=a.row);
     var colsHit = !(a.c1<=b.c0 || b.c1<=a.c0);
     if(rowsHit&&colsHit){ nodes[a.slug].classList.add('overlap'); nodes[b.slug].classList.add('overlap'); }
   }
 }

 var drag=null;
 function startDrag(ev,m){
   if(!on(m.slug)) return;
   ev.preventDefault();
   var d=nodes[m.slug], cs=cell(), b=d.getBoundingClientRect();
   drag={m:m,d:d,cs:cs,dx:ev.clientX-b.left,dy:ev.clientY-b.top};
   d.classList.add('dragging'); d.setPointerCapture(ev.pointerId);
   badge=document.createElement('div'); badge.className='osd-badge'; stage.appendChild(badge);
   showBadge(m);
 }
 function showBadge(m){
   if(!badge) return;
   var cs=cell(), row=num('row',m.slug), col=num('col',m.slug);
   badge.textContent = centred(m.slug) ? ('row '+row+' · centred') : ('row '+row+' · col '+col);
   badge.style.left=((centred(m.slug)? COLS/2 : col)*cs.w)+'px';
   badge.style.top=(row*cs.h)+'px';
   if(readout) readout.textContent=m.label+': '+badge.textContent;
 }
 stage.addEventListener('pointermove', function(ev){
   if(!drag) return;
   var m=drag.m, cs=cell();
   var row=Math.round((ev.clientY-cs.r.top-drag.dy)/cs.h);
   inp('row',m.slug).value=Math.max(0,Math.min(ROWS-(m.rows||1),row));
   if(!centred(m.slug)){
     var col=Math.round((ev.clientX-cs.r.left-drag.dx)/cs.w);
     inp('col',m.slug).value=Math.max(0,Math.min(Math.max(0,COLS-m.preview.length),col));
   }
   place(); showBadge(m);
 });
 function endDrag(){
   if(!drag) return;
   drag.d.classList.remove('dragging'); drag=null;
   if(badge){ badge.remove(); badge=null; }
 }
 stage.addEventListener('pointerup', endDrag);
 stage.addEventListener('pointercancel', endDrag);

 var gt=document.getElementById('osdGridToggle');
 if(gt) gt.addEventListener('change', function(){ grid.hidden=!gt.checked; });

 document.querySelectorAll('[data-osd]').forEach(function(e){
   e.addEventListener('input',place); e.addEventListener('change',place);
 });
 window.addEventListener('resize',place);
 place();
})();
</script>`, elrs.OSDCols, elrs.OSDRows, string(meta), elrs.OSDCols, elrs.OSDRows)
}

// beatToggles renders the phase and per-beat switches.
//
// Grouped by PHASE rather than listed flat, because that is how a race director
// thinks about it: "no chatter while they are racing" is one decision, not
// seven. The per-beat boxes underneath are for the finer cuts.
func beatToggles(bc localBooth) string {
	phases := []struct {
		key, label, note string
		on               bool
	}{
		{"phase_pre", "Before the race", "welcome, the lineup, between-heats chatter", phaseOn(bc.PhasePre)},
		{"phase_in", "During the race", "the launch, mid-heat calls, pilots down and back", phaseOn(bc.PhaseIn)},
		{"phase_post", "After the race", "the result and what it does to the standings", phaseOn(bc.PhasePost)},
	}
	labels := map[string]string{
		"meeting_open": "Welcome to the meeting",
		"staged":       "Heat on the grid — the line-up",
		"filler":       "Between-heats chatter",
		"launch":       "The launch",
		"holeshot":     "Holeshot — first to gate one",
		"progress":     "Mid-heat lap calls",
		"meeting_talk": "Standings talk during a heat",
		"pilot_out":    "\"Looks like we've lost one\"",
		"pilot_back":   "\"Back in action\"",
		"qual_move":    "Climbed the qualifying ladder",
		"finish":       "The result wrap",
	}

	b := &strings.Builder{}
	for _, ph := range phases {
		chk := ""
		if ph.on {
			chk = " checked"
		}
		fmt.Fprintf(b, `<div style="margin:.7rem 0 .3rem">
<label><input type="checkbox" name="%s" value="1"%s style="width:auto">
 <strong>%s</strong> <span class="mut">— %s</span></label></div>
<div style="margin-left:1.4rem">`, ph.key, chk, esc(ph.label), esc(ph.note))

		for _, kind := range boothpkg.BeatKinds {
			if boothpkg.BeatPhase(kind) != strings.TrimPrefix(ph.key, "phase_") {
				continue
			}
			bchk := ""
			if bc.beatOn(kind) {
				bchk = " checked"
			}
			name := labels[kind]
			if name == "" {
				name = kind
			}
			fmt.Fprintf(b, `<label style="margin:.15rem 0"><input type="checkbox" name="beat_%s" value="1"%s style="width:auto"> %s</label>`,
				kind, bchk, esc(name))
		}
		b.WriteString(`</div>`)
	}
	return b.String()
}

// describeCommentary says whether the booth is set up, in one line, on the page
// where someone is deciding who speaks.
//
// The two settings are independent and that is not obvious: an operator reading
// "FPVTrackside speaks the calls" reasonably concludes dd-pits is silent, and
// never finds the combination that is probably what they want.
func describeCommentary(b localBooth) string {
	switch strings.ToLower(b.Engine) {
	case "qwen":
		if strings.TrimSpace(b.QwenKey) == "" {
			return `commentary is set to Qwen but has <strong>no key yet</strong> — ` +
				`<a href="/setup/booth">add one</a>.`
		}
		return `commentary is <strong>on</strong>, spoken by Qwen over the internet.`
	case "local":
		return `commentary is <strong>on</strong>, spoken by the local model on this machine.`
	default:
		return `commentary is <strong>off</strong> — <a href="/setup/booth">switch it on</a> ` +
			`if you want colour around the calls.`
	}
}

// ── where FPVTrackside lives ─────────────────────────────────────────────────

func (s *liveServer) setupTrackside(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	msg := ""

	if r.Method == http.MethodPost {
		if !localSetupAllowed(r) {
			setupDenied(w)
			return
		}
		_ = r.ParseForm()
		cc := cfg.CallsConfig()
		cc.TracksideDir = strings.TrimSpace(r.FormValue("dir"))
		cc.Profile = strings.TrimSpace(r.FormValue("profile"))

		// Checked BEFORE saving: a path that holds no Sounds.xml produces no
		// race calls and no error, so accepting it silently would hand the
		// operator the exact failure this page exists to prevent.
		switch {
		case cc.TracksideDir != "" && inspectTracksideDir(cc.TracksideDir) == nil:
			msg = "Nothing usable in " + cc.TracksideDir +
				" — it needs a data folder with a profile in it. Pick one of the folders found below."
		default:
			cfg.SetCalls(cc)
			if err := cfg.Save(); err != nil {
				msg = "Could not save: " + err.Error()
			} else {
				msg = "Saved — restart dd-pits to read from there."
			}
		}
	}

	cc := cfg.CallsConfig()
	found := findTracksideInstalls()

	// What is in use right now, and whether it is really there.
	sp, lp := cc.soundsPath(), cc.logPath()
	if sp == "" {
		sp = DefaultSoundsPath(cc.Profile)
	}
	if lp == "" {
		lp = DefaultSpeechLogPath()
	}

	b := &strings.Builder{}
	if msg != "" {
		fmt.Fprintf(b, `<div class="card"><p class="ok">%s</p></div>`, esc(msg))
	}

	fmt.Fprintf(b, `<div class="card"><h2>What dd-pits is reading</h2>
<p class="mut">Two files, and it needs both. <strong>Sounds.xml</strong> carries the wording of every
call, so your own phrasing is used rather than something invented here. The <strong>log</strong> is how
dd-pits hears the calls it cannot work out from the timing data — "Hurry Up" is a keypress, and
invisible from outside FPVTrackside.</p>
<table>
<tr><th>Wording</th><td><code>%s</code> %s</td></tr>
<tr><th>Log</th><td><code>%s</code> %s</td></tr>
</table></div>`, esc(sp), fileState(sp), esc(lp), fileState(lp))

	if len(found) == 0 {
		b.WriteString(`<div class="card"><h2>No installation found</h2>
<p>dd-pits looked in the usual places and did not find FPVTrackside. Start it once so it creates
its folders, or type where it lives below.</p></div>`)
	} else {
		b.WriteString(`<div class="card"><h2>Found on this machine</h2>
<p class="mut">Pick the one you race with. A club often has more than one profile, and the wording
differs between them — the wrong profile gives you calls in someone else's phrasing rather than
an error.</p><form method="post">`)
		for i, in := range found {
			for _, prof := range in.Profiles {
				id := fmt.Sprintf("tsd%d_%s", i, prof)
				checked := ""
				if strings.EqualFold(in.Dir, cc.TracksideDir) && strings.EqualFold(prof, cc.Profile) {
					checked = " checked"
				} else if cc.TracksideDir == "" && strings.EqualFold(in.Dir, defaultTracksideDir()) &&
					(cc.Profile == "" && prof == "Profile 1" || strings.EqualFold(prof, cc.Profile)) {
					checked = " checked" // what it is using by default
				}
				logNote := `<span class="warn"> — no log found, so relayed calls will not work</span>`
				if in.HasLog {
					logNote = ""
				}
				fmt.Fprintf(b, `<p><label><input type="radio" name="pick" value="%s" id="%s"%s style="width:auto"
 onchange="document.getElementsByName('dir')[0].value=this.dataset.dir;document.getElementsByName('profile')[0].value=this.dataset.prof;"
 data-dir="%s" data-prof="%s">
<strong>%s</strong> <span class="mut">— %s</span>%s</label></p>`,
					esc(in.Dir+"|"+prof), esc(id), checked,
					esc(in.Dir), esc(prof), esc(prof), esc(in.Dir), logNote)
			}
		}
		pick := ""
		if canPickFolder() {
			pick = `<button type="button" class="sec" id="pickDir" style="flex:0 0 auto">Choose…</button>`
		}
		fmt.Fprintf(b, `<div class="row" style="margin-top:.6rem">
 <div><label>Folder</label>
   <div style="display:flex;gap:.4rem"><input type="text" name="dir" value="%s" placeholder="%s">%s</div></div>
 <div><label>Profile</label><input type="text" name="profile" value="%s" placeholder="Profile 1"></div>
</div>
<p style="margin-top:.7rem"><button>Use this</button></p></form>
<p class="mut">Leave the folder blank to use the standard location.</p></div>`,
			esc(cc.TracksideDir), esc(defaultTracksideDir()), pick, esc(cc.Profile))
	}

	b.WriteString(`
<script>
(function(){
 var btn=document.getElementById('pickDir');
 if(!btn) return;
 btn.addEventListener('click', function(){
   var field=document.getElementsByName('dir')[0];
   var was=btn.textContent; btn.textContent='…'; btn.disabled=true;
   var body=new URLSearchParams();
   if(field&&field.value) body.set('start', field.value);
   fetch('/setup/pick-folder',{method:'POST',body:body})
     .then(function(r){
       if(r.status===204) return null;                 // cancelled
       if(!r.ok) return r.text().then(function(t){ throw new Error(t); });
       return r.json();
     })
     .then(function(j){
       if(!j) return;
       if(field) field.value=j.dir;
       // Said now, while the choice is still in mind, rather than after a save
       // and a restart.
       if(j.warning) alert(j.warning);
     })
     .catch(function(e){ alert(String(e.message||e).trim()); })
     .finally(function(){ btn.textContent=was; btn.disabled=false; });
 });
})();
</script>`)

	writeSetup(w, "FPVTrackside", b.String())
}

// setupPickFolder opens the machine's own folder chooser.
//
// Refused from anywhere but this machine — not only for the usual reason, but
// because a modal dialog opened by a phone in the pits would sit on the timing
// laptop with nobody in front of it, blocking the screen running the meeting.
func (s *liveServer) setupPickFolder(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	if !canPickFolder() {
		http.Error(w, "This machine has no folder picker — type the path instead.",
			http.StatusNotImplemented)
		return
	}

	start := strings.TrimSpace(r.FormValue("start"))
	if start == "" {
		start = defaultTracksideDir()
	}
	dir, err := pickFolder("Choose the FPVTrackside folder", start)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if dir == "" {
		w.WriteHeader(http.StatusNoContent) // cancelled, which is not a failure
		return
	}

	// Checked here so the answer is immediate: the operator finds out they
	// picked the wrong folder while the picker is still in mind, not after
	// saving and restarting.
	msg := ""
	if in := inspectTracksideDir(dir); in == nil {
		msg = "No FPVTrackside data in there — look for the folder containing \"data\" and \"log\"."
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"dir":%q,"warning":%q}`, dir, msg)
}

// ── the ELRS backpack ────────────────────────────────────────────────────────

func (s *liveServer) setupBackpack(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	msg := ""

	if r.Method == http.MethodPost {
		if !localSetupAllowed(r) {
			setupDenied(w)
			return
		}
		_ = r.ParseForm()
		v := localELRS{
			Enabled: r.FormValue("enabled") == "1",
			Mode:    r.FormValue("mode"),
			Netpack: strings.TrimSpace(r.FormValue("netpack")),
			Serial:  strings.TrimSpace(r.FormValue("serial")),
		}
		switch {
		case v.Enabled && (v.Mode == "netpack" || v.Mode == "both") && v.Netpack == "":
			msg = "Give the netpack a hostname — usually elrs-netpack.local."
		default:
			cfg.SetELRS(v)
			if err := cfg.Save(); err != nil {
				msg = "Could not save: " + err.Error()
			} else {
				msg = "Saved — restart dd-pits to connect."
			}
		}
	}

	v := cfg.ELRSSettings()
	if v.Mode == "" {
		// An older file, or a first run: infer what it already meant so the
		// page shows the truth rather than a default that would change it.
		switch {
		case v.Netpack != "" && strings.EqualFold(v.Serial, "auto"):
			v.Mode = "both"
		case v.Serial != "" && v.Netpack == "":
			v.Mode = "usb"
		default:
			v.Mode = "netpack"
		}
	}

	state := `<span class="mut">unknown</span>`
	if s.backpackStatus != nil {
		if s.backpackStatus() {
			state = `<span class="ok">connected</span>`
		} else {
			state = `<span class="warn">not connected</span>`
		}
	}
	chk := ""
	if v.Enabled {
		chk = " checked"
	}
	pick := func(m string) string {
		if strings.EqualFold(v.Mode, m) {
			return " checked"
		}
		return ""
	}
	port := v.Serial
	if port == "" {
		port = "auto"
	}

	b := &strings.Builder{}
	if msg != "" {
		fmt.Fprintf(b, `<div class="card"><p class="ok">%s</p></div>`, esc(msg))
	}
	fmt.Fprintf(b, `<div class="card"><h2>ELRS backpack</h2>
<p class="mut">How this laptop reaches the pilots' goggles. Everything on the goggles — lap times,
DVR file naming, the clock, channel changes — goes through it.</p>
<table><tr><th>Right now</th><td>%s</td></tr></table>

<form method="post" style="margin-top:.9rem">
<label><input type="checkbox" name="enabled" value="1"%s style="width:auto"> Use an ELRS backpack</label>

<div style="margin-top:.8rem">
 <label style="color:var(--fg);font-size:.9rem;font-weight:600">Connection</label>

 <p><label><input type="radio" name="mode" value="netpack"%s style="width:auto">
  <strong>Network (netpack)</strong>
  <span class="mut">— faster, and the only one that reports whether a message actually reached
  the goggles. Needs the venue network to resolve its name. Runs the open-source
  <a href="https://github.com/bob9/elrs-netpack" target="_blank" rel="noopener">ELRS Netpack</a>
  firmware on an Ethernet ESP32 board.</span></label></p>

 <p><label><input type="radio" name="mode" value="usb"%s style="width:auto">
  <strong>USB backpack</strong>
  <span class="mut">— plugged into this laptop. No network involved, so nothing to go wrong there;
  delivery is not confirmed.</span></label></p>

 <p><label><input type="radio" name="mode" value="both"%s style="width:auto">
  <strong>Network, falling back to USB</strong>
  <span class="mut">— tries the netpack first and uses a USB backpack if it is missing. Only
  redundancy if a backpack is actually plugged in.</span></label></p>
</div>

<div id="bpNetpack" style="margin-top:.6rem">
 <label>Netpack hostname</label>
 <input type="text" name="netpack" value="%s" placeholder="elrs-netpack.local">
</div>

<div id="bpUsb" style="margin-top:.6rem">
 <label>USB port</label>
 <select name="serial">%s</select>
 <span class="mut" style="font-size:.8rem">%s</span>
</div>

<p style="margin-top:.8rem"><button>Save</button></p>
</form>
<p class="mut">Also settable as flags (<code>-elrs</code>, <code>-elrs-netpack</code>,
<code>-elrs-serial</code>), and a flag wins over this page.</p></div>`,
		state, chk, pick("netpack"), pick("usb"), pick("both"),
		esc(v.Netpack), serialOptions(port), esc(serialHint()))

	// Only the field that belongs to the chosen connection is shown. Leaving
	// both visible was how the ambiguous combinations got typed in the first
	// place.
	b.WriteString(`
<script>
(function(){
 function sync(){
   var m=(document.querySelector('input[name=mode]:checked')||{}).value||'netpack';
   var np=document.getElementById('bpNetpack'), usb=document.getElementById('bpUsb');
   if(np) np.style.display = (m==='netpack'||m==='both') ? '' : 'none';
   if(usb) usb.style.display = (m==='usb'||m==='both') ? '' : 'none';
 }
 document.querySelectorAll('input[name=mode]').forEach(function(e){
   e.addEventListener('change', sync);
 });
 sync();
})();
</script>`)

	writeSetup(w, "Backpack", b.String())
}

// serialOptions renders the port picker.
//
// A dropdown of what is actually plugged in, because a device path is not
// something an operator can be expected to know: "/dev/cu.usbserial-A50285BI"
// is discoverable only by looking, and a typo produces a backpack that silently
// never connects.
//
// "auto" stays as the first choice and the default — it is the right answer for
// almost everyone, and it keeps working when the port name changes between
// meetings, which on macOS it does.
func serialOptions(selected string) string {
	b := &strings.Builder{}
	sel := func(v string) string {
		if strings.EqualFold(selected, v) {
			return " selected"
		}
		return ""
	}
	fmt.Fprintf(b, `<option value="auto"%s>Find it automatically</option>`, sel("auto"))

	ports := elrs.ListSerialPorts()
	for _, p := range ports {
		fmt.Fprintf(b, `<option value="%s"%s>%s</option>`, esc(p), sel(p), esc(p))
	}
	// A port that was chosen but is not plugged in now must still be offered,
	// or opening the page would silently reset the venue's setting.
	if selected != "" && !strings.EqualFold(selected, "auto") {
		found := false
		for _, p := range ports {
			if strings.EqualFold(p, selected) {
				found = true
			}
		}
		if !found {
			fmt.Fprintf(b, `<option value="%s" selected>%s (not plugged in)</option>`,
				esc(selected), esc(selected))
		}
	}
	return b.String()
}

// serialHint says what the picker is showing, so an empty list reads as "there
// is nothing plugged in" rather than as a broken page.
func serialHint() string {
	n := len(elrs.ListSerialPorts())
	switch n {
	case 0:
		return "No USB devices found — plug the backpack in and reload."
	case 1:
		return "1 USB device found."
	default:
		return fmt.Sprintf("%d USB devices found.", n)
	}
}

// ── commentary booth ─────────────────────────────────────────────────────────

func (s *liveServer) setupBooth(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	msg := ""

	if r.Method == http.MethodPost {
		if !localSetupAllowed(r) {
			setupDenied(w)
			return
		}
		_ = r.ParseForm()
		bc := cfg.BoothConfig()
		// On/off, without destroying where it runs.
		//
		// engine=="" IS the off state, so the previous choice is remembered and
		// restored — otherwise pausing commentary silently reverted a venue to
		// the cloud, which is both a cost and a surprise.
		if r.FormValue("commentary_on") == "1" {
			if bc.Engine == "" {
				if bc.EngineWas != "" {
					bc.Engine = bc.EngineWas
				} else {
					bc.Engine = "qwen" // nothing remembered: the no-setup default
				}
			}
		} else {
			if bc.Engine != "" {
				bc.EngineWas = bc.Engine
			}
			bc.Engine = ""
		}
		bc.Mode = r.FormValue("mode")
		bc.Theme = r.FormValue("theme")
		bc.Humour = r.FormValue("humour")
		bc.Style = strings.TrimSpace(r.FormValue("style"))
		bc.VoicePBP = strings.TrimSpace(r.FormValue("voice_pbp"))
		bc.VoiceColor = strings.TrimSpace(r.FormValue("voice_color"))
		bc.QwenBase = strings.TrimSpace(r.FormValue("qwen_base"))
		bc.AnnounceRecords = r.FormValue("announce_records") == "1"
		bc.ScriptModel = strings.TrimSpace(r.FormValue("script_model"))
		bc.VoicePython = strings.TrimSpace(r.FormValue("voice_python"))
		bc.VoiceRef = strings.TrimSpace(r.FormValue("voice_ref"))
		bc.VoiceRefText = strings.TrimSpace(r.FormValue("voice_ref_text"))
		bc.Language = strings.TrimSpace(r.FormValue("language"))
		bc.Accent = boothpkg.SanitiseAccent(r.FormValue("accent"))
		bc.Energy = r.FormValue("energy")

		// Every toggle is written explicitly once the form posts, so an
		// UNTICKED box sticks rather than reading as "never configured" and
		// switching itself back on.
		on := func(name string) *bool { v := r.FormValue(name) == "1"; return &v }
		bc.PhasePre, bc.PhaseIn, bc.PhasePost = on("phase_pre"), on("phase_in"), on("phase_post")
		bc.Beats = map[string]bool{}
		for _, k := range boothpkg.BeatKinds {
			bc.Beats[k] = r.FormValue("beat_"+k) == "1"
		}

		// Keep-if-blank, like every other stored secret: opening the page and
		// pressing Save must not wipe a key.
		if k := strings.TrimSpace(r.FormValue("qwen_key")); k != "" {
			bc.QwenKey = k
		}
		if r.FormValue("qwen_key_clear") == "1" {
			bc.QwenKey = ""
		}
		if k := strings.TrimSpace(r.FormValue("gemini_key")); k != "" {
			bc.GeminiKey = k
		}
		if r.FormValue("gemini_key_clear") == "1" {
			bc.GeminiKey = ""
		}
		cfg.SetBooth(bc)
		newAudio := localAudio{
			Enabled: r.FormValue("audio_out") == "1",
			Device:  strings.TrimSpace(r.FormValue("audio_device")),
			Latency: strings.TrimSpace(r.FormValue("audio_latency")),
		}
		cfg.SetAudio(newAudio)
		if err := cfg.Save(); err != nil {
			msg = "Could not save: " + err.Error()
		} else {
			msg = "Saved."
			// The booth picks the change up NOW — beats, phases, language,
			// personality. It used to take a restart, with nothing saying so.
			if s.applyBoothConfig != nil {
				s.applyBoothConfig()
			}
			// Sound out applies NOW rather than at the next restart. Changing a
			// speaker used to mean restarting the timing agent, which nobody
			// wants to do mid-meeting — and because the tickbox saved happily
			// and changed nothing, the honest reading of it was that commentary
			// audio was broken.
			if s.applyAudioOut != nil {
				if err := s.applyAudioOut(newAudio.Enabled, newAudio.Device, newAudio.Latency); err != nil {
					msg = "Saved, but the speakers could not be used: " + err.Error()
				} else if newAudio.Enabled {
					msg = "Saved — playing through the speakers now."
				}
			} else if newAudio.Enabled {
				// No audio bus exists, so there is nothing to play. Saying
				// "restart to apply" here would be false — a restart changes
				// nothing until something is actually producing audio.
				msg = "Saved, but there is no audio to play yet — switch on commentary " +
					"or race calls first."
			}
		}
	}

	bc := cfg.BoothConfig()
	au := cfg.AudioSettings()

	var cloned []qwentts.ClonedVoice
	if strings.EqualFold(bc.Engine, "qwen") {
		cloned = clonedVoices(bc.QwenKey, bc.QwenBase)
	}

	audioReady, audioWhy := speakerOutputReady(bc.VoicePython)

	s.renderBooth(w, boothPageData{
		Message:       msg,
		Engine:        bc.Engine,
		Mode:          bc.Mode,
		Themes:        template.HTML(themeOptions(bc.Theme)),
		Humours:       template.HTML(humourOptions(bc.Humour)),
		Energy:        template.HTML(energyOptions(bc.Energy)),
		Style:         bc.Style,
		Accent:        bc.Accent,
		QwenBase:      bc.QwenBase,
		QwenKeySet:    bc.QwenKey != "",
		GeminiKey:     bc.GeminiKey != "",
		ScriptModel:   bc.ScriptModel,
		VoicePython:   bc.VoicePython,
		VoiceRef:      bc.VoiceRef,
		VoiceRefTxt:   bc.VoiceRefText,
		Records:       bc.AnnounceRecords,
		AudioOut:      au.Enabled,
		AudioOutReady: audioReady,
		AudioOutWhy:   audioWhy,
		AudioDevices:  template.HTML(audioDeviceOptions(s, au.Device)),
		AudioNow:      s.audioPlaying != nil && s.audioPlaying(),
		AudioDevice:   au.Device,
		AudioLat:      au.Latency,
		VoicePBP:      template.HTML(qwenVoiceOptions(bc.VoicePBP, qwentts.DefaultVoicePBP, cloned)),
		VoiceColor:    template.HTML(qwenVoiceOptions(bc.VoiceColor, qwentts.DefaultVoiceColor, cloned)),
		Languages:     template.HTML(languageOptions(bc.Language, bc.Engine)),
		Beats:         template.HTML(beatToggles(bc)),
	})
}

// boothPageData is the commentary form's fields, BY NAME.
//
// This page was built with a single Sprintf carrying thirty-odd positional
// arguments, and it garbled three separate times: a new field inserted in the
// markup but added to the argument list in the wrong place shifts every value
// after it, so a voice list renders into the language box and raw HTML into a
// text input. Nothing errors — Sprintf cannot know the order was wrong.
//
// Named fields make that impossible: a mismatch is a template error naming the
// field, not a screen full of markup.
type boothPageData struct {
	Message     string
	Engine      string
	Mode        string
	Themes      template.HTML
	Humours     template.HTML
	Energy      template.HTML
	Style       string
	Accent      string
	QwenBase    string
	QwenKeySet  bool
	GeminiKey   bool
	ScriptModel string
	VoicePython string
	VoiceRef    string
	VoiceRefTxt string
	Records     bool
	AudioOut    bool
	// AudioOutReady says whether the speaker path can actually run here, and
	// AudioOutWhy says what is missing. This is surfaced on the PAGE because it
	// used to be a single log line: someone using cloud TTS has no reason to
	// have Python or sounddevice installed, so the tickbox silently did
	// nothing and the only symptom was "commentary has no audio".
	AudioOutReady bool
	AudioOutWhy   string
	// AudioDevices is the rendered <select> of real outputs. A free-text box
	// meant typing a device name from memory, where a typo is indistinguishable
	// from a device that is not working.
	AudioDevices template.HTML
	AudioNow     bool
	AudioDevice  string
	AudioLat     string

	// Pre-rendered option lists. HTML because they are markup this file built,
	// never anything a user typed.
	VoicePBP   template.HTML
	VoiceColor template.HTML
	Languages  template.HTML
	Beats      template.HTML
}

var boothTmpl = template.Must(template.New("booth").Funcs(template.FuncMap{
	"eq": strings.EqualFold,
}).Parse(`
{{if .Message}}<div class="card"><p class="ok">{{.Message}}</p></div>{{end}}

<div class="card"><h2>Commentary booth</h2>
<p class="mut">The booth calls the racing live from the timing data. It is optional — race calls
work without it.</p>

<form method="post">
<label style="display:flex;align-items:center;gap:.5rem;font-size:1rem;color:var(--fg)">
 <input type="checkbox" name="commentary_on" value="1"{{if .Engine}} checked{{end}} style="width:auto">
 <strong>Commentary is switched on</strong></label>
<p class="mut" style="font-size:.83rem;margin:.2rem 0 .9rem">Untick to silence the booth. Race
 calls and everything else keep working, and your setup is remembered for when you turn it
 back on.</p>

<div style="padding:.7rem .8rem;border:1px solid var(--line);border-radius:9px;margin-bottom:1rem">
 <strong>Models, keys and where the AI runs live on one page:</strong>
 <a href="/setup/models">Setup &rarr; AI models</a>.
 <div class="mut" style="font-size:.83rem;margin-top:.3rem">
  {{if .Engine}}Currently: <strong>{{if eq .Engine "local"}}voice on this Mac{{else}}voice from the cloud{{end}}{{if .ScriptModel}}, words on this Mac{{else}}, words from the cloud{{end}}</strong>.
  {{else}}Nothing is set up yet — start there.{{end}}
 </div>
 <div class="mut" style="font-size:.83rem;margin-top:.35rem">This page is for
  <strong>what it says</strong> and <strong>how you hear it</strong>.</div>
</div>

<div style="padding:.6rem .8rem;border:1px solid var(--line);border-radius:9px;margin-bottom:1rem">
 <strong>Voices, language and accent are on the AI models page:</strong>
 <a href="/setup/models">Setup &rarr; AI models</a>
 <div class="mut" style="font-size:.83rem;margin-top:.25rem">Everything about the AI &mdash; which
  models, where they run, which voices and how they sound &mdash; is in one place there.</div>
</div>

<label style="color:var(--fg);font-weight:600">What the booth talks about</label>
<p class="mut" style="font-size:.82rem;margin:.1rem 0 .5rem">Switch a whole phase off, or pick
individual moments. A moment only happens when its phase is on as well.</p>
{{.Beats}}
</div>



<label id="cmRecords" style="margin-top:.6rem"><input type="checkbox" name="announce_records" value="1"{{if .Records}} checked{{end}} style="width:auto">
 🏅 Call personal bests — a pilot beating their own best lap, or their best run over the scoring window</label>

<div style="margin-top:1rem;padding-top:.8rem;border-top:1px solid var(--line)">
<label style="color:var(--fg);font-weight:600">How to hear it</label>
<p style="margin:.3rem 0 .6rem">
 <a href="/booth.wav" target="_blank" rel="noreferrer"
    style="display:inline-block;padding:.45rem .8rem;border-radius:8px;
           background:var(--accent,#b6e925);color:#111;font-weight:600;text-decoration:none">
  &#128266; Listen in a browser tab</a>
 <span class="mut" style="font-size:.82rem;margin-left:.5rem">Works straight away &mdash; nothing to install.</span></p>
<p class="mut" style="font-size:.82rem;margin-top:0">Same address goes into OBS as a Media Source:
 <code>/booth.wav</code> (untick &ldquo;Local File&rdquo;).</p>

<label style="margin-top:.7rem"><input type="checkbox" name="audio_out" value="1"{{if .AudioOut}} checked{{end}} style="width:auto">
 Play through this laptop's speakers</label>
{{if .AudioOutReady}}<p class="mut" style="font-size:.82rem">Takes effect when you <strong>restart dd-pits</strong>.</p>
{{else}}<p style="font-size:.82rem;color:var(--warn,#c88);margin:.2rem 0 0">
 <strong>This option can&rsquo;t run on this machine yet.</strong> {{.AudioOutWhy}}<br>
 It is only needed to use the laptop&rsquo;s own speakers &mdash; the browser tab above needs none of it,
 and OBS takes <code>/booth.wav</code> directly.</p>{{end}}
<div class="row">
 <div><label>Output device</label>{{.AudioDevices}}</div>
 <div><label>Buffer</label><input type="text" name="audio_latency" value="{{.AudioLat}}" placeholder="0.06">
   <span class="mut" style="font-size:.8rem">Raise it if audio breaks up.</span></div>
</div>
</div>

<p style="margin-top:1rem"><button>Save</button></p>
</form></div>
`))

// renderBooth writes the commentary page.
func (s *liveServer) renderBooth(w http.ResponseWriter, d boothPageData) {
	var body strings.Builder
	if err := boothTmpl.Execute(&body, d); err != nil {
		http.Error(w, "commentary page: "+err.Error(), http.StatusInternalServerError)
		return
	}
	body.WriteString(boothPageScript)
	writeSetup(w, "Commentary", body.String())
}

// boothPageScript shows only the fields the chosen engine uses, and plays a
// voice sample.
const boothPageScript = `
<script>
(function(){
 var sel=document.querySelector('select[name=engine]');
 if(!sel) return;
 function show(id,on){ var e=document.getElementById(id); if(e) e.style.display = on ? '' : 'none'; }
 function sync(){
   var e=sel.value, on = e!=='';
   show('cmWriter', on); show('cmStyle', on); show('cmStyleWrap', on);
   show('cmRecords', on); show('cmLang', on); show('cmBeats', on);
   show('cmQwen', e==='qwen'); show('cmVoices', e==='qwen');
   show('cmQwenHelp', e==='qwen'); show('cmClone', e==='qwen');
   show('cmWriterQwen', e==='qwen');
   show('cmSolo', e==='local'); show('cmLocalVoice', e==='local');
 }
 sel.addEventListener('change', sync);
 sync();

 var audio=null;
 document.querySelectorAll('[data-hear]').forEach(function(btn){
   btn.addEventListener('click', function(){
     var pick=document.getElementsByName(btn.dataset.hear)[0];
     if(!pick) return;
     var keyEl=document.getElementsByName('qwen_key')[0];
     var baseEl=document.getElementsByName('qwen_base')[0];
     var langEl=document.getElementsByName('language')[0];
     var body=new URLSearchParams();
     body.set('voice', pick.value);
     if(langEl) body.set('language', langEl.value);
     // The delivery settings ride along so the sample sounds like the real
     // thing, including changes not saved yet.
     ['theme','humour','energy','accent','style'].forEach(function(n){
       var el=document.getElementsByName(n)[0];
       if(el&&el.value) body.set(n, el.value);
     });
     if(keyEl&&keyEl.value) body.set('key', keyEl.value);
     if(baseEl&&baseEl.value) body.set('base', baseEl.value);

     var was=btn.textContent; btn.textContent='…'; btn.disabled=true;
     fetch('/setup/voice-test',{method:'POST',body:body})
       .then(function(r){
         if(!r.ok) return r.text().then(function(t){ throw new Error(t); });
         return r.blob();
       })
       .then(function(b){ if(audio){audio.pause();} audio=new Audio(URL.createObjectURL(b)); audio.play(); })
       .catch(function(e){ alert(String(e.message||e).trim()); })
       .finally(function(){ btn.textContent=was; btn.disabled=false; });
   });
 });
})();
</script>`

// ── who speaks the race calls ────────────────────────────────────────────────

func (s *liveServer) setupCalls(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	msg := ""

	if r.Method == http.MethodPost {
		if !localSetupAllowed(r) {
			setupDenied(w)
			return
		}
		_ = r.ParseForm()
		before := callsSource(cfg.CallsConfig(), s.hasVoice)
		cfg.SetCalls(localCalls{
			Source:       r.FormValue("source"),
			TracksideDir: strings.TrimSpace(r.FormValue("trackside_dir")),
			Profile:      strings.TrimSpace(r.FormValue("profile")),
		})
		if err := cfg.Save(); err != nil {
			msg = "Could not save: " + err.Error()
		} else if after := callsSource(cfg.CallsConfig(), s.hasVoice); after != before {
			// The OTHER program's half of the handover, settled here.
			//
			// Handing calls to dd-pits used to end with "mute FPVTrackside's own
			// audio output" — an instruction. Handing them back said nothing,
			// and the operator was left hunting FPVTrackside's audio settings
			// for a mute that is not even in them (its button is runtime-only;
			// the persistent lever is TextToSpeechVolume). One switch should
			// mean one switch.
			vol := 0
			if after == "trackside" {
				vol = 100
			}
			ftNote := ftSetSpeechVolume(cfg.CallsConfig(), vol)
			log.Printf("race calls: %s", ftNote)
			// FPVTrackside running right now means it is still on the OLD
			// setting in memory. Remember that beyond the two-second restart
			// page: the calls page banners it until the reconciler sees
			// FPVTrackside actually close.
			if ftRunning() {
				cc2 := cfg.CallsConfig()
				cc2.FTRestartNeeded = true
				cfg.SetCalls(cc2)
				_ = cfg.Save()
			}
			// Applied by restarting, right now, without being asked.
			//
			// The announcer only consults this setting when it STARTS, so a
			// save used to store the choice and change nothing: the operator
			// picked "FPVTrackside speaks", the page agreed, and dd-pits kept
			// calling laps. A banner saying "restart to apply" is technically
			// honest and practically ignored while the wrong audio keeps
			// playing over it. Who speaks the race calls is not a setting
			// anyone changes casually mid-heat, so the restart is the correct
			// price and it is paid immediately rather than remembered.
			log.Printf("race calls: source changed (%s -> %s) — restarting to apply", before, after)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8">
<title>Applying…</title><body style="font-family:system-ui;background:#111;color:#ddd;padding:2rem">
<h2>Saved — restarting to hand the audio over</h2>
<p>dd-pits is restarting so the change takes effect now. This page will come back by itself.</p>
<p><strong>FPVTrackside: </strong>` + template.HTMLEscapeString(ftNote) + `</p>
<script>const t=setInterval(async()=>{try{const r=await fetch('/setup/calls',{cache:'no-store'});
if(r.ok){clearInterval(t);location.href='/setup/calls';}}catch(e){}},700);</script>`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			go func() {
				time.Sleep(600 * time.Millisecond) // let the response land
				exe, err := osExecutable()
				if err == nil {
					err = restartSelf(exe)
				}
				if err != nil {
					log.Printf("restart: %v — still running the old process; the calls "+
						"source applies on the next manual restart", err)
				}
			}()
			return
		} else {
			msg = "Saved."
		}
	}

	cur := callsSource(cfg.CallsConfig(), s.hasVoice)
	pick := func(v string) string {
		if cur == v {
			return " checked"
		}
		return ""
	}

	b := &strings.Builder{}
	if msg != "" {
		fmt.Fprintf(b, `<div class="card"><p class="ok">%s</p></div>`, esc(msg))
	}
	// The FPVTrackside handover, asked for out loud and confirmed when done.
	//
	// This used to live on the two-second restarting page, which is the same as
	// not saying it. The ask stays on screen until the reconciler SEES
	// FPVTrackside close (the page watches and updates itself), and the
	// confirmation appears exactly once so "it worked" is said rather than
	// implied by a warning quietly disappearing.
	ccNow := cfg.CallsConfig()
	if ccNow.FTRestartNeeded {
		b.WriteString(`<div class="card" style="border-color:#c66">
<h2>⚠ Close FPVTrackside</h2>
<p>The audio handover is saved, but FPVTrackside is still running with its
PREVIOUS setting — close it now (quit it fully, not just the window).</p>
<p class="mut">The moment it closes, dd-pits sets its audio to match. This page
is watching and will tell you when that has happened — then start FPVTrackside
again whenever you're ready.</p></div>
<script>const ftw=setInterval(async()=>{try{const r=await fetch('/setup/calls/ft-status',{cache:'no-store'});
const j=await r.json(); if(!j.needed){clearInterval(ftw);location.reload();}}catch(e){}},2000);</script>`)
	} else if ccNow.FTRestartDone {
		b.WriteString(`<div class="card" style="border-color:#5a5">
<h2>✓ FPVTrackside closed — done</h2>
<p>Its audio setting has been corrected, and it will start up matching the
choice below. Start FPVTrackside again whenever you're ready.</p></div>`)
		ccNow.FTRestartDone = false
		cfg.SetCalls(ccNow)
		_ = cfg.Save()
	}

	fmt.Fprintf(b, `<div class="card"><h2>Who speaks the race calls</h2>
<p class="mut">%s</p>
<form method="post">
<p><label><input type="radio" name="source" value="ddpits" %s style="width:auto"> <strong>dd-pits</strong>
 — one voice for the calls and the commentary, and it can say things FPVTrackside cannot.
 <span class="mut">Mute FPVTrackside's own audio output.</span></label></p>
<p><label><input type="radio" name="source" value="trackside" %s style="width:auto"> <strong>FPVTrackside</strong>
 — it speaks its own calls, exactly as it always has, and dd-pits leaves the audio alone.
 <span class="mut">The dashboard, overlays and goggles all still work.</span></label></p>
<p style="margin-top:.8rem"><button>Save</button></p></form>

<div style="margin-top:1rem;padding-top:.8rem;border-top:1px solid var(--line)">
<p style="margin:0"><strong>Commentary is a separate choice.</strong></p>
<p class="mut" style="margin:.3rem 0 0">This setting is only about the <em>lap times and results</em>.
The commentary booth is switched on independently, so the common arrangement —
<strong>FPVTrackside calling the laps, dd-pits providing the commentary</strong> — is simply
FPVTrackside above plus a voice on the <a href="/setup/booth">Commentary</a> page. That way the
timing calls stay exactly as your pilots are used to and the colour is added around them.</p>
<p class="mut" style="margin:.3rem 0 0">Right now: %s</p>
</div></div>`,
		esc(describeCallsSource(cur, s.hasVoice)), pick("ddpits"), pick("trackside"),
		describeCommentary(cfg.BoothConfig()))

	b.WriteString(`<div class="card"><h2>Where FPVTrackside is</h2>
<p class="mut">dd-pits reads its wording and its log from the FPVTrackside folder —
<a href="/setup/trackside">check or change it</a>.</p></div>`)

	if !s.hasVoice {
		b.WriteString(`<div class="card"><h2>No voice is configured</h2>
<p>dd-pits cannot speak without one, so FPVTrackside is doing it. To change that, either set up
the <a href="/setup/booth">local model</a> (Apple Silicon) or add a Qwen key — both are on the
commentary page.</p></div>`)
	}
	writeSetup(w, "Race calls", b.String())
}

// ── goggle control (the cloud command queue's local replacement) ──────────────

func (s *liveServer) setupGoggles(w http.ResponseWriter, r *http.Request) {
	msg := ""
	if r.Method == http.MethodPost {
		if !localSetupAllowed(r) {
			setupDenied(w)
			return
		}
		_ = r.ParseForm()

		switch r.FormValue("action") {
		case "enable":
			if s.enableBackpack == nil {
				msg = "This agent cannot start the backpack."
			} else if err := s.enableBackpack(); err != nil {
				msg = "Could not start the backpack: " + err.Error()
			} else {
				msg = "Backpack starting — it may take a few seconds to connect."
			}

		case "announce":
			text := strings.TrimSpace(r.FormValue("message"))
			var who []string
			if r.FormValue("to") != "" && r.FormValue("to") != "*" {
				who = []string{r.FormValue("to")}
			}
			sent, failed := s.announce(text, who)
			switch {
			case len(sent) == 0 && len(failed) > 0:
				msg = "Nothing sent: " + firstError(failed)
			case len(failed) > 0:
				msg = fmt.Sprintf("Sent to %s — but %s", strings.Join(sent, ", "), firstError(failed))
			default:
				msg = "Sent to " + strings.Join(sent, ", ")
			}

		default:
			call := strings.TrimSpace(r.FormValue("callsign"))
			ch := strings.TrimSpace(r.FormValue("channel"))
			if err := s.setGoggleChannel(call, ch); err != nil {
				msg = "Could not change " + call + ": " + err.Error()
			} else {
				msg = call + " set to " + ch
			}
		}
	}

	b := &strings.Builder{}
	if msg != "" {
		fmt.Fprintf(b, `<div class="card"><p class="ok">%s</p></div>`, esc(msg))
	}

	// Not started yet: offer to start it rather than naming a flag.
	if s.setChannel == nil {
		b.WriteString(`<div class="card"><h2>The backpack is not running</h2>
<p>Goggle messages go out over an ELRS backpack, and one has not been started.</p>
<form method="post"><input type="hidden" name="action" value="enable">
<p><button>Start the backpack</button></p></form>
<p class="mut">Set which backpack on the <a href="/setup/backpack">Backpack</a> page first.</p></div>`)
		writeSetup(w, "Goggle control", b.String())
		return
	}

	// WHO IS RACING, first.
	//
	// A race director changing a channel is almost always changing it for
	// someone in the heat that is on the line right now. Making them find that
	// pilot in an alphabetical list of everyone who has ever been configured is
	// the wrong way round.
	racing := s.racingPilots()
	configured := map[string]bool{}
	for _, p := range s.cfg.PilotList() {
		if p.resolvedUID() != "" {
			configured[strings.ToUpper(p.Callsign)] = true
		}
	}

	sig := s.currentHeatName()
	for _, p := range racing {
		sig += "|" + p.Name + ":" + p.ChannelLabel
	}

	fmt.Fprintf(b, `<div id="gogLive" data-sig="%s">`, esc(sig))
	if len(racing) > 0 {
		fmt.Fprintf(b, `<div class="card"><h2>On the line now — %s</h2>
<table><tr><th>Pilot</th><th>Channel</th><th>Goggles</th><th>Change to</th></tr>`, esc(s.currentHeatName()))
		for _, p := range racing {
			state := `<span class="warn">not set up</span>`
			if configured[strings.ToUpper(p.Name)] {
				state = `<span class="ok">✓</span>`
			}
			fmt.Fprintf(b, `<tr><td><strong>%s</strong></td><td>%s</td><td>%s</td>
<td><form method="post" style="margin:0;display:flex;gap:.4rem">
<input type="hidden" name="callsign" value="%s">
<select name="channel" style="width:auto">%s</select>
<button class="sec">Set</button></form></td></tr>`,
				esc(p.Name), esc(p.ChannelLabel), state, esc(p.Name), channelOptions(p.ChannelLabel))
		}
		b.WriteString(`</table></div>`)
	} else {
		b.WriteString(`<div class="card"><h2>Nobody is racing</h2>
<p class="mut">When a heat is on the line its pilots appear here, with their channels, so you can
change one without hunting for them.</p></div>`)
	}
	b.WriteString(`</div>`)

	// Everyone configured, for a change outside a heat.
	pilots := s.cfg.PilotList()
	b.WriteString(`<div class="card"><h2>Any pilot</h2>`)
	if len(pilots) == 0 {
		b.WriteString(`<p class="warn">No goggle pilots are set up yet — <a href="/setup/pilots">add one</a>.</p>`)
	} else {
		b.WriteString(`<form method="post"><div class="row">
<div><label>Pilot</label><select name="callsign">`)
		for _, p := range pilots {
			if p.resolvedUID() == "" {
				continue
			}
			fmt.Fprintf(b, `<option value="%s">%s</option>`, esc(p.Callsign), esc(p.Callsign))
		}
		b.WriteString(`</select></div><div><label>Channel</label><select name="channel">` +
			channelOptions("") + `</select></div></div>
<p style="margin-top:.8rem"><button>Set channel</button></p></form>`)
	}
	b.WriteString(`</div>`)

	// Messages to the goggles.
	b.WriteString(`<div class="card"><h2>Send a message</h2>
<p class="mut">Puts a line on the goggle screen for a few seconds. A pilot in goggles cannot hear
the PA or see you waving — this is how you reach them.</p>
<form method="post"><input type="hidden" name="action" value="announce">
<div class="row">
 <div style="flex:2"><label>Message</label>
   <input type="text" name="message" maxlength="40" placeholder="TRACK CLEAR" required></div>
 <div><label>To</label><select name="to"><option value="*">Everyone</option>`)
	for _, p := range pilots {
		if p.resolvedUID() == "" {
			continue
		}
		fmt.Fprintf(b, `<option value="%s">%s</option>`, esc(p.Callsign), esc(p.Callsign))
	}
	b.WriteString(`</select></div></div>
<p style="margin-top:.8rem"><button>Send</button></p></form>
<p class="mut">Shown in capitals — the goggle font has no lowercase.</p></div>`)

	// Follow the running order.
	//
	// The pilots on this page are the ones on the line, so it is wrong the
	// moment a heat changes — and a race director looking at a stale list is
	// exactly how a channel gets changed for the wrong pilot.
	//
	// Only the live card is swapped, and only when its signature differs, so a
	// half-typed message or an open channel picker elsewhere is never disturbed.
	b.WriteString(`
<script>
(function(){
 var live=document.getElementById('gogLive');
 if(!live) return;
 var busy=false;
 function refresh(){
   if(busy) return; busy=true;
   fetch(location.pathname,{headers:{'X-Requested-With':'fetch'}})
     .then(function(r){return r.text()})
     .then(function(html){
       var doc=new DOMParser().parseFromString(html,'text/html');
       var next=doc.getElementById('gogLive');
       var cur=document.getElementById('gogLive');
       if(!next||!cur) return;
       if(next.dataset.sig===cur.dataset.sig) return;   // nothing changed
       // Never swap under someone's hands: if the operator is using a control
       // inside this card, leave it and pick the change up next time.
       if(cur.contains(document.activeElement)) return;
       cur.replaceWith(next);
     })
     .catch(function(){})
     .finally(function(){ busy=false; });
 }
 // The timing stream is the signal; the interval is a backstop for a dropped
 // connection, which at a venue is a matter of when rather than if.
 try{
   var es=new EventSource('/events');
   es.onmessage=refresh;
   es.onerror=function(){};
 }catch(e){}
 setInterval(refresh, 10000);
})();
</script>`)

	writeSetup(w, "Goggle control", b.String())
}

// channelOptions renders the channel picker, marking the pilot's current one.
func channelOptions(current string) string {
	b := &strings.Builder{}
	for _, ch := range localChannelChoices() {
		sel := ""
		if strings.EqualFold(ch, current) {
			sel = " selected"
		}
		fmt.Fprintf(b, `<option value="%s"%s>%s</option>`, esc(ch), sel, esc(ch))
	}
	return b.String()
}

// firstError renders one failure from a batch. With a field of six, "it did not
// work" is useless — which pilot missed it is the whole question.
func firstError(failed map[string]error) string {
	keys := make([]string, 0, len(failed))
	for k := range failed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	k := keys[0]
	if k == "*" {
		return failed[k].Error()
	}
	extra := ""
	if len(keys) > 1 {
		extra = fmt.Sprintf(" (and %d more)", len(keys)-1)
	}
	return fmt.Sprintf("%s: %v%s", k, failed[k], extra)
}

// localChannelChoices is the set a race director actually uses, in band order.
func localChannelChoices() []string {
	out := []string{}
	for _, band := range []struct {
		prefix string
		n      int
	}{{"R", 8}, {"F", 8}, {"L", 8}, {"E", 8}} {
		for i := 1; i <= band.n; i++ {
			out = append(out, fmt.Sprintf("%s%d", band.prefix, i))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return false }) // keep band order
	return out
}

// ── help ─────────────────────────────────────────────────────────────────────

func (s *liveServer) setupHelp(w http.ResponseWriter, r *http.Request) {
	base := esc(setupBaseURL)
	writeSetup(w, "Help", `

<div class="card"><h2>What this is</h2>
<p>dd-pits runs on the timing laptop, reads FPVTrackside, and does the rest itself: the
dashboard pilots watch, the OBS overlay, the race calls, live commentary, and the lap
times in pilots' goggles.</p>
<p><strong>None of it needs the internet or an account.</strong> Everything below is configured on this
machine and stored in one file beside the agent.</p>
</div>

<div class="card"><h2>Getting started — 5 minutes</h2>
<p><strong>1. Start FPVTrackside</strong> and open your event. dd-pits reads it at
<code>http://localhost:8080</code>; nothing to configure if it is on this machine.</p>

<p><strong>2. Check the dashboard.</strong> Open <a href="/">the dashboard</a>. If you can see your
event and pilots, dd-pits is talking to FPVTrackside and the hard part is done. Pilots can
open the same address on their phones over the venue wifi.</p>

<p><strong>3. Decide who speaks the lap times</strong> — <a href="/setup/calls">Race calls</a>.
Leave it on <strong>FPVTrackside</strong> and it announces laps exactly as it always has; you need
nothing else. Choose <strong>dd-pits</strong> for one voice across calls and commentary, and mute
FPVTrackside's own audio so you do not hear both.</p>

<p><strong>4. Add your pilots' goggles</strong> (optional) — <a href="/setup/pilots">Goggle pilots</a>.
One <strong>bind phrase</strong> per pilot is all it takes. See "Goggles" below.</p>

<p><strong>5. Turn on commentary</strong> (optional) — <a href="/setup/booth">Commentary</a>.
See "Commentary" below; it needs two things, not one.</p>

<p class="mut">Settings take effect when you restart dd-pits, except pilots and OSD screens, which
apply on the next heat.</p>
</div>

<div class="card"><h2>Race day</h2>
<p>All of these work with no internet. Give pilots and spectators the first one.</p>
<table>
<tr><th><a href="/">Dashboard</a></th><td>Current heat, standings, best-consecutive, time trials</td></tr>
<tr><th><a href="/dronedash/races">Race history</a></th><td>Every heat with lap times</td></tr>
<tr><th><a href="/live">Live view</a></th><td>Full-screen current race, for a TV at the pits</td></tr>
<tr><th><a href="/overlay">OBS overlay</a></th><td>Add as a Browser Source in OBS</td></tr>
<tr><th><code>/booth.wav</code></th><td>Commentary audio — add as a Media Source in OBS</td></tr>
<tr><th><a href="/calls">Manual calls</a></th><td>"Hurry up" and other operator calls</td></tr>
<tr><th><a href="/goggles">Goggle control</a></th><td>Change a pilot's video channel</td></tr>
<tr><th><a href="/docs">JSON API</a></th><td>For building your own displays</td></tr>
</table>
</div>

<div class="card"><h2>Goggles — lap times, DVR names, clock</h2>
<p><strong>What you need:</strong> an ELRS backpack this laptop can reach, and each pilot's bind phrase.</p>

<p><strong>The backpack</strong> — <a href="/setup/backpack">Backpack</a>. Choose one:</p>
<ul>
<li><strong>Network (netpack)</strong> — an
<a href="https://github.com/bob9/elrs-netpack" target="_blank" rel="noopener">ELRS Netpack</a>
(open-source firmware for an Ethernet ESP32 board; one PoE cable powers and connects it) on the
venue network. Faster, and the only option that tells you whether a message actually reached the
goggles. Its name is usually <code>elrs-netpack.local</code>, and browsing to that address shows its
firmware update page and a goggle test console.</li>
<li><strong>USB backpack</strong> — plugged into this laptop. Nothing network-related to go wrong.
Pick the port from the list, or leave it on "find it automatically".</li>
<li><strong>Network, falling back to USB</strong> — the safest if you have both. Only real redundancy
if a USB backpack is actually plugged in.</li>
</ul>

<p><strong>The pilots</strong> — <a href="/setup/pilots">Goggle pilots</a>. For each pilot, enter their
callsign <em>exactly as it appears in FPVTrackside</em> and the bind phrase they typed into their
goggles. That is all — the goggle ID is worked out from the phrase, the same way the
ExpressLRS firmware does it.</p>

<p>Once a pilot is added they get their lap times on screen, their DVR files named after the
heat, and their goggle clock set correctly. The <a href="/setup/osd">OSD screen</a> page controls what
they see, and you can give one pilot their own screen if they want it different.</p>

<p class="mut"><strong>If a pilot sees nothing:</strong> check the callsign matches FPVTrackside exactly,
check the backpack says "connected" on the Backpack page, and check they have not been switched
off on the pilots page.</p>
</div>

<div class="card"><h2>Commentary — it needs TWO things</h2>
<p>Something to <strong>write</strong> the words, and a <strong>voice</strong> to say them. Setting only one is the
most common mistake; the booth simply will not start.</p>

<p><strong>The writer</strong> — either a model on this machine (no internet, needs Apple Silicon and a
few GB of memory) or a Google Gemini key (works on any hardware, needs a connection).</p>

<p><strong>The voice</strong> — either the local voice (no internet, wants the graphics chip) or Qwen3-TTS
(any hardware, needs a connection, a dropout means silence rather than a bad call).</p>

<p>Both are on the <a href="/setup/booth">Commentary</a> page. Mix and match freely — a common
setup is a local writer with a Qwen voice, which keeps the words private and the graphics chip
free for the race calls.</p>
</div>

<div class="card"><h2>Sound</h2>
<p>Commentary and race calls share one audio bus, so a lap time always interrupts colour rather
than queueing behind it.</p>
<p>Two ways to hear it: <strong>through this laptop's speakers</strong> (tick it on the
<a href="/setup/booth">Commentary</a> page), or <strong>into OBS</strong> by adding
<code>`+base+`/booth.wav</code> as a Media Source — untick "Local File" and paste that address.</p>
<p class="mut">Bluetooth headphones add about half a second of delay of their own, whatever is
configured here. For a stream or a PA, use a wired output.</p>
</div>

<div class="card"><h2>If you have no voice at all</h2>
<p>Set <a href="/setup/calls">Race calls</a> to <strong>FPVTrackside</strong>. It announces laps exactly as
it always has, and dd-pits stays out of the audio — you still get the dashboard, the overlays,
the goggle OSD and the API. This is also what happens automatically when nothing is
configured, so you can never end up with silence by accident.</p>
</div>

<div class="card"><h2>Your stream — Watch links and chapters</h2>
<p>If you streamed the day to YouTube, the <a href="/setup/video">Video</a> page turns
that stream into a <strong>&#9654; Watch</strong> link on every heat, and a
<strong>chapter list</strong> to paste into the video&rsquo;s description.</p>
<p>You only have to place <em>one</em> heat. Find any heat in the video, note the time
on the scrubber, and enter it. Every other heat is worked out from there, because
the timing data already knows when each one started &mdash; so you never scrub
through the whole video hunting for them.</p>
<ol>
 <li>Paste the stream&rsquo;s YouTube address.</li>
 <li>Pick a heat you can find in the video and type its time (<code>1:12:30</code>).</li>
 <li>Press <strong>Anchor</strong>. Every heat now has a link.</li>
</ol>
<p>Copy the chapter list into the description and YouTube shows clickable chapters.
The <code>00:00:00 Stream Start</code> line at the top is <strong>required</strong> &mdash;
without a chapter at zero YouTube ignores the whole list, which is the usual reason
a pasted list appears to do nothing.</p>
<p>A heat that ran <em>before</em> the stream started gets no link, rather than a link
to the wrong moment.</p>
<h3>Commentary over the video</h3>
<p>Once a heat has finished you can have the booth <strong>write and voice a call for
it after the fact</strong>, and the Watch player will play that over the muted video.
This is for a day you streamed with nobody on the microphone.</p>
<p>It needs commentary set up (see above) &mdash; the same writer and voice the live
booth uses. Scrubbing the video scrubs the commentary with it, so it stays lined up.</p>
<p>Broadcasts usually open with a bit of pre-race footage, so there is a
<strong>delay</strong> setting that holds the commentary back that many seconds into
the video. Eight is a reasonable starting point; press a heat&rsquo;s &#9654; to hear it
against the real footage and adjust.</p>
<p>A heat with <strong>no recorded lap times</strong> is refused. There is nothing to
describe, and a commentator handed nothing does not say so &mdash; it makes a race up,
which sounds exactly as convincing as a real one.</p>
</div>

<div class="card"><h2>☕ Support dd-pits</h2>
<p>dd-pits is free and open source, built by a race director for race directors. If it
saves your club time on race day, you can say thanks and help keep it going at
<a href="`+supportURL+`" target="_blank" rel="noopener">buymeacoffee.com/bobnine</a>.</p>
</div>

<div class="card"><h2>Who can change these settings</h2>
<p><strong>Only this laptop.</strong> The dashboard is deliberately open to the whole venue network so
pilots can watch it — which is exactly why changing someone's goggle channel from a phone in
the pits is refused.</p>
<p class="mut">To configure from a phone anyway, start with <code>-setup-pin 1234</code> and open
<code>`+base+`/setup?pin=1234</code>.</p>
</div>

<div class="card"><h2>Your settings file</h2>
<p>One JSON file beside the agent. Back it up with your race-day files — it is plain text, and
copying it to another laptop moves the whole setup with it.</p>
<p class="mut">Everything here can also be set with command-line flags, and a flag always wins over
these pages. Run <code>dd-pits -h</code> to see them.</p>
</div>`)
}

// logSetupReady points the operator at the pages once, at startup.
//
// Worth a line every time: a venue running offline has no other way to discover
// that these pages exist, and "the goggles do nothing" is not a symptom anyone
// would think to solve by opening a web page.
func logSetupReady() {
	log.Printf("local setup: configure this venue at %s/setup — help at %s/setup/help",
		setupBaseURL, setupBaseURL)
}

// racingPilots returns the pilots in the heat on the line, with their channels.
//
// The reason Goggle control leads with these: a race director changing a
// channel is nearly always changing it for someone in the heat that is up right
// now. Making them find that pilot in an alphabetical list of everyone ever
// configured is the wrong way round, and slow at exactly the moment there is no
// time.
func (s *liveServer) racingPilots() []ddapi.Pilot {
	mv, _, ok := s.meeting()
	if !ok || mv == nil || mv.Current == nil {
		return nil
	}
	return mv.Current.Pilots
}

// currentHeatName labels the heat on the line, so the list has a heading an
// operator can check against the screen in front of them.
func (s *liveServer) currentHeatName() string {
	mv, _, ok := s.meeting()
	if !ok || mv == nil || mv.Current == nil {
		return "current heat"
	}
	r := mv.Current
	if n := strings.TrimSpace(r.RoundName); n != "" {
		return fmt.Sprintf("%s, race %d", n, r.Number)
	}
	return fmt.Sprintf("round %d, race %d", r.Round, r.Number)
}

// announce sends a message to the goggles, of one pilot or of everyone.
func (s *liveServer) announce(text string, who []string) ([]string, map[string]error) {
	if s.sendAnnounce == nil {
		return nil, map[string]error{"*": fmt.Errorf("the backpack is not running")}
	}
	return s.sendAnnounce(text, who)
}

// ── voice auditioning ────────────────────────────────────────────────────────

// setupVoiceTest renders a short sample in one voice and returns it as a WAV.
//
// Picking a voice from a list of names and one-line descriptions is guesswork:
// "measured female" tells you very little, and the first time a club actually
// hears their commentator is otherwise at a meeting, on the PA, in front of
// everyone. A few seconds of audio at setup time is the difference between
// choosing a voice and accepting one.
//
// It honours a key typed but NOT yet saved, so an operator can paste a key,
// audition voices, and only then commit — the natural order, and impossible if
// testing required saving first.
func (s *liveServer) setupVoiceTest(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	_ = r.ParseForm()

	voice := strings.TrimSpace(r.FormValue("voice"))

	// A "ref:" voice is one of the venue's own RECORDINGS, not a cloud voice
	// name. They share a dropdown now — which is the only arrangement that is
	// not confusing — so every consumer of that dropdown has to understand both.
	// This one did not, and sent a file path to Qwen as a voice name:
	//   Voice 'ref:/Users/…/voice-victory.wav' ... InvalidParameter
	if strings.HasPrefix(voice, "ref:") {
		pcm, rate, err := s.renderSpeech(r, voiceSampleFor(
			firstNonEmpty(r.FormValue("language"), s.boothCfg().Language)), voice)
		if err != nil {
			http.Error(w, "Could not speak with that recording: "+err.Error(),
				http.StatusInternalServerError)
			return
		}
		if rate <= 0 {
			rate = boothSampleRate
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(wavFromPCM(pcm, rate))
		return
	}
	if voice == "" {
		voice = qwentts.DefaultVoicePBP
	}

	bc := s.cfg.BoothConfig()
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		key = bc.QwenKey
	}
	if key == "" {
		http.Error(w, "Add your Qwen key first — then you can listen to the voices.",
			http.StatusPreconditionRequired)
		return
	}

	c, err := qwentts.New(qwentts.Config{APIKey: key, BaseURL: strings.TrimSpace(r.FormValue("base"))})
	if err != nil {
		http.Error(w, "Qwen: "+err.Error(), http.StatusBadGateway)
		return
	}

	// In the language the commentary will actually be in.
	//
	// A voice auditioned in English says nothing about how it calls a race in
	// Spanish — the rhythm, the number handling and the pronunciation all
	// change. Testing in English and discovering that at a meeting is the
	// failure this avoids.
	lang := strings.TrimSpace(r.FormValue("language"))
	if lang == "" {
		lang = bc.Language
	}
	sample := voiceSampleFor(lang)

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	// The SAME delivery instruction the booth will use.
	//
	// Without it the audition is a flat read and the real commentary sounds
	// different — you would choose a voice on evidence that does not apply.
	// Built from what is on the FORM, not from what is saved, so an operator
	// can turn the energy up and hear the difference before committing.
	st := boothpkg.Style{
		Theme:    firstNonEmpty(r.FormValue("theme"), bc.Theme),
		Humour:   firstNonEmpty(r.FormValue("humour"), bc.Humour),
		Energy:   firstNonEmpty(r.FormValue("energy"), bc.Energy),
		Language: lang,
		Accent:   boothpkg.SanitiseAccent(firstNonEmpty(r.FormValue("accent"), bc.Accent)),
	}
	characteristics := firstNonEmpty(r.FormValue("style"), bc.Style)

	// The FINISH call, at the emotion the booth genuinely uses for it.
	//
	// Auditioning on a mid-heat lap made every voice sound flat, because that
	// beat IS flat — it is one line about a lap time. The finish is where a
	// commentator is judged, and "big-finish" is the register the booth really
	// applies there, so this is the voice at its most animated without
	// inventing energy the meeting will not have.
	instruction := boothpkg.TTSInstructionStyled("big-finish", characteristics, true, st, "finish")

	// Language goes to the engine as well as into the words: Qwen takes a
	// language hint, and without it a voice can read foreign text with English
	// pronunciation rules.
	pcm, rate, err := tts.FromQwen(c).Speak(ctx, tts.Request{
		Text: sample, Voice: voice, Language: lang, Instruction: instruction,
	})
	if err != nil {
		http.Error(w, "Could not reach Qwen: "+err.Error(), http.StatusBadGateway)
		return
	}
	if rate <= 0 {
		rate = 24000
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(wavFromPCM(pcm, rate))
}

// voiceSampleFor is the audition line, in the language being auditioned.
//
// Deliberately a REAL race call with a callsign, a lap number and a time rather
// than a neutral sentence: reading figures at speed is most of what this voice
// will ever do, and it is where voices differ most.
//
// The translations are for the languages a voice engine actually speaks well
// enough to judge. Anything else falls back to English with the language hint
// still sent — an honest half-test, and better than pretending to audition a
// language the sample is not written in.
func voiceSampleFor(lang string) string {
	// A finish, not a lap time.
	//
	// The line has to give the voice somewhere to GO: a build, a turn, and a
	// call to land on. A single flat statement of a lap time made every voice
	// sound identical and dull, which told an operator nothing about which one
	// to pick. Callsigns and figures are kept in, because reading those cleanly
	// at speed is the thing actually being judged.
	samples := map[string]string{
		"Spanish":          "¡Y bob nueve cruza la meta! Once cuarenta y dos en la última vuelta — ¡le arrebata la victoria a Wizz por dos décimas! ¡Qué final!",
		"French":           "Et bob neuf franchit la ligne ! Onze quarante-deux dans le dernier tour — il souffle la victoire à Wizz pour deux dixièmes ! Quelle fin !",
		"German":           "Und bob neun über die Linie! Elf Komma vier zwei in der letzten Runde — er schnappt Wizz den Sieg um zwei Zehntel weg! Was für ein Finale!",
		"Italian":          "E bob nove taglia il traguardo! Undici e quarantadue nell'ultimo giro — strappa la vittoria a Wizz per due decimi! Che finale!",
		"Portuguese":       "E bob nove cruza a linha! Onze e quarenta e dois na última volta — rouba a vitória a Wizz por dois décimos! Que final!",
		"Dutch":            "En bob negen over de streep! Elf komma vier twee in de laatste ronde — hij pakt de winst van Wizz af met twee tienden! Wat een finale!",
		"Russian":          "И боб девять на финише! Одиннадцать сорок два на последнем круге — вырывает победу у Wizz на две десятых! Какой финал!",
		"Japanese":         "ボブ・ナイン、ゴール！ 最終ラップ11秒42 — ウィズをわずか2で逆転です！ すごいフィニッシュ！",
		"Korean":           "밥 나인, 결승선 통과! 마지막 랩 11초 42 — 위즈를 0.2초 차로 제쳤습니다! 대단한 피니시!",
		"Mandarin Chinese": "Bob nine 冲过终点！最后一圈十一秒四二 —— 以两个十分之一秒的优势力压 Wizz！精彩的收尾！",
		"Chinese":          "Bob nine 冲过终点！最后一圈十一秒四二 —— 以两个十分之一秒的优势力压 Wizz！精彩的收尾！",
	}
	if s, ok := samples[strings.TrimSpace(lang)]; ok {
		return s
	}
	return "And bob nine takes the flag! Eleven forty-two on the final lap — he snatches it from Wizz " +
		"by two tenths! What a finish!"
}

// firstNonEmpty returns the first value that has something in it, so a form
// field the operator has changed but not yet saved wins over the stored one.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// wavFromPCM wraps raw 16-bit mono PCM in a WAV header so a browser will play
// it without any client-side decoding.
func wavFromPCM(pcm []byte, rate int) []byte {
	var b bytes.Buffer
	n := len(pcm)
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+n))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))      // PCM
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))      // mono
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate))   //
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate*2)) // byte rate
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))      // block align
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))     // bits
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(n))
	b.Write(pcm)
	return b.Bytes()
}

// fileState reports whether a file dd-pits depends on is actually there.
//
// The point is to make a wrong path VISIBLE. Both files it checks fail
// silently — no race calls, no error — and "check the path" is not advice
// anyone can act on without being told which path and whether it is right now.
func fileState(path string) string {
	if path == "" {
		return `<span class="warn">not set</span>`
	}
	if _, err := os.Stat(path); err != nil {
		return `<span class="warn">not found</span>`
	}
	return `<span class="ok">found</span>`
}

// defaultTracksideDir is the assumed install location, shown as a placeholder
// so an operator can see what to match.
func defaultTracksideDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/Documents/FPVTrackside"
	}
	return filepath.Join(home, "Documents", "FPVTrackside")
}

// clonedVoices lists the voices enrolled on this Qwen account.
//
// Time-boxed hard: a slow or unreachable DashScope must cost the CLONED
// entries, not the settings page. An operator who cannot open the page cannot
// fix the key that is making it slow.
func clonedVoices(key, base string) []qwentts.ClonedVoice {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	c, err := qwentts.New(qwentts.Config{APIKey: key, BaseURL: base})
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := c.ListVoices(ctx)
	if err != nil {
		return nil
	}
	return out
}

// qwenVoiceOptions renders the voice picker.
//
// A free-text box asked an operator to know Qwen's voice names, which are not
// discoverable from anywhere they would look — and a wrong name is not a
// validation error, it is a booth that fails at the first beat of a meeting.
// The catalogue carries a description of each, so the choice can be made by
// what it sounds like rather than by recognising a name.
func qwenVoiceOptions(selected, fallback string, cloned []qwentts.ClonedVoice) string {
	if strings.TrimSpace(selected) == "" {
		selected = fallback
	}
	b := &strings.Builder{}

	// A club's OWN voice first: having gone to the trouble of cloning one,
	// hunting for it under twenty stock voices is a poor reward.
	if len(cloned) > 0 {
		b.WriteString(`<optgroup label="Your cloned voices">`)
		for _, v := range cloned {
			sel := ""
			if strings.EqualFold(v.ID, selected) {
				sel = " selected"
			}
			name := qwentts.ClonedVoiceName(v.ID)
			if name == "" {
				name = v.ID
			}
			fmt.Fprintf(b, `<option value="%s"%s>%s — your cloned voice</option>`, esc(v.ID), sel, esc(name))
		}
		b.WriteString(`</optgroup><optgroup label="Built-in voices">`)
	}

	for _, v := range qwentts.Voices {
		sel := ""
		if strings.EqualFold(v.Name, selected) {
			sel = " selected"
		}
		note := ""
		if strings.EqualFold(v.Name, fallback) {
			note = " (default)"
		}
		fmt.Fprintf(b, `<option value="%s"%s>%s — %s%s</option>`, esc(v.Name), sel, esc(v.Name), esc(v.Note), note)
	}
	if len(cloned) > 0 {
		b.WriteString(`</optgroup>`)
	}

	// A cloned voice that is selected but no longer on the account must still
	// be offered, or opening the page would silently reset a club's own
	// commentator to a stock voice.
	if qwentts.IsClonedVoice(selected) {
		found := false
		for _, v := range cloned {
			if strings.EqualFold(v.ID, selected) {
				found = true
			}
		}
		if !found {
			fmt.Fprintf(b, `<option value="%s" selected>%s — cloned (not found on this account)</option>`,
				esc(selected), esc(qwentts.ClonedVoiceName(selected)))
		}
	}
	return b.String()
}

// energyOptions renders the delivery-energy picker.
func energyOptions(selected string) string {
	b := &strings.Builder{}
	for _, o := range boothpkg.EnergyOptions {
		sel := ""
		if strings.EqualFold(o.Key, selected) {
			sel = " selected"
		}
		fmt.Fprintf(b, `<option value="%s"%s>%s</option>`, esc(o.Key), sel, esc(o.Label))
	}
	return b.String()
}

// languageOptions renders the language picker.
//
// Annotated by ENGINE, because the two backends do not speak the same set:
// Qwen handles ten and the rest need the local voice. Offering a language the
// chosen voice cannot say produces commentary that fails at the first beat of a
// meeting, which is the worst place to discover it.
func languageOptions(selected, engine string) string {
	qwenLangs := []string{
		"English", "Mandarin Chinese", "French", "German", "Russian",
		"Italian", "Spanish", "Portuguese", "Japanese", "Korean",
	}
	otherLangs := []string{
		"Dutch", "Swedish", "Norwegian", "Danish", "Finnish", "Polish", "Czech",
		"Greek", "Turkish", "Arabic", "Hindi", "Indonesian", "Thai", "Vietnamese",
		"Māori", "Welsh", "Irish",
	}

	b := &strings.Builder{}
	selEnglish := ""
	if strings.TrimSpace(selected) == "" || strings.EqualFold(selected, "English") {
		selEnglish = " selected"
	}
	fmt.Fprintf(b, `<option value=""%s>English (default)</option>`, selEnglish)

	opt := func(name, note string) {
		sel := ""
		if strings.EqualFold(name, selected) {
			sel = " selected"
		}
		fmt.Fprintf(b, `<option value="%s"%s>%s%s</option>`, esc(name), sel, esc(name), note)
	}

	b.WriteString(`<optgroup label="Spoken by any voice engine">`)
	for _, l := range qwenLangs {
		if strings.EqualFold(l, "English") {
			continue
		}
		opt(l, "")
	}
	b.WriteString(`</optgroup>`)

	label, note := "Local voice only", ""
	if strings.EqualFold(engine, "qwen") {
		label = "Not spoken by Qwen — switch to the local voice for these"
		note = " (needs the local voice)"
	}
	fmt.Fprintf(b, `<optgroup label="%s">`, label)
	for _, l := range otherLangs {
		opt(l, note)
	}
	b.WriteString(`</optgroup>`)

	// A language chosen before and not in either list must survive a reload.
	if selected != "" && !strings.EqualFold(selected, "English") {
		known := false
		for _, l := range append(append([]string{}, qwenLangs...), otherLangs...) {
			if strings.EqualFold(l, selected) {
				known = true
			}
		}
		if !known {
			fmt.Fprintf(b, `<option value="%s" selected>%s</option>`, esc(selected), esc(selected))
		}
	}
	return b.String()
}

// themeOptions / humourOptions render the personality pickers.
//
// Same shape as energyOptions. These live here rather than as plain strings in
// the template because the catalogues are owned by the booth package — a new
// personality should appear on this page without anyone editing markup.
func themeOptions(selected string) string {
	b := &strings.Builder{}
	for _, t := range boothpkg.Themes {
		sel := ""
		if strings.EqualFold(t.Key, selected) {
			sel = " selected"
		}
		fmt.Fprintf(b, `<option value="%s"%s>%s</option>`, esc(t.Key), sel, esc(t.Name))
	}
	return b.String()
}

func humourOptions(selected string) string {
	b := &strings.Builder{}
	for _, o := range boothpkg.HumourOptions {
		sel := ""
		if strings.EqualFold(o.Key, selected) {
			sel = " selected"
		}
		fmt.Fprintf(b, `<option value="%s"%s>%s</option>`, esc(o.Key), sel, esc(o.Name))
	}
	return b.String()
}

// demoMode makes the whole agent read-only, for a public demo: every setting
// page still renders, but nothing can be saved, installed or restarted.
var demoMode bool

func demoBanner() string {
	if !demoMode {
		return ""
	}
	return `<div class="card" style="border-color:rgba(198,255,61,.35);background:rgba(198,255,61,.08)"><b>Read-only demo.</b>
<span class="mut">A real dd-pits fed by a simulated meeting. Look around: nothing here can be changed.
<a href="https://github.com/bob9/dd-pits/releases">Get dd-pits</a> to run it at your venue.</span></div>`
}

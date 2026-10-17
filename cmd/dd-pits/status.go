package main

// The Status page (/status): what dd-pits is doing right now, in plain words,
// with the live log underneath. It's where the browser opens when dd-pits
// starts, so a race director sees at once whether the timer and the goggle
// backpack are connected and what race is on, and can quit from there.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

func (s *liveServer) registerStatus(mux *http.ServeMux) {
	mux.HandleFunc("/status", s.statusPage)
	mux.HandleFunc("/status.json", s.statusJSON)
	mux.HandleFunc("/status/quit", s.statusQuit)
}

type statusItem struct {
	Name  string `json:"name"`
	State string `json:"state"` // ok, warn, off
	Text  string `json:"text"`
}

type statusDoc struct {
	Items   []statusItem `json:"items"`
	Log     []string     `json:"log"`
	LogFile string       `json:"logFile"`
	Up      string       `json:"up"`
}

var startedAt = time.Now()

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
}

func (s *liveServer) status() statusDoc {
	var items []statusItem

	// The timer.
	ok, lastOK, lastErr, since := timerStatus.get()
	addr := s.tracksideURL
	switch {
	case ok:
		items = append(items, statusItem{"Timer", "ok", fmt.Sprintf("Connected to %s at %s, since %s.", timingSystem, addr, ago(since))})
	case lastErr == "":
		items = append(items, statusItem{"Timer", "warn", fmt.Sprintf("Looking for %s at %s…", timingSystem, addr)})
	default:
		txt := fmt.Sprintf("Can't reach %s at %s: %s.", timingSystem, addr, plainTimerError(lastErr))
		if !lastOK.IsZero() {
			txt += " Last connected " + ago(lastOK) + "."
		} else if timingSystem == "FPVTrackside" {
			txt += " Is FPVTrackside open, with an event open and its web server switched on?"
		}
		items = append(items, statusItem{"Timer", "warn", txt})
	}

	// The race.
	if mv, _, have := s.meeting(); have && mv != nil {
		race := "No race on the line."
		if mv.Current != nil {
			race = fmt.Sprintf("On the line: %s (%d pilots).", mv.Current.Label, len(mv.Current.Pilots))
		}
		if len(mv.Upcoming) > 0 {
			race += fmt.Sprintf(" Up next: %s.", mv.Upcoming[0].Label)
		}
		items = append(items, statusItem{"Meeting", "ok", fmt.Sprintf("%s · %d races. %s", mv.Name, len(mv.Races), race)})
	} else {
		items = append(items, statusItem{"Meeting", "off", "No meeting read from the timer yet."})
	}

	// The goggle backpack.
	switch {
	case s.backpackStatus == nil:
		items = append(items, statusItem{"Goggle backpack", "off", "Not started. Lap times in goggles need it: Settings → Backpack."})
	case s.backpackStatus():
		items = append(items, statusItem{"Goggle backpack", "ok", "Connected: pilots get lap times, race channels and DVR names."})
	default:
		items = append(items, statusItem{"Goggle backpack", "warn", "Started but not connected. Check the netpack or USB backpack."})
	}

	// Pilots.
	n, osd := 0, 0
	for _, p := range s.cfg.PilotList() {
		if p.resolvedUID() != "" {
			n++
			if p.OSDEnabled {
				osd++
			}
		}
	}
	if n == 0 {
		items = append(items, statusItem{"Goggle pilots", "off", "None set up yet: Settings → Goggle pilots."})
	} else {
		items = append(items, statusItem{"Goggle pilots", "ok", fmt.Sprintf("%d set up, %d with lap times on.", n, osd)})
	}

	doc := statusDoc{Items: items, LogFile: appLogPath, Up: ago(startedAt)}
	if s.logs != nil {
		lines := s.logs.Lines()
		if len(lines) > 300 {
			lines = lines[len(lines)-300:]
		}
		doc.Log = lines
	}
	return doc
}

func (s *liveServer) statusJSON(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.status())
}

func (s *liveServer) statusQuit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	writeSetup(w, r, "Status", `<div class="card"><h2>dd-pits has stopped</h2>
<p>You can close this tab. Start dd-pits again to pick up where you left off.</p></div>`)
	if s.quit != nil {
		go func() {
			time.Sleep(400 * time.Millisecond) // let the page reach the browser
			s.quit()
		}()
	}
}

func (s *liveServer) statusPage(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	logFile := `<span class="mut">no log file</span>`
	if appLogPath != "" {
		logFile = "<code>" + esc(appLogPath) + "</code>"
	}
	quit := ""
	if s.quit != nil {
		quit = `<form method="post" action="/status/quit" data-full onsubmit="return confirm('Stop dd-pits? The dashboard, overlays, calls and goggles stop until you start it again.')"><button class="sec">Quit dd-pits</button></form>`
	}
	body := fmt.Sprintf(`<div class="card st-head"><h2>dd-pits is running</h2>
<p class="mut">Started <span id="stUp">just now</span> · %s · %s/%s. This page updates by itself.</p>
<div class="row st-actions"><a href="/"><button>Open the dashboard</button></a><a href="/setup"><button class="sec">Settings</button></a>%s</div></div>
<div class="card"><h2>Right now</h2><div id="stItems"></div></div>
<div class="card"><h2>What's happening</h2><p class="mut">The latest from the log, newest at the bottom. Also saved to %s. <a href="/setup/debug.txt" download="dd-pits-log.txt">Download it</a></p>
<pre id="stLog" class="st-log"></pre></div>
<style>
.st-actions{gap:.5rem;flex-wrap:wrap;justify-content:flex-start}.st-actions>*{flex:0 0 auto!important;width:auto!important}.st-actions form{margin:0}
.st-item{display:flex;gap:.8rem;align-items:flex-start;padding:.55rem 0;border-bottom:1px solid rgba(255,255,255,.06)}
.st-item:last-child{border-bottom:0}
.st-dot{flex:0 0 12px;height:12px;border-radius:50%%;margin-top:.35rem;background:#555}
.st-dot.ok{background:#c6ff3d;box-shadow:0 0 10px rgba(198,255,61,.6)}.st-dot.warn{background:#ffb340;box-shadow:0 0 10px rgba(255,179,64,.5)}
.st-item b{display:block}.st-item span{color:#b8c0cc;font-size:.92rem}
.st-log{max-height:52vh;overflow:auto;background:#07090c;border:1px solid rgba(255,255,255,.08);border-radius:10px;padding:.8rem;font-size:.78rem;line-height:1.45;white-space:pre-wrap;word-break:break-word;color:#cfd4dc}
</style>
<script>
(function(){
  var items=document.getElementById('stItems'), logEl=document.getElementById('stLog'), up=document.getElementById('stUp');
  function esc(t){return String(t).replace(/[&<>]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;'}[c];});}
  function tick(){
    fetch('/status.json',{credentials:'same-origin',cache:'no-store'}).then(function(r){return r.json();}).then(function(d){
      items.innerHTML=d.items.map(function(i){return '<div class="st-item"><i class="st-dot '+i.state+'"></i><div><b>'+esc(i.name)+'</b><span>'+esc(i.text)+'</span></div></div>';}).join('');
      var atEnd=logEl.scrollHeight-logEl.scrollTop-logEl.clientHeight<40;
      logEl.textContent=(d.log||[]).join('\n');
      if(atEnd) logEl.scrollTop=logEl.scrollHeight;
      up.textContent=d.up;
    }).catch(function(){ items.innerHTML='<div class="st-item"><i class="st-dot warn"></i><div><b>dd-pits</b><span>Not answering. If you quit it, start it again.</span></div></div>'; });
  }
  tick(); setInterval(tick,2000);
})();
</script>`, esc("dd-pits "+version), runtime.GOOS, runtime.GOARCH, quit, logFile)
	writeSetup(w, r, "Status", body)
}

// shouldOpenBrowser: dd-pits opens a browser when it starts, unless told not
// to, started by the RotorHazard plugin (which runs it on the timer), or on a
// machine with no screen.
func shouldOpenBrowser(noBrowser bool, startedByRotorHazard bool) bool {
	if noBrowser || startedByRotorHazard {
		return false
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return false
	}
	return true
}

// openOnStart opens the setup help on a first run, otherwise the Status page.
func openOnStart(configPath, base string) {
	page := "/status"
	if isFirstRun(configPath) {
		markFirstRunSeen(configPath)
		page = "/setup/help"
	}
	url := base + page
	log.Printf("dd-pits: opening %s", url)
	go func() {
		time.Sleep(1500 * time.Millisecond) // after the server is listening
		openBrowser(url)
	}()
}

// plainTimerError says in plain words why the timer can't be read; the full
// error stays in the log.
func plainTimerError(e string) string {
	l := strings.ToLower(e)
	switch {
	case strings.Contains(l, "connection refused"):
		return "nothing is answering there (not running, or its web server is off)"
	case strings.Contains(l, "no such host"):
		return "that address doesn't exist on this network"
	case strings.Contains(l, "timeout") || strings.Contains(l, "deadline exceeded"):
		return "it isn't answering in time"
	case strings.Contains(l, "no event") || strings.Contains(l, "event"):
		return "it's running but no event is open"
	}
	return "see the log below"
}

// localServeURL turns a listen address (":8088", "127.0.0.1:8088") into a URL
// on this machine.
func localServeURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return "http://" + addr
}

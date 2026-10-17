package main

// The one menu every dd-pits page shares — the dashboard pages and the setup,
// bracket, overlay and marshal pages alike — so moving between them never
// feels like landing in a different app.
//
//	Home · Live · Upcoming · Races · Brackets · Overlays ·
//	Race director ▾ · Settings ▾ · RotorHazard ↗
//
// The first six are for everyone at the event; the two menus are the race
// director's tools and the venue's settings.

import (
	"fmt"
	"strings"
)

type navItem struct{ href, label, hint string }

// siteNav is the menu's links, for a <nav class="site">. The race
// director's tools and the settings are only offered to someone who can use
// them; everyone else can log in.
func siteNav(admin bool) string {
	b := &strings.Builder{}
	for _, it := range []navItem{
		{"/", "Home", ""},
		{"/live", "Live", ""},
		{"/dronedash/upcoming", "Upcoming", ""},
		{"/dronedash/races", "Races", ""},
		{"/stages", "Brackets", ""},
		{"/overlays", "Overlays", ""},
	} {
		fmt.Fprintf(b, `<a href="%s">%s</a>`, it.href, esc(it.label))
	}
	group := func(name string, items ...navItem) {
		fmt.Fprintf(b, `<details class="ng"><summary>%s</summary><div class="menu">`, esc(name))
		for _, it := range items {
			fmt.Fprintf(b, `<a href="%s">%s<small>%s</small></a>`, it.href, esc(it.label), esc(it.hint))
		}
		b.WriteString(`</div></details>`)
	}
	if !admin {
		if !demoMode {
			b.WriteString(`<a href="/login">Log in</a>`)
		}
		rhLink(b)
		return b.String()
	}
	director := []navItem{{"/stages/new", "Create bracket", "Draw a bracket from your pilots"}}
	if timingSystem == "FPVTrackside" {
		director = append(director, navItem{"/meeting", "Meeting editor", "Rounds, races, line-ups, pilots, event"})
	}
	director = append(director,
		navItem{"/marshal", "Marshal", "Fix laps from each pilot's signal"},
		navItem{"/calls", "Race calls", "Say a call now"},
		navItem{"/goggles", "Goggle control", "Channels and messages to goggles"},
		navItem{"/logout", "Log out", "View as a spectator: no race controls"},
	)
	group("Race director", director...)
	settings := []navItem{
		{"/setup", "Overview", "What this venue is running"},
		{"/setup/pilots", "Goggle pilots", "Bind phrases, OSD on or off"},
		{"/setup/osd", "OSD screen", "What pilots see in their goggles"},
		{"/setup/backpack", "Backpack", "ELRS netpack or USB backpack"},
		{"/setup/booth", "Commentary", "AI play-by-play and colour"},
		{"/setup/calls", "Race call voice", "Who speaks the lap times"},
		{"/setup/video", "Video", "Watch links and chapters"},
	}
	if timingSystem == "FPVTrackside" {
		settings = append(settings, navItem{"/setup/trackside", "FPVTrackside", "Where it is, what dd-pits reads"})
	}
	settings = append(settings,
		navItem{"/setup/models", "AI models", "Local voice and writing models"},
		navItem{"/setup/debug", "Debug", "The log, for when something goes wrong"},
		navItem{"/setup/help", "Help", "How dd-pits works"},
	)
	group("Settings", settings...)
	rhLink(b)
	return b.String()
}

func rhLink(b *strings.Builder) {
	if timingSystem == "RotorHazard" {
		// Its address is worked out in the browser (siteNavJS): this same
		// site behind a proxy, else this host on RotorHazard's port.
		fmt.Fprintf(b, `<a class="rhlink" data-port="%s" target="_self">RotorHazard ↗</a>`, esc(rotorhazardPort))
	}
}

// siteNavCSS styles nav.site the same on every page. Selectors carry the
// class twice so they win over each page's own nav rules.
const siteNavCSS = `<style>
nav.site.site{display:flex;align-items:center;gap:.15rem;flex-wrap:wrap;margin-left:auto}
nav.site.site>a,nav.site.site .ng>summary{border:1px solid transparent;border-radius:999px;padding:.38rem .8rem;color:#cfd4dc;
 font-weight:600;font-size:.86rem;text-decoration:none;cursor:pointer;transition:background .15s,color .15s;white-space:nowrap}
nav.site.site>a:hover,nav.site.site .ng>summary:hover,nav.site.site .ng[open]>summary{background:rgba(255,255,255,.07);color:#fff}
nav.site.site>a.on,nav.site.site .ng.on>summary{background:rgba(198,255,61,.14);color:#c6ff3d}
nav.site.site .rhlink{color:#9fb4c8}
nav.site .ng{position:relative}
nav.site .ng>summary{list-style:none;user-select:none}
nav.site .ng>summary::-webkit-details-marker{display:none}
nav.site .ng>summary::after{content:"▾";margin-left:.35rem;font-size:.7rem;opacity:.7}
nav.site .ng .menu{position:absolute;top:calc(100% + .35rem);right:0;z-index:30;min-width:14rem;padding:.4rem;display:grid;gap:.1rem;
 background:rgba(14,16,20,.98);border:1px solid rgba(255,255,255,.09);border-radius:16px;box-shadow:0 24px 50px -20px rgba(0,0,0,.9)}
nav.site.site .ng .menu a{display:block;padding:.5rem .7rem;border-radius:10px;white-space:nowrap;color:#e6e9ee;text-decoration:none;font-weight:600;font-size:.88rem;border:0}
nav.site.site .ng .menu a:hover{background:rgba(255,255,255,.07)}
nav.site.site .ng .menu a.on{background:rgba(198,255,61,.14);color:#c6ff3d}
nav.site .ng .menu a small{display:block;color:#8e96a3;font-weight:500;font-size:.75rem;margin-top:.05rem}
@media(max-width:760px){
 .top.top:has(nav.site){flex-wrap:wrap}
 .top.top:has(nav.site),header:has(nav.site){position:static}
 nav.site.site{flex-basis:100%;order:10;margin-left:0;gap:.1rem;flex-wrap:wrap;overflow:visible}
 nav.site.site>a,nav.site.site .ng>summary{padding:.3rem .55rem;font-size:.82rem}
 nav.site .ng .menu{position:fixed;left:.8rem;right:.8rem;top:auto}}
</style>`

// siteNavJS marks where you are, keeps one menu open at a time, and points
// the RotorHazard link at the timer (through the plugin, which picks the web
// app or the classic interface, whichever this browser uses).
const siteNavJS = `<script>(function(){
 var nav=document.querySelector('nav.site'); if(!nav) return;
 var path=location.pathname.replace(/\/+$/,'')||'/', best=null, bestLen=-1;
 nav.querySelectorAll('a[href]').forEach(function(a){
  var h=(a.getAttribute('href')||'').replace(/\/+$/,'')||'/';
  if(h===path){ best=a; bestLen=1e9; return; }
  if(h!=='/'&&path.indexOf(h+'/')===0&&h.length>bestLen){ best=a; bestLen=h.length; }
 });
 if(best){ best.classList.add('on'); var g=best.closest('.ng'); if(g) g.classList.add('on'); }
 document.addEventListener('click',function(e){ nav.querySelectorAll('.ng[open]').forEach(function(d){ if(!d.contains(e.target)) d.open=false; }); });
 nav.querySelectorAll('.ng').forEach(function(d){ d.addEventListener('toggle',function(){ if(d.open) nav.querySelectorAll('.ng[open]').forEach(function(o){ if(o!==d) o.open=false; }); }); });
 // Log in or out and come back to this same page.
 nav.querySelectorAll('a[href$="/logout"],a[href$="/login"]').forEach(function(a){
  a.href=a.getAttribute('href')+'?next='+encodeURIComponent(location.pathname+location.search+location.hash);
 });
 var rh=nav.querySelector('.rhlink');
 if(rh){
  // Home's link carries the sub-path dd-pits is served under (e.g. /pits/):
  // behind a proxy RotorHazard is the rest of this site.
  var home=(nav.querySelector('a[href]').getAttribute('href')||'/');
  var origin=location.origin;
  if(home==='/'&&rh.dataset.port&&location.port!==rh.dataset.port) origin=location.protocol+'//'+location.hostname+':'+rh.dataset.port;
  rh.href=origin+'/dd'+'-pits/open';
 }
})();</script>`

package main

// The stage (bracket) pages.
//
//	/stages              every bracket; "Create bracket" for the race director
//	/stages/new          set one up: format, options, pilots in seed order
//	/stages/<id>         the bracket: rounds, races, results, standings
//	/stages/<id>.json    the same, for scripts and overlays
//
// Anyone on the venue network can watch a stage; changing one needs the same
// permission as the setup pages (this machine, or the setup PIN).

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/bob9/dd-pits/internal/stages/raceformat"
)

func (s *liveServer) registerStages(mux *http.ServeMux) {
	if s.stages == nil {
		return
	}
	mux.HandleFunc("/stages", s.stagesList)
	mux.HandleFunc("/stages/", s.stageRoute)
}

func (s *liveServer) stageRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/stages/")
	switch {
	case rest == "":
		http.Redirect(w, r, "/stages", http.StatusSeeOther)
	case rest == "new":
		s.stageNew(w, r)
	case rest == "find":
		s.stageFind(w, r)
	case rest == "links":
		s.stageLinks(w, r)
	case rest == "preview":
		s.stagePreview(w, r)
	case strings.HasSuffix(rest, ".json"):
		s.stageJSON(w, r, strings.TrimSuffix(rest, ".json"))
	case strings.HasSuffix(rest, "/action"):
		s.stageAction(w, r, strings.TrimSuffix(rest, "/action"))
	default:
		s.stageView(w, r, rest)
	}
}

func (s *liveServer) stagesList(w http.ResponseWriter, r *http.Request) {
	admin := localSetupAllowed(r)
	b := &strings.Builder{}
	b.WriteString(loginBar(r))
	list := s.stages.list()
	if admin {
		b.WriteString(`<div class="card"><h2>Run a bracket</h2>
<p>Pick a format and the pilots in seed order, and dd-pits draws the races. Each race goes to ` + esc(timingSystem) + ` as soon as its line-up is known,
and the results come back from the timing, so the next races fill in on their own.</p>
<p><a href="/stages/new"><button>Create bracket</button></a></p></div>`)
	}
	if len(list) == 0 {
		b.WriteString(`<div class="card"><p class="mut">No brackets yet.</p></div>`)
	}
	for i := len(list) - 1; i >= 0; i-- {
		st := list[i]
		races, f, err := s.stages.races(st, s.currentMeeting())
		status := ""
		if err != nil {
			status = `<span class="warn">` + esc(err.Error()) + `</span>`
		} else {
			done := 0
			for _, r := range races {
				if r.State == "finished" || r.State == "void" {
					done++
				}
			}
			status = fmt.Sprintf("%d of %d races run", done, len(races))
			if c := f.Champion(); c != "" {
				status += " · winner <strong>" + esc(c) + "</strong>"
			}
		}
		fmt.Fprintf(b, `<div class="card"><h2><a href="/stages/%s">%s</a></h2><p class="mut">%s · %d pilots · %s</p></div>`,
			esc(st.ID), esc(st.Name), esc(formatLabel(st.Format)), len(st.Pilots), status)
	}
	writeSetup(w, r, "Brackets", b.String())
}

func formatLabel(k raceformat.FormatKind) string {
	for _, f := range stageFormats {
		if f.Kind == k {
			return f.Label
		}
	}
	return string(k)
}

func (s *liveServer) currentMeeting() *meetingView {
	mv, _, ok := s.meeting()
	if !ok {
		return nil
	}
	return mv
}

// ── new stage ────────────────────────────────────────────────────────────────

func atoiOr(v string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return n
	}
	return def
}

// stageForm reads the create page's format, options and seeded pilots.
func stageForm(r *http.Request) (raceformat.FormatKind, stageSetup, []string) {
	kind := raceformat.FormatKind(r.FormValue("format"))
	setup := stageSetup{
		EliminationType: r.FormValue("eliminationType"),
		FinalsFormat:    r.FormValue("finalsFormat"),
		PilotsPerRace:   atoiOr(r.FormValue("pilotsPerRace"), 4),
		PilotsToAdvance: atoiOr(r.FormValue("pilotsToAdvance"), 2),
		AceWinsRequired: atoiOr(r.FormValue("aceWinsRequired"), 2),
		QualifierCTA:    r.FormValue("qualifierCta") == "on",
		QualifierSeed:   r.FormValue("qualifierSeed"),
		PilotsPerMain:   atoiOr(r.FormValue("pilotsPerMain"), 16),
		BumpUp:          atoiOr(r.FormValue("bumpUp"), 4),
		Mains:           atoiOr(r.FormValue("mains"), 0),
		Rounds:          atoiOr(r.FormValue("rounds"), 3),
		Direction:       r.FormValue("direction"),
		DropWorst:       atoiOr(r.FormValue("dropWorst"), 0),
		Laps:            atoiOr(r.FormValue("laps"), 0),
	}
	if secs, err := parseDuration(r.FormValue("raceTime")); err == nil {
		setup.RaceSeconds = secs
	}
	if r.FormValue("ctaAllMains") == "on" {
		setup.CTAMains = []string{"*"}
	}
	for _, p := range strings.Split(r.FormValue("pointsTable"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			setup.PointsTable = append(setup.PointsTable, n)
		}
	}
	pilots := strings.Split(strings.ReplaceAll(r.FormValue("pilots"), "\r", ""), "\n")
	return kind, setup, pilots
}

// stagePreview draws the bracket the create page would make, without
// saving it: every race in every round, round 1 seated, later rounds
// filled in as results come.
func (s *liveServer) stagePreview(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	kind, setup, pilots := stageForm(r)
	st, err := draftStage(r.FormValue("name"), kind, pilots, setup, false)
	var races []stageRace
	if err == nil {
		races, _, err = s.stages.races(*st, nil)
	}
	if err != nil {
		msg := strings.TrimPrefix(err.Error(), "raceformat: ")
		fmt.Fprintf(w, `<p class="warn">Can't draw this bracket: %s</p>`, esc(msg))
		return
	}
	rounds := map[int]bool{}
	n := 0
	for _, rc := range races {
		if !rc.IsVoid {
			n++
			rounds[rc.Round] = true
		}
	}
	plural := func(k int, one string) string {
		if k == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", k, one)
	}
	fmt.Fprintf(w, `<p class="mut">%s · %s · %s, up to %s. Round 1 is seeded now; later races fill in as results come in.</p>`,
		esc(formatLabel(kind)), plural(len(st.Pilots), "pilot"), plural(n, "race"), plural(len(rounds), "round"))
	shown := races[:0]
	for _, rc := range races {
		if rc.IsVoid {
			continue
		}
		rc.State = "pv-later"
		if len(rc.Pilots) > 0 {
			rc.State = "pv-seeded"
		}
		shown = append(shown, rc)
	}
	b := &strings.Builder{}
	s.bracketGrid(b, *st, shown, false, nil, false)
	_, _ = w.Write([]byte(b.String()))
}

func (s *liveServer) stageNew(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	errMsg := ""
	if r.Method == http.MethodPost {
		kind, setup, pilots := stageForm(r)
		st, err := s.stages.createInBackground(r.FormValue("name"), kind, pilots, setup, r.FormValue("autoSend") == "on")
		if err == nil {
			http.Redirect(w, r, "/stages/"+st.ID, http.StatusSeeOther)
			return
		}
		errMsg = err.Error()
	}

	seeded := seedingFromMeeting(s.currentMeeting())
	seedJSON, _ := json.Marshal(seeded)
	classes := s.seedClasses()
	classJSON, _ := json.Marshal(classes)

	b := &strings.Builder{}
	if errMsg != "" {
		fmt.Fprintf(b, `<div class="card"><p class="warn">Couldn't create the bracket: %s</p></div>`, esc(errMsg))
	}
	b.WriteString(`<form method="post" id="stageform">
<div class="card"><h2>1 · Format</h2><div class="fmts">`)
	for i, f := range stageFormats {
		checked := ""
		if i == 0 {
			checked = " checked"
		}
		fmt.Fprintf(b, `<label class="fmt"><input type="radio" name="format" value="%s"%s><b>%s</b><span>%s</span></label>`,
			esc(string(f.Kind)), checked, esc(f.Label), esc(f.What))
	}
	b.WriteString(`</div></div>

<div class="card"><h2>2 · Pilots, top seed first</h2>`)
	if len(classes) > 0 {
		b.WriteString(`<div class="row"><div><label>Seed from</label><select id="seedfrom">`)
		for _, c := range classes {
			fmt.Fprintf(b, `<option value="%d">%s · %d pilots</option>`, c.ID, esc(c.Name), len(c.Pilots))
		}
		b.WriteString(`<option value="">The whole meeting (dd-pits leaderboard)</option></select></div></div>`)
	}
	b.WriteString(`<p class="mut" id="seednote">Seeded from this meeting's results so far: best consecutive laps, then laps flown.</p>
<p class="mut">Drag to reorder, untick to leave a pilot out, or add pilots below.</p>
<ol id="seedlist" class="seedlist"></ol>
<div class="row"><div><label>Add pilots (one per line)</label><textarea id="addp" rows="2" placeholder="Callsign"></textarea></div></div>
<p><button type="button" class="sec" id="addbtn">Add</button> <span class="mut" id="count"></span></p>
<input type="hidden" name="pilots" id="pilots">
</div>

<div class="card"><h2>3 · Options</h2>
<div class="row">
 <div><label>Name</label><input type="text" name="name" value="Finals" required></div>
 <div data-for="elimination ladder qualifier mainsprogression"><label>Pilots per race</label><input type="number" name="pilotsPerRace" value="4" min="2" max="8"></div>
 <div data-for="elimination ladder"><label>Pilots advancing per race</label><input type="number" name="pilotsToAdvance" value="2" min="1" max="7"></div>
</div>
<div class="row" data-for="elimination">
 <div><label>Elimination</label><select name="eliminationType"><option value="double">Double elimination</option><option value="single">Single elimination</option></select></div>
 <div><label>Finals</label><select name="finalsFormat"><option value="chase-the-ace">Chase the ace</option><option value="double-elimination">Grand final</option></select></div>
 <div><label>Chase-the-ace wins needed</label><input type="number" name="aceWinsRequired" value="2" min="1" max="5"></div>
</div>
<div class="row" data-for="qualifier mainsprogression">
 <div><label>Round-1 seeding (16 pilots)</label><select name="qualifierSeed"><option value="snake">MultiGP snake</option><option value="photo">Standard seed sheet</option></select></div>
 <div><label><input type="checkbox" name="qualifierCta"> Run the final as chase the ace</label></div>
</div>
<div class="row" data-for="mainsprogression">
 <div><label>Pilots per bracket</label><input type="number" name="pilotsPerMain" value="16" min="4"></div>
 <div><label>Move up per bracket</label><input type="number" name="bumpUp" value="4" min="1"></div>
 <div><label><input type="checkbox" name="ctaAllMains"> Chase the ace in every bracket (not just A)</label></div>
</div>
<div class="row" data-for="mains">
 <div><label>Brackets (0 = one per 8 pilots)</label><input type="number" name="mains" value="0" min="0" max="26"></div>
 <div><label>Rounds</label><input type="number" name="rounds" value="3" min="1"></div>
 <div><label>Points</label><select name="direction"><option value="descending">High points win</option><option value="ascending">Low points win</option></select></div>
</div>
<div class="row" data-for="mains">
 <div><label>Points table (1st, 2nd, …)</label><input type="text" name="pointsTable" value="25,18,15,12,10,8,6,4"></div>
 <div><label>Drop each pilot's worst rounds</label><input type="number" name="dropWorst" value="0" min="0"></div>
</div>
<div class="row">
 <div><label>Laps per race (first to finish them wins; 0 = the timer's format)</label><input type="number" name="laps" value="3" min="0" max="99"></div>
 <div><label>Race time, e.g. 2:00 (0 = no time limit)</label><input type="text" name="raceTime" value="0" inputmode="numeric"></div>
</div>
<p class="mut">` + raceSettingsNote() + `</p>
<p><label><input type="checkbox" name="autoSend" checked> Send each race to ` + esc(timingSystem) + ` as soon as its line-up is known</label></p>
</div>
<div class="card pvcard"><h2>4 · Preview <span class="mut pvsub">what will be created · updates as you change things</span></h2><div id="preview"><p class="mut">Drawing the bracket…</p></div></div>
<p><button type="submit">Create bracket</button> <a href="/stages" class="mut">Cancel</a></p>
</form>` + stageCSS + `
<style>
 .fmts{display:grid;grid-template-columns:repeat(auto-fit,minmax(15rem,1fr));gap:.7rem}
 .fmt{display:block;margin:0;padding:.85rem 1rem;border:1px solid var(--line);border-radius:16px;cursor:pointer;background:rgba(0,0,0,.25)}
 .fmt input{margin-right:.4rem}.fmt b{color:var(--fg)}.fmt span{display:block;color:var(--mut);font-size:.84rem;margin-top:.25rem}
 .fmt:has(input:checked){border-color:var(--acc);box-shadow:0 0 0 3px rgba(198,255,61,.15)}
 .seedlist{list-style:none;padding:0;margin:.4rem 0;display:grid;gap:.3rem;counter-reset:seed}
 .seedlist li{display:flex;align-items:center;gap:.6rem;padding:.45rem .7rem;border:1px solid var(--line);border-radius:12px;background:rgba(0,0,0,.3);cursor:grab}
 .seedlist li.off{opacity:.45}.seedlist li .n{color:var(--acc);font-weight:700;min-width:2.2rem}
 .seedlist li .nm{flex:1}.seedlist li button{padding:.15rem .55rem;font-size:.8rem}
 .seedlist li.drag{opacity:.4}
 #preview .psec+.psec{margin-top:1.1rem;padding-top:1rem;border-top:1px solid var(--line)}
 .pvsub{font-size:.8rem;font-weight:500;margin-left:.4rem}
 /* The preview's rounds stack top to bottom, each a row of its races. */
 #preview .grid{flex-direction:column;overflow:visible;gap:1rem}
 #preview .col{flex:none;min-width:0;display:grid;grid-template-columns:repeat(auto-fill,minmax(12.5rem,1fr));gap:.6rem}
 #preview .colh{grid-column:1/-1;padding-bottom:.2rem;border-bottom:1px solid var(--line)}
 /* Wide screens: it sits beside the form and stays in view. */
 @media(min-width:1200px){
  main:has(#stageform){max-width:100rem}
  #stageform{display:grid;grid-template-columns:minmax(0,34rem) minmax(0,1fr);gap:0 1.2rem;align-items:start}
  #stageform>*{grid-column:1}
  #stageform>.pvcard{grid-column:2;grid-row:1 / span 20;position:sticky;top:4.5rem;max-height:calc(100vh - 5.5rem);overflow:auto;margin:0}
 }
 #preview .psec h2{font-size:1rem;margin:0 0 .6rem}
 #preview.busy{opacity:.6;transition:opacity .2s}
</style>
<script>
(function(){
 const seeded = ` + string(seedJSON) + `;
 const classes = ` + string(classJSON) + `;
 const list = document.getElementById('seedlist');
 let items = seeded.map(n => ({name:n, on:true}));
 // Seeding from a RotorHazard class: its pilots in RotorHazard's ranking,
 // everyone else in the meeting listed after them, left out.
 const meetingNote = document.getElementById('seednote').textContent;
 function seedFrom(id){
  const c = classes.find(x => String(x.id) === String(id));
  const note = document.getElementById('seednote');
  if(!c){ items = seeded.map(n => ({name:n, on:true})); note.textContent = meetingNote; draw(); return; }
  const inClass = new Set(c.pilots.map(n => n.toLowerCase()));
  items = c.pilots.map(n => ({name:n, on:true})).concat(seeded.filter(n => !inClass.has(n.toLowerCase())).map(n => ({name:n, on:false})));
  note.textContent = c.ranked
    ? 'Seeded from RotorHazard\'s ' + c.name + ' results, ranked by ' + c.by + '.' + (c.pilots.length > c.ranked ? ' Pilots without a result yet come after.' : '')
    : c.name + ' has no results yet, so its pilots are in heat order.';
  const name = document.querySelector('input[name=name]');
  if(name && (name.value === 'Finals' || name.dataset.auto)){ name.value = c.name + ' Bracket'; name.dataset.auto = '1'; }
  draw();
 }
 const sf = document.getElementById('seedfrom');
 if(sf){ sf.onchange = () => seedFrom(sf.value); }
 function draw(){
  list.innerHTML='';
  let n=0;
  items.forEach((it,i)=>{
   const li=document.createElement('li'); li.draggable=true; li.dataset.i=i; if(!it.on) li.className='off';
   if(it.on) n++;
   li.innerHTML='<input type="checkbox" '+(it.on?'checked':'')+'><span class="n">'+(it.on?('#'+n):'—')+'</span><span class="nm"></span>'+
     '<button type="button" class="sec up">▲</button><button type="button" class="sec dn">▼</button>';
   li.querySelector('.nm').textContent=it.name;
   li.querySelector('input').onchange=e=>{it.on=e.target.checked;draw()};
   li.querySelector('.up').onclick=()=>{if(i>0){[items[i-1],items[i]]=[items[i],items[i-1]];draw()}};
   li.querySelector('.dn').onclick=()=>{if(i<items.length-1){[items[i+1],items[i]]=[items[i],items[i+1]];draw()}};
   li.ondragstart=e=>{e.dataTransfer.setData('text/plain',i);li.classList.add('drag')};
   li.ondragend=()=>li.classList.remove('drag');
   li.ondragover=e=>e.preventDefault();
   li.ondrop=e=>{e.preventDefault();const from=+e.dataTransfer.getData('text/plain');const [m]=items.splice(from,1);items.splice(i,0,m);draw()};
   list.appendChild(li);
  });
  document.getElementById('count').textContent=n+' pilots in the bracket';
  document.getElementById('pilots').value=items.filter(x=>x.on).map(x=>x.name).join('\n');
  preview();
 }
 // The preview: the same bracket engine draws the races, nothing is saved.
 const form = document.getElementById('stageform'), pv = document.getElementById('preview');
 let pvTimer = null, pvSeq = 0;
 function preview(){
  clearTimeout(pvTimer);
  pvTimer = setTimeout(() => {
   const seq = ++pvSeq; pv.classList.add('busy');
   fetch('/stages/preview', {method:'POST', body:new URLSearchParams(new FormData(form))})
    .then(r => r.text()).then(html => { if(seq === pvSeq){ pv.innerHTML = html; pv.classList.remove('busy'); } })
    .catch(() => { if(seq === pvSeq) pv.classList.remove('busy'); });
  }, 200);
 }
 form.addEventListener('input', e => { if(e.target.id !== 'addp') preview(); });
 form.addEventListener('change', e => { if(e.target.id !== 'addp') preview(); });
 document.getElementById('addbtn').onclick=()=>{
  const ta=document.getElementById('addp');
  ta.value.split('\n').map(x=>x.trim()).filter(Boolean).forEach(n=>{if(!items.some(x=>x.name.toLowerCase()===n.toLowerCase()))items.push({name:n,on:true})});
  ta.value=''; draw();
 };
 function showOpts(){
  const k=document.querySelector('input[name=format]:checked').value;
  document.querySelectorAll('[data-for]').forEach(el=>{el.style.display=el.dataset.for.split(' ').includes(k)?'':'none'});
 }
 document.querySelectorAll('input[name=format]').forEach(r=>r.onchange=showOpts);
 showOpts();
 if(sf && sf.value) seedFrom(sf.value); else draw();
})();
</script>`)
	writeSetup(w, r, "Create bracket", b.String())
}

// seedClass is a RotorHazard race class with its pilots in RotorHazard's
// ranking order, from the dd-pits plugin.
type seedClass struct {
	ID     int      `json:"id"`
	Name   string   `json:"name"`
	Pilots []string `json:"pilots"`
	Ranked int      `json:"ranked"`
	By     string   `json:"by"`
}

// seedClasses lists the timer's race classes to seed a bracket from, best
// first: classes with results, then by most pilots. Nil on FPVTrackside or
// a plugin too old to say.
func (s *liveServer) seedClasses() []seedClass {
	if s.stages == nil {
		return nil
	}
	t, ok := s.stages.timer.(*rhStageTimer)
	if !ok {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(t.base, "/")+"/dd-pits/api/seed-classes", nil)
	if err != nil {
		return nil
	}
	resp, err := stageHTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out []seedClass
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	// Brackets' own classes aren't where a bracket's pilots come from.
	own := map[int]bool{}
	for _, st := range s.stages.list() {
		if st.ClassID != 0 {
			own[st.ClassID] = true
		}
	}
	kept := out[:0]
	for _, c := range out {
		if !own[c.ID] && len(c.Pilots) > 0 {
			kept = append(kept, c)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if (kept[i].Ranked > 0) != (kept[j].Ranked > 0) {
			return kept[i].Ranked > 0
		}
		return len(kept[i].Pilots) > len(kept[j].Pilots)
	})
	return kept
}

// ── one stage ────────────────────────────────────────────────────────────────

var stateLabel = map[string]string{
	"waiting":  "Waiting",
	"ready":    "Ready to send",
	"sent":     "On the timer",
	"running":  "Racing",
	"flown":    "Flown",
	"finished": "Done",
	"void":     "Not needed",
	// the create page's preview
	"pv-seeded": "Seeded",
	"pv-later":  "From results",
}

func (s *liveServer) stageView(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.stages.get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	admin := localSetupAllowed(r)
	races, f, err := s.stages.races(st, s.currentMeeting())
	b := &strings.Builder{}
	b.WriteString(loginBar(r))
	if err != nil {
		fmt.Fprintf(b, `<div class="card"><p class="warn">%s</p></div>`, esc(err.Error()))
		writeSetup(w, r, st.Name, b.String())
		return
	}
	// RotorHazard's page for a heat this bracket sent there.
	var heatLink func(string) string
	if base := rhBase(r); base != "" {
		heatLink = func(link string) string {
			var heat int
			if _, err := fmt.Sscanf(link, "rh-h%d", &heat); err != nil || heat == 0 {
				return ""
			}
			// The plugin sends it on to the web app's Heats page when that's
			// installed, else the classic Event page with the heat picked out.
			return fmt.Sprintf("%s/dd-pits/heat/%d", base, heat)
		}
	}
	ready, done, total := 0, 0, 0
	for _, rc := range races {
		if rc.State == "ready" {
			ready++
		}
		if !rc.IsVoid {
			total++
			if rc.IsComplete {
				done++
			}
		}
	}

	rh := ""
	if base := rhBase(r); base != "" {
		cls := ""
		if st.ClassID != 0 {
			cls = fmt.Sprintf("class=%d", st.ClassID)
		} else {
			for _, l := range st.Links {
				var h int
				if _, err := fmt.Sscanf(l, "rh-h%d", &h); err == nil && h > 0 {
					cls = fmt.Sprintf("heat_class=%d", h)
					break
				}
			}
		}
		open := esc(base) + "/dd-pits/open?"
		if cls != "" {
			rh += ` · <a href="` + open + cls + `">RotorHazard heats ↗</a>`
		}
		rh += ` · <a href="` + open + `page=run">RotorHazard Run page ↗</a> · <a href="` + open + `page=results">Results ↗</a>`
	}
	fmt.Fprintf(b, `<div class="card stagehead"><div><p class="mut"><a href="/stages">← All brackets</a>%s%s</p><h1>%s</h1><p class="mut">%s · %d pilots</p>`,
		rh, logoutLink(r), esc(st.Name), esc(formatLabel(st.Format)), len(st.Pilots))
	if c := f.Champion(); c != "" {
		fmt.Fprintf(b, `<p class="champ">🏆 %s</p>`, esc(c))
	}
	fmt.Fprintf(b, `<p class="mut">%d of %d races run · <a href="#standings">%s</a></p>`, done, total,
		map[bool]string{true: "Final standings", false: "Standings"}[f.IsComplete()])
	b.WriteString(`</div>`)

	if admin {
		b.WriteString(`<form method="post" action="/stages/` + esc(st.ID) + `/action" class="acts">`)
		if ready > 0 {
			fmt.Fprintf(b, `<button name="do" value="send">Send %d ready race%s to %s</button>`, ready, plural(ready), esc(timingSystem))
		}
		b.WriteString(`<button class="sec" name="do" value="sync">Check results</button>`)
		fmt.Fprintf(b, `<button class="sec" name="do" value="autosend">Auto-send: %s</button>`, onOff(st.AutoSend))
		fmt.Fprintf(b, `<button class="sec" name="do" value="autoresults">Results from timing: %s</button>`, onOff(st.AutoResults))
		b.WriteString(`<button class="sec" name="do" value="rebuild" onclick="return confirm('Re-create this bracket\'s unflown races on the timer, round by round? Flown races and results stay.')">Rebuild on timer</button>`)
		b.WriteString(`<button class="sec" name="do" value="delete" onclick="return confirm('Delete this bracket? Its races not flown yet are removed from the timer; flown races and their results stay.')">Delete</button></form>`)
	}
	b.WriteString(`</div>`)
	if f.IsComplete() {
		standingsCard(b, f, st, "Final standings") // the result, first thing on the page
	}
	if admin {
		fmt.Fprintf(b, `<div class="card"><form method="post" action="/stages/%s/action" class="row" style="align-items:flex-end">
<div><label>Laps per race (0 = the timer's format)</label><input type="number" name="laps" value="%d" min="0" max="99"></div>
<div><label>Race time (0 = no limit)</label><input type="text" name="raceTime" value="%s"></div>
<div style="flex:0"><button class="sec" name="do" value="settings">Save race settings</button></div></form>
<p class="mut">Applies to the races not flown yet. %s</p></div>`, esc(st.ID), st.Setup.Laps, esc(fmtDuration(st.Setup.RaceSeconds)), raceSettingsNote())
	} else if st.Setup.Laps > 0 || st.Setup.RaceSeconds > 0 {
		fmt.Fprintf(b, `<div class="card"><p class="mut">%s</p></div>`, esc(describeRaceSettings(st.Setup)))
	}
	if admin {
		s.orderCard(b, st, races)
	}
	if st.LastError != "" && admin {
		fmt.Fprintf(b, `<div class="card"><p class="warn">%s</p></div>`, esc(st.LastError))
	}

	s.bracketGrid(b, st, races, admin, heatLink, true)

	if !f.IsComplete() {
		if st.Format == raceformat.KindMains || st.Format == raceformat.KindLadder {
			standingsCard(b, f, st, "Standings so far")
		} else {
			fmt.Fprintf(b, `<div class="card" id="standings"><h2>Final standings</h2><p class="mut">They appear here, and at the top of the page, once the final is flown. %d of %d races run so far.</p></div>`, done, total)
		}
	}
	b.WriteString(stageCSS + flowLines + stageRefresh)
	writeSetup(w, r, st.Name, b.String())
}

// bracketGrid lays out a bracket's races: one section per bracket (bracket
// progression) or points bracket, a column per round within it. card is
// false for a section without its own card (the create page's preview).
func (s *liveServer) bracketGrid(b *strings.Builder, st stageRecord, races []stageRace, admin bool, heatLink func(string) string, card bool) {
	flow := stageFlow(st)
	titles := map[string]string{}
	for _, rc := range races {
		titles[rc.ID] = rc.Title
	}
	// One section per bracket (bracket progression) or per points bracket;
	// within it, a column per round.
	type section struct {
		key    string
		rounds map[int][]stageRace
	}
	var sections []*section
	idx := map[string]*section{}
	for _, rc := range races {
		key := mainOf(rc.ID)
		if st.Format == raceformat.KindMains {
			key = rc.GroupLabel
		}
		sec := idx[key]
		if sec == nil {
			sec = &section{key: key, rounds: map[int][]stageRace{}}
			idx[key] = sec
			sections = append(sections, sec)
		}
		sec.rounds[rc.Round] = append(sec.rounds[rc.Round], rc)
	}
	for _, sec := range sections {
		if card {
			b.WriteString(`<div class="card">`)
		} else {
			b.WriteString(`<div class="psec">`)
		}
		if sec.key != "" {
			name := sec.key
			if len(name) == 1 {
				name = "Bracket " + name
			}
			fmt.Fprintf(b, `<h2>%s</h2>`, esc(name))
		}
		var nums []int
		for n := range sec.rounds {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		// The lines between races, drawn by flowLines once laid out.
		inSec := map[string]bool{}
		for _, rs := range sec.rounds {
			for _, rc := range rs {
				inSec[rc.ID] = true
			}
		}
		var edges []flowEdge
		for _, e := range flowEdges(flow, titles) {
			if inSec[e.From] && inSec[e.To] {
				edges = append(edges, e)
			}
		}
		ej, _ := json.Marshal(edges)
		if card && len(edges) > 0 {
			b.WriteString(`<p class="flowkey">Lines follow the winners. Point at a race (or tap it) to see where all its pilots go, losers' bracket included.</p>`)
		}
		fmt.Fprintf(b, `<div class="grid" data-flow="%s">`, esc(string(ej)))
		for _, n := range nums {
			fmt.Fprintf(b, `<div class="col"><div class="colh">Round %d</div><div class="colb">`, n)
			for _, rc := range sec.rounds[n] {
				s.raceCard(b, st, rc, admin, heatLink, flow, titles)
			}
			b.WriteString(`</div></div>`)
		}
		b.WriteString(`</div></div>`)
	}

}

// standingsCard is a bracket's standings table, medals for the podium.
func standingsCard(b *strings.Builder, f raceformat.Format, st stageRecord, title string) {
	rows := standings(f, st.seeds())
	if len(rows) == 0 {
		return
	}
	medal := map[int]string{1: "🥇", 2: "🥈", 3: "🥉"}
	fmt.Fprintf(b, `<div class="card" id="standings"><h2>%s</h2><table class="standings"><tr><th>Place</th><th>Pilot</th><th></th></tr>`, esc(title))
	for _, row := range rows {
		tied := ""
		if row.Tied {
			tied = ` <span class="badge">not decided on track</span>`
		}
		place := ordinalPlace(row.Rank)
		if m := medal[row.Rank]; m != "" && !row.Tied {
			place = m + " " + place
		}
		cls := ""
		if row.Rank <= 3 {
			cls = ` class="pod"`
		}
		fmt.Fprintf(b, `<tr%s><td>%s</td><td>%s%s</td><td class="mut">%s</td></tr>`, cls, place, esc(row.Pilot), tied, esc(row.Detail))
	}
	b.WriteString(`</table></div>`)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (s *liveServer) raceCard(b *strings.Builder, st stageRecord, rc stageRace, admin bool, rhHeatLink func(string) string, flow raceFlow, titles map[string]string) {
	label := stateLabel[rc.State]
	if rc.State == "waiting" && rc.Link != "" {
		label = "On the timer · pilots to come"
	}
	side, sideLabel := raceSide(rc.Title)
	tag := ""
	if sideLabel != "" {
		tag = `<span class="side">` + sideLabel + `</span>`
	}
	fmt.Fprintf(b, `<div class="race st-%s side-%s" id="r-%s"><div class="rh">%s<b class="rt">%s</b><span class="badge"><i></i>%s</span></div><ol>`,
		esc(rc.State), side, esc(rc.ID), tag, esc(rc.Title), esc(label))
	place := map[string]int{}
	for i, p := range resultOrder(rc.RaceView) {
		place[p] = i + 1
	}
	won := map[string]bool{}
	for _, p := range rc.Winners {
		won[p] = true
	}
	from := func(src seatSource) string {
		return fmt.Sprintf(`<span class="from"><b>%s</b> %s</span>`, ordinalPlace(src.Rank), esc(shortRaceTitle(titles[src.From])))
	}
	for _, p := range rc.Pilots {
		cls, mark := "", ""
		if rc.IsComplete {
			cls = " out"
			if won[p] {
				cls = " win"
			}
			if n := place[p]; n > 0 {
				mark = fmt.Sprintf(`<span class="pl p%d">%d</span>`, n, n)
			}
		}
		seed := ""
		if n := rc.Seeds[p]; n > 0 {
			seed = fmt.Sprintf(`<span class="sd" title="Seed %d">%d</span>`, n, n)
		}
		via := ""
		if src, ok := flow.Known[rc.ID][p]; ok && !rc.IsComplete {
			via = from(src)
		}
		fmt.Fprintf(b, `<li class="seat%s">%s<span class="nm">%s</span>%s%s</li>`, cls, seed, esc(p), via, mark)
	}
	for _, src := range flow.Open[rc.ID] {
		fmt.Fprintf(b, `<li class="seat tbd"><span class="sd">·</span>%s</li>`, from(src))
	}
	if len(rc.Pilots) == 0 && len(flow.Open[rc.ID]) == 0 {
		b.WriteString(`<li class="seat tbd"><span class="sd">·</span><span class="from">to be decided</span></li>`)
	}
	b.WriteString(`</ol>`)
	if strings.HasPrefix(rc.State, "pv-") {
		b.WriteString(`</div>`)
		return // a preview: nothing to link to or run yet
	}
	b.WriteString(`<div class="links">`)
	if u := s.dashboardLink(rc); u != "" {
		fmt.Fprintf(b, `<a class="mlink" href="%s">Dashboard</a>`, esc(u))
	}
	if rc.Link != "" && rhHeatLink != nil {
		if u := rhHeatLink(rc.Link); u != "" {
			fmt.Fprintf(b, `<a class="mlink" href="%s">RotorHazard ↗</a>`, esc(u))
		}
	}
	b.WriteString(`</div>`)
	if admin && rc.Locked && rc.IsComplete {
		b.WriteString(`<p class="mut small">Result entered by hand</p>`)
	}
	if admin && !rc.IsVoid && len(rc.Pilots) > 0 {
		act := `/stages/` + esc(st.ID) + `/action`
		b.WriteString(`<div class="ra">`)
		ctl := func(do, label, cls, confirm string) {
			onclick := ""
			if confirm != "" {
				onclick = ` onclick="return confirm('` + confirm + `')"`
			}
			fmt.Fprintf(b, `<button class="%s" name="do" value="%s" form="rc-%s"%s>%s</button>`, cls, do, esc(rc.ID), onclick, label)
		}
		fmt.Fprintf(b, `<form method="post" action="%s" id="rc-%s"><input type="hidden" name="race" value="%s"></form><div class="rc">`, act, esc(rc.ID), esc(rc.ID))
		switch rc.State {
		case "ready", "sent":
			ctl("start", "▶ Start race", "small go", "")
		case "running":
			ctl("stop", "■ Stop &amp; save", "small stop", "")
			ctl("restart", "↻ Restart", "sec small", "Restart this race? The run so far is thrown away.")
			ctl("discard", "Discard", "sec small", "Discard this run?")
		case "flown", "finished":
			if rc.CanEdit || !rc.IsComplete {
				ctl("restart", "↻ Re-run", "sec small", "Fly this race again? The new result replaces this one.")
			}
		}
		if rc.Link != "" && (rc.State == "flown" || rc.State == "finished" || rc.State == "running") {
			fmt.Fprintf(b, `<a class="mlink" href="/marshal?race=%s">Marshal laps</a>`, url.QueryEscape(s.stages.linkedRaceID(rc.Link, s.currentMeeting())))
		}
		b.WriteString(`</div>`)
		if rc.State == "ready" || rc.State == "sent" {
			fmt.Fprintf(b, `<form method="post" action="%s"><input type="hidden" name="race" value="%s"><button class="sec small" name="do" value="race-send">%s</button></form>`,
				act, esc(rc.ID), map[bool]string{true: "Send &amp; make current", false: "Make current"}[rc.State == "ready"])
		}
		if rc.State != "waiting" {
			fmt.Fprintf(b, `<details><summary>Enter result</summary><form method="post" action="%s"><input type="hidden" name="race" value="%s">`, act, esc(rc.ID))
			for i := range rc.Pilots {
				fmt.Fprintf(b, `<label>%s</label><select name="p%d">`, ordinalPlace(i+1), i)
				for j, p := range rc.Pilots {
					sel := ""
					if j == i {
						sel = " selected"
					}
					fmt.Fprintf(b, `<option%s>%s</option>`, sel, esc(p))
				}
				b.WriteString(`</select>`)
			}
			b.WriteString(`<p><button class="small" name="do" value="result">Save result</button></p></form></details>`)
		}
		if rc.IsComplete && rc.CanEdit {
			fmt.Fprintf(b, `<form method="post" action="%s"><input type="hidden" name="race" value="%s"><button class="sec small" name="do" value="reset" onclick="return confirm('Clear this result? Its pilots are taken back out of later races.')">Clear result</button></form>`, act, esc(rc.ID))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
}

// raceSide is which part of the bracket a race is in, by its title: the
// winners' or losers' bracket, or the finals.
func raceSide(title string) (string, string) {
	switch {
	case strings.HasPrefix(title, "Winners"):
		return "w", "Winners"
	case strings.HasPrefix(title, "Losers"):
		return "l", "Losers"
	case strings.Contains(title, "Final") || strings.HasPrefix(title, "Chase the Ace"):
		return "f", "Final"
	}
	return "x", ""
}

func ordinalPlace(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	}
	return fmt.Sprintf("%dth", n)
}

func (s *liveServer) stageAction(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/stages/"+id, http.StatusSeeOther)
		return
	}
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	m := s.stages
	raceID := r.FormValue("race")
	var err error
	switch r.FormValue("do") {
	case "send":
		m.syncAll(true)
	case "sync":
		m.syncAll(false)
	case "autosend":
		err = m.update(id, func(st *stageRecord) error { st.AutoSend = !st.AutoSend; return nil })
	case "autoresults":
		err = m.update(id, func(st *stageRecord) error { st.AutoResults = !st.AutoResults; return nil })
	case "delete":
		if _, derr := m.deleteWithRaces(id, s.currentMeeting()); derr != nil {
			log.Printf("stages: deleting %s: %v", id, derr)
		}
		http.Redirect(w, r, "/stages", http.StatusSeeOther)
		return
	case "race-send":
		err = m.sendOne(id, raceID, s.currentMeeting())
	case "result":
		var order []string
		seen := map[string]bool{}
		for i := 0; ; i++ {
			v, ok := r.Form["p"+strconv.Itoa(i)]
			if !ok {
				break
			}
			if len(v) > 0 && !seen[v[0]] {
				seen[v[0]] = true
				order = append(order, v[0])
			}
		}
		err = m.setResult(id, raceID, order)
	case "reset":
		err = m.resetResult(id, raceID)
	case "order":
		err = m.setOrder(id, r.Form["race"], s.currentMeeting())
	case "rebuild":
		err = m.rebuildOnTimer(id, s.currentMeeting())
	case "settings":
		secs, perr := parseDuration(r.FormValue("raceTime"))
		if perr != nil {
			err = perr
			break
		}
		err = m.setRaceSettings(id, atoiOr(r.FormValue("laps"), 0), secs, s.currentMeeting())
	case "start", "stop", "restart", "discard":
		err = m.raceControl(id, raceID, r.FormValue("do"), s.currentMeeting())
	}
	if err != nil {
		_ = m.update(id, func(st *stageRecord) error { st.LastError = err.Error(); return nil })
	}
	http.Redirect(w, r, "/stages/"+id, http.StatusSeeOther)
}

// stageFind answers which bracket race a timer race belongs to, for the
// RotorHazard plugin's "open the bracket in dd-pits" strip:
// /stages/find?link=rh-h12 -> {"stage","race","title","path"}.
func (s *liveServer) stageFind(w http.ResponseWriter, r *http.Request) {
	link := r.FormValue("link")
	w.Header().Set("Content-Type", "application/json")
	for _, st := range s.stages.list() {
		for raceID, l := range st.Links {
			if l != link || link == "" {
				continue
			}
			title := raceID
			if f, err := st.engine(); err == nil {
				for _, rv := range f.Races() {
					if rv.ID == raceID {
						title = stageTerms(rv.Title)
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"stage": st.Name, "race": raceID, "title": title,
				"path": "/stages/" + st.ID + "#r-" + raceID})
			return
		}
	}
	_, _ = w.Write([]byte("{}"))
}

// stageLinks lists every bracket with the timer races it made, so the
// RotorHazard plugin can link each race class back to its bracket:
// [{"id","name","path","classId","links":["rh-h12",…]}].
func (s *liveServer) stageLinks(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Path    string   `json:"path"`
		ClassID int      `json:"classId,omitempty"`
		Links   []string `json:"links"`
	}
	out := []entry{}
	for _, st := range s.stages.list() {
		e := entry{ID: st.ID, Name: st.Name, Path: "/stages/" + st.ID, ClassID: st.ClassID, Links: []string{}}
		for _, l := range st.Links {
			e.Links = append(e.Links, l)
		}
		sort.Strings(e.Links)
		out = append(out, e)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *liveServer) stageJSON(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.stages.get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	races, f, err := s.stages.races(st, s.currentMeeting())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type raceJSON struct {
		ID, Title, Group, State, Link string
		Round                         int
		Pilots, Winners               []string
		Locked                        bool
	}
	out := struct {
		ID, Name, Format, Champion string
		Pilots                     []string
		Races                      []raceJSON
		Standings                  []raceformat.Standing
	}{ID: st.ID, Name: st.Name, Format: string(st.Format), Champion: f.Champion(), Pilots: st.Pilots, Standings: standings(f, st.seeds())}
	for _, rc := range races {
		out.Races = append(out.Races, raceJSON{rc.ID, rc.Title, rc.GroupLabel, rc.State, rc.Link, rc.Round, rc.Pilots, rc.Winners, rc.Locked})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

const stageCSS = `<style>
 table.standings tr.pod td{font-weight:700;font-size:1.05rem}
 table.standings tr.pod:first-of-type td,table.standings tr.pod td:first-child{white-space:nowrap}
 main{max-width:96rem}
 .loginbar{display:flex;align-items:center;justify-content:space-between;gap:1rem;flex-wrap:wrap;border-color:rgba(198,255,61,.35)}
 .stagehead{display:flex;justify-content:space-between;gap:1rem;flex-wrap:wrap;align-items:center}
 .stagehead h1{margin:.1rem 0;font-size:clamp(1.6rem,3.5vw,2.3rem);letter-spacing:-.03em}
 .stagehead .champ{font-size:1.4rem;font-weight:800;color:var(--acc);margin:.2rem 0}
 .acts{display:flex;gap:.5rem;flex-wrap:wrap}
 .grid{display:flex;gap:2.4rem;overflow-x:auto;padding:.2rem .2rem .6rem;position:relative}
 .grid>svg.flow{position:absolute;left:0;top:0;pointer-events:none;overflow:visible;z-index:0}
 .col{min-width:15.5rem;flex:0 0 15.5rem;display:flex;flex-direction:column;gap:.6rem;position:relative;z-index:1}
 .colb{flex:1;display:flex;flex-direction:column;justify-content:space-around;gap:.9rem}
 #preview .colb{display:contents}
 .grid>svg.flow path{transition:opacity .15s}
 .grid>svg.flow path.drop{opacity:0}
 .grid>svg.flow.focus path{opacity:.1}
 .grid>svg.flow.focus path.on{opacity:1;stroke-width:2.5}
 .race.lit{border-color:rgba(198,255,61,.6)}
 .flowkey{color:var(--mut);font-size:.78rem;margin:.2rem .2rem .6rem}
 .colh{color:var(--mut);font-size:.72rem;letter-spacing:.12em;text-transform:uppercase;font-weight:700;padding:0 .2rem .35rem;border-bottom:1px solid var(--line);margin-bottom:.1rem;align-self:stretch}
 .race{border:1px solid var(--line);border-radius:14px;background:linear-gradient(180deg,rgba(30,33,40,.96),rgba(18,20,25,.96));box-shadow:0 10px 30px -18px rgba(0,0,0,.9);overflow:hidden}
 .race .rh{display:flex;flex-direction:column;align-items:flex-start;gap:.3rem;padding:.6rem .75rem .5rem;border-bottom:1px solid var(--line);background:rgba(255,255,255,.025)}
 .race .rt{font-size:.88rem;line-height:1.25;letter-spacing:-.01em}
 .race{border-left-width:4px}
 .race .side{font-size:.62rem;font-weight:800;letter-spacing:.14em;text-transform:uppercase}
 .race.side-w{border-left-color:#9fe03a}.race.side-w .rh{background:linear-gradient(90deg,rgba(159,224,58,.13),rgba(159,224,58,.02))}.race.side-w .side{color:#b5f04f}
 .race.side-l{border-left-color:#ff9f43}.race.side-l .rh{background:linear-gradient(90deg,rgba(255,159,67,.13),rgba(255,159,67,.02))}.race.side-l .side{color:#ffb36b}
 .race.side-f{border-left-color:#f5c542}.race.side-f .rh{background:linear-gradient(90deg,rgba(245,197,66,.16),rgba(245,197,66,.03))}.race.side-f .side{color:#f7d264}
 .race ol{list-style:none;margin:0;padding:.3rem 0;display:grid}
 .race li.seat{display:flex;align-items:center;gap:.55rem;padding:.3rem .75rem;font-size:.9rem;min-height:2rem}
 .race li.seat+li.seat{border-top:1px solid rgba(255,255,255,.04)}
 .race li .sd{flex:0 0 1.35rem;height:1.35rem;display:inline-grid;place-items:center;border-radius:6px;background:rgba(255,255,255,.06);color:var(--mut);font-size:.7rem;font-weight:700;font-variant-numeric:tabular-nums}
 .race li .nm{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-weight:600}
 .race li .from{flex:1;min-width:0;color:var(--mut);font-size:.76rem;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
 .race li .nm+.from{flex:0 1 auto;max-width:55%;opacity:.8}
 .race li .from b{color:#c3c9d2;font-weight:700;margin-right:.15rem}
 .race li.tbd{background:repeating-linear-gradient(135deg,transparent 0 6px,rgba(255,255,255,.018) 6px 12px)}
 .race li.tbd .sd{background:transparent;border:1px dashed rgba(255,255,255,.18)}
 .race li.win .nm{color:var(--acc)}.race li.win .sd{background:rgba(198,255,61,.16);color:var(--acc)}
 .race li.out .nm{color:var(--mut);font-weight:500}
 .race li .pl{flex:0 0 auto;font-size:.7rem;font-weight:800;padding:.08rem .42rem;border-radius:999px;background:rgba(255,255,255,.08);color:#c3c9d2}
 .race li .pl.p1{background:#f5c542;color:#1b1400}.race li .pl.p2{background:#c9d1dc;color:#14181d}.race li .pl.p3{background:#d08a4e;color:#1d0f04}
 .race .links{display:flex;gap:.8rem;padding:0 .75rem .45rem}
 .race .links:empty{display:none}
 .race .ra{padding:0 .75rem .65rem}
 .badge{display:inline-flex;align-items:center;gap:.35rem;font-size:.66rem;font-weight:700;letter-spacing:.02em;padding:.14rem .5rem;border-radius:999px;background:rgba(255,255,255,.07);color:#c3c9d2;white-space:nowrap}
 .badge i{width:.42rem;height:.42rem;border-radius:50%;background:currentColor;opacity:.85}
 .st-pv-seeded,.st-ready{border-color:rgba(198,255,61,.45)}.st-pv-seeded .badge,.st-ready .badge{background:rgba(198,255,61,.14);color:var(--acc)}
 .st-running{border-color:#ff453a;box-shadow:0 0 0 1px #ff453a,0 12px 34px -14px rgba(255,69,58,.6)}.st-running .badge{background:rgba(255,69,58,.2);color:#ff6b61}
 .st-running .badge i{animation:ddpulse 1.2s ease-in-out infinite}
 @keyframes ddpulse{50%{opacity:.25}}
 .st-sent .badge,.st-flown .badge{background:rgba(61,224,255,.13);color:var(--cyan)}
 .st-finished .badge{background:rgba(255,255,255,.06);color:var(--mut)}
 .st-waiting .badge,.st-pv-later .badge{background:transparent;border:1px dashed rgba(255,255,255,.16);color:var(--mut)}
 .st-void{opacity:.4}
 .race:target{box-shadow:0 0 0 3px var(--acc);border-color:var(--acc)}
 .ra{display:grid;gap:.35rem}
 .ra details summary{cursor:pointer;color:var(--acc);font-size:.84rem}
 .ra label{margin:.35rem 0 .1rem}
 button.small{padding:.3rem .75rem;font-size:.8rem}
 .rc{display:flex;gap:.35rem;flex-wrap:wrap}
 button.go{background:var(--acc);color:#07080a}
 .mlink{font-size:.76rem;align-self:center;color:var(--mut)}.mlink:hover{color:var(--acc)}
 button.stop{background:#ff453a;color:#fff}
 p.small{font-size:.78rem;margin:.35rem 0 0}
 .race>p.small{padding:0 .75rem;margin:0 0 .35rem}
</style>`

// flowLines draws the progression between races: from the right of a race
// to the left of the race its pilots go on to, winners' paths in the accent
// colour, drops into the losers' bracket dashed. Redrawn whenever the page
// redraws itself or changes size; nothing where races are stacked.
const flowLines = `<script>
(function(){
 if(window.ddFlowBound) return; window.ddFlowBound=true;
 var NS='http://www.w3.org/2000/svg', R=8;
 // Bracket connectors: out of the race, along, then down a trunk just before
 // the next race, so the lines into a race join up.
 function elbow(x1,y1,x2,y2){
  var xm=x2-16, dy=y2-y1, r=Math.min(R,Math.abs(dy)/2,Math.max(0,(xm-x1)/2));
  if(Math.abs(dy)<1) return 'M'+x1+' '+y1+' H'+x2;
  var s=dy>0?1:-1;
  return 'M'+x1+' '+y1+' H'+(xm-r)+' Q'+xm+' '+y1+' '+xm+' '+(y1+s*r)+' V'+(y2-s*r)+' Q'+xm+' '+y2+' '+(xm+r)+' '+y2+' H'+x2;
 }
 function draw(){
  document.querySelectorAll('.grid[data-flow]').forEach(function(g){
   var old=g.querySelector(':scope > svg.flow');
   var edges; try{ edges=JSON.parse(g.dataset.flow||'[]'); }catch(e){ edges=[]; }
   var gr=g.getBoundingClientRect(), svg=document.createElementNS(NS,'svg');
   svg.setAttribute('class','flow'); svg.setAttribute('width',g.scrollWidth); svg.setAttribute('height',g.scrollHeight);
   var n=0;
   edges.forEach(function(e){
    var a=document.getElementById('r-'+e.from), b=document.getElementById('r-'+e.to);
    if(!a||!b||!g.contains(a)||!g.contains(b)) return;
    var ra=a.getBoundingClientRect(), rb=b.getBoundingClientRect();
    if(rb.left<ra.right-2) return; // stacked, not side by side
    var x1=ra.right-gr.left+g.scrollLeft, y1=ra.top+Math.min(ra.height/2,48)-gr.top+g.scrollTop;
    var x2=rb.left-gr.left+g.scrollLeft, y2=rb.top+Math.min(rb.height/2,48)-gr.top+g.scrollTop;
    var p=document.createElementNS(NS,'path');
    p.setAttribute('d',elbow(x1,y1,x2,y2)); p.setAttribute('fill','none');
    p.setAttribute('stroke-width','1.5'); p.setAttribute('stroke-linecap','round');
    p.setAttribute('stroke', e.drop?'rgba(255,170,90,.85)':'rgba(198,255,61,.5)');
    if(e.drop){ p.setAttribute('stroke-dasharray','4 5'); p.setAttribute('class','drop'); }
    p.dataset.from=e.from; p.dataset.to=e.to;
    var t=document.createElementNS(NS,'title');
    t.textContent=(e.ranks||[]).map(function(n){return n===1?'1st':n===2?'2nd':n===3?'3rd':n+'th'}).join(', ');
    p.appendChild(t); svg.appendChild(p); n++;
   });
   if(old) old.remove();
   if(n) g.insertBefore(svg,g.firstChild);
   if(focusId) light(focusId);
  });
 }
 // Point at a race: its lines in and out, everything else faded.
 var focusId=null;
 function light(id){
  focusId=id;
  document.querySelectorAll('.race.lit').forEach(function(r){ r.classList.remove('lit'); });
  document.querySelectorAll('.grid > svg.flow').forEach(function(svg){
   svg.classList.toggle('focus',!!id);
   svg.querySelectorAll('path').forEach(function(p){
    var on=!!id&&(p.dataset.from===id||p.dataset.to===id);
    p.classList.toggle('on',on);
    if(on){ var o=document.getElementById('r-'+(p.dataset.from===id?p.dataset.to:p.dataset.from)); if(o) o.classList.add('lit'); }
   });
  });
 }
 document.addEventListener('mouseover',function(e){
  var r=e.target.closest&&e.target.closest('.grid .race'); var id=r?r.id.replace(/^r-/,''):null;
  if(id!==focusId) light(id);
 });
 document.addEventListener('click',function(e){
  if(e.target.closest('a,button,summary,select,input')) return;
  var r=e.target.closest('.grid .race'); light(r?r.id.replace(/^r-/,''):null);
 });
 var t=null; function soon(){ clearTimeout(t); t=setTimeout(draw,60); }
 window.addEventListener('resize',soon);
 // The page redraws itself as results arrive; draw the lines straight away
 // (before the browser paints) so they never blink out.
 window.ddDrawFlow=draw;
 if(document.readyState!=='loading') draw(); else document.addEventListener('DOMContentLoaded',draw);
 if(document.fonts&&document.fonts.ready) document.fonts.ready.then(draw);
})();
</script>`

// stageRefresh reloads the bracket while racing, unless someone is in the
// middle of entering a result.
const stageRefresh = `<script>
// ddPatch brings cur up to date with next, replacing only what differs:
// race cards by id, other blocks one by one; the lines are left to redraw.
function ddPatch(cur, next){
 var a=[].filter.call(cur.children,function(n){return !(n.matches&&n.matches('svg.flow'))}), b=[].slice.call(next.children);
 if(a.length!==b.length){ cur.innerHTML=next.innerHTML; return; }
 for(var i=0;i<a.length;i++){
  var x=a[i], y=b[i];
  if(x.tagName!==y.tagName||x.id!==y.id||x.className!==y.className){ x.replaceWith(y.cloneNode(true)); continue; }
  var xs=x.cloneNode(true); xs.querySelectorAll('svg.flow').forEach(function(n){n.remove()});
  if(xs.outerHTML===y.outerHTML) continue;
  [].forEach.call(y.attributes,function(at){ if(x.getAttribute(at.name)!==at.value) x.setAttribute(at.name,at.value); });
  if(x.id&&/^r-/.test(x.id)||!y.children.length){ x.replaceWith(y.cloneNode(true)); continue; }
  ddPatch(x, y);
 }
}
// The page as served, without the drawn lines, to compare refreshes against.
(function(){ var m=document.querySelector('main'); if(!m) return; var c=m.cloneNode(true);
 c.querySelectorAll('svg.flow').forEach(function(x){x.remove()}); window.ddLastMain=c.innerHTML; })();
setInterval(function(){
 if(document.querySelector('details[open]')||document.activeElement&&/INPUT|SELECT|TEXTAREA/.test(document.activeElement.tagName))return;
 if(window.ddSaving)return;
 fetch(location.href,{headers:{'X-Fragment':'1'}}).then(r=>r.text()).then(t=>{
  const d=new DOMParser().parseFromString(t,'text/html');const m=d.querySelector('main');if(!m)return;
  // Only when something changed, and then only the parts that did, with
  // the lines redrawn before the browser paints: no flicker, no lost place.
  if(m.innerHTML===window.ddLastMain)return; window.ddLastMain=m.innerHTML;
  ddPatch(document.querySelector('main'), m); if(window.ddDrawFlow)window.ddDrawFlow();
 }).catch(()=>{});
},4000);
</script>` + ajaxForms

// ajaxForms makes a page's buttons save in the background and redraw the
// page in place, so a race director keeps their place (scroll, open
// sections) when they start, stop or save something. A reply from another
// page (a deleted bracket's list) is followed normally.
const ajaxForms = `<script>
if(!window.ddAjaxBound){ window.ddAjaxBound=true;
 document.addEventListener('submit',function(e){
  var f=e.target; if(!f.closest('main')||f.method.toLowerCase()!=='post'||f.dataset.full!==undefined)return;
  e.preventDefault();
  var btn=e.submitter, fd=new FormData(f);
  if(btn&&btn.name)fd.append(btn.name,btn.value);
  if(btn){btn.disabled=true;btn.dataset.label=btn.innerHTML;btn.innerHTML='…';}
  var open=[].map.call(document.querySelectorAll('main details[open] > summary'),function(s){return s.textContent.trim()});
  window.ddSaving=true;
  fetch(f.getAttribute('action')||location.href,{method:'POST',body:new URLSearchParams(fd),credentials:'same-origin'})
  .then(function(r){
    var u=new URL(r.url,location.href);
    if(u.pathname!==location.pathname){location.href=r.url;return;}
    return r.text().then(function(t){
      var d=new DOMParser().parseFromString(t,'text/html'),m=d.querySelector('main'); if(!m)return;
      var y=window.scrollY; if(window.ddPatch) ddPatch(document.querySelector('main'),m); else document.querySelector('main').innerHTML=m.innerHTML; window.ddLastMain=m.innerHTML; if(window.ddDrawFlow)window.ddDrawFlow(); window.scrollTo(0,y);
      document.querySelectorAll('main details > summary').forEach(function(s){if(open.indexOf(s.textContent.trim())>=0)s.parentNode.open=true;});
    });
  })
  .catch(function(){ if(btn){btn.disabled=false;btn.innerHTML=btn.dataset.label;} alert('Not saved: the connection dropped. Try again.'); })
  .finally(function(){ window.ddSaving=false; });
 });
}
</script>`

// parseDuration reads a race time: "2:00", "120", "90s" or "2m" as seconds.
func parseDuration(v string) (int, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" || v == "0" {
		return 0, nil
	}
	if strings.HasSuffix(v, "m") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(v, "m"), 64)
		return int(n * 60), err
	}
	v = strings.TrimSuffix(v, "s")
	if i := strings.Index(v, ":"); i >= 0 {
		m, err1 := strconv.Atoi(v[:i])
		sec, err2 := strconv.Atoi(v[i+1:])
		if err1 != nil || err2 != nil || sec >= 60 {
			return 0, fmt.Errorf("give the race time like 2:00")
		}
		return m*60 + sec, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("give the race time like 2:00")
	}
	return n, nil
}

func fmtDuration(secs int) string {
	if secs <= 0 {
		return "0"
	}
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

func describeRaceSettings(s stageSetup) string {
	var parts []string
	if s.Laps > 0 {
		parts = append(parts, fmt.Sprintf("first to %d laps", s.Laps))
	}
	if s.RaceSeconds > 0 {
		parts = append(parts, fmtDuration(s.RaceSeconds)+" race time")
	}
	return "Races: " + strings.Join(parts, ", ") + "."
}

// raceSettingsNote says where the settings land on this timer.
func raceSettingsNote() string {
	if timingSystem == "RotorHazard" {
		return "On RotorHazard the bracket's race class gets its own race format with these, copied from your current one."
	}
	return "FPVTrackside takes the laps per race; its race time is set for the whole event in FPVTrackside."
}

// orderCard lets the race director choose the running order within each
// round. Saving re-creates the unflown races on the timer in that order.
func (s *liveServer) orderCard(b *strings.Builder, st stageRecord, races []stageRace) {
	ordered := s.stages.runningOrder(st, races)
	fmt.Fprintf(b, `<div class="card"><details><summary><b>Running order</b> <span class="mut">— choose which races fly first within each round</span></summary>
<form method="post" action="/stages/%s/action" id="orderform">`, esc(st.ID))
	round := -1
	for _, rc := range ordered {
		if rc.IsVoid {
			continue
		}
		if rc.Round != round {
			if round != -1 {
				b.WriteString(`</ol>`)
			}
			round = rc.Round
			fmt.Fprintf(b, `<h3 class="ordh">Round %d</h3><ol class="ord" data-round="%d">`, round, round)
		}
		fixed := rc.State == "running" || rc.State == "flown" || rc.State == "finished"
		cls, grip, btns := "", `<span class="grip" title="Drag to reorder" aria-hidden="true">⠿</span>`,
			`<button type="button" class="sec small up" aria-label="Move up">▲</button><button type="button" class="sec small dn" aria-label="Move down">▼</button>`
		if fixed {
			cls, grip, btns = ` class="fixed"`, `<span class="grip off" aria-hidden="true">⠿</span>`, `<span class="mut small">flown</span>`
		}
		fmt.Fprintf(b, `<li%s>%s<input type="hidden" name="race" value="%s"><span class="nm">%s</span>%s</li>`, cls, grip, esc(rc.ID), esc(rc.Title), btns)
	}
	if round != -1 {
		b.WriteString(`</ol>`)
	}
	b.WriteString(`<p><button name="do" value="order" class="sec" id="ordersave">Save running order</button>
<span class="mut">Re-creates the races not flown yet on the timer in this order; flown races and results stay.</span></p></form></details></div>
<style>
 .ordh{font-size:.8rem;letter-spacing:.08em;text-transform:uppercase;color:var(--mut);margin:.9rem 0 .3rem}
 .ord{list-style:none;margin:0;padding:0;display:grid;gap:.3rem}
 .ord li{display:flex;align-items:center;gap:.5rem;padding:.4rem .7rem;border:1px solid var(--line);border-radius:12px;background:rgba(0,0,0,.3)}
 .ord li .nm{flex:1}.ord li.fixed{opacity:.55}
 .ord .grip{cursor:grab;touch-action:none;user-select:none;font-size:1.15rem;line-height:1;color:var(--mut);padding:.2rem .35rem;border-radius:8px}
 .ord .grip:hover{color:var(--fg);background:rgba(255,255,255,.08)}.ord .grip.off{cursor:default;visibility:hidden}
 .ord li.dragging{border-color:var(--acc);box-shadow:0 10px 30px -12px rgba(198,255,61,.6);background:rgba(198,255,61,.08)}
 #ordersave.dirty{background:var(--acc);color:#07080a;border-color:var(--acc)}
</style>
<script>
// Handlers on the document: the page refreshes itself, replacing the list.
if(!window.ddOrderBound){ window.ddOrderBound=true;
 function changed(){ var s=document.getElementById('ordersave'); if(s){ s.classList.add('dirty'); s.textContent='Save running order •'; } }
 document.addEventListener('click',function(e){
  var btn=e.target.closest('#orderform button.up,#orderform button.dn'); if(!btn) return;
  var li=btn.closest('li'), ol=li.parentNode;
  if(btn.classList.contains('up')){ var p=li.previousElementSibling; if(p&&!p.classList.contains('fixed')){ ol.insertBefore(li,p); changed(); } }
  else { var n=li.nextElementSibling; if(n){ ol.insertBefore(n,li); changed(); } }
 });
 // Drag by the grip, with a mouse or a finger, within the race's own round.
 // Flown races stay where they are, above anything still to fly.
 document.addEventListener('pointerdown',function(e){
  var grip=e.target.closest('#orderform .grip'); if(!grip||grip.classList.contains('off')) return;
  var li=grip.closest('li'), ol=li.parentNode; e.preventDefault();
  // Followed on the document, not by capturing the pointer on the grip:
  // moving the row in the page would drop the capture after the first step.
  li.classList.add('dragging');
  var moved=false;
  function move(ev){
   var items=[].filter.call(ol.children,function(x){return x!==li});
   var lastFixed=-1; items.forEach(function(x,i){ if(x.classList.contains('fixed')) lastFixed=i; });
   var before=null;
   for(var i=0;i<items.length;i++){ var r=items[i].getBoundingClientRect(); if(ev.clientY<r.top+r.height/2){ before=items[i]; break; } }
   var idx=before?items.indexOf(before):items.length;
   if(idx<=lastFixed) before=items[lastFixed+1]||null;
   if(li.nextElementSibling!==before){ ol.insertBefore(li,before); moved=true; }
  }
  function up(){
   li.classList.remove('dragging');
   document.removeEventListener('pointermove',move); document.removeEventListener('pointerup',up); document.removeEventListener('pointercancel',up);
   if(moved) changed();
  }
  document.addEventListener('pointermove',move); document.addEventListener('pointerup',up); document.addEventListener('pointercancel',up);
 });
}
</script>`)
}

// dashboardLink is where a bracket race is on the dd-pits dashboard: the Live
// view while it races, its card in Race history once flown, Upcoming before.
func (s *liveServer) dashboardLink(rc stageRace) string {
	switch rc.State {
	case "running":
		return "/live"
	case "flown", "finished":
		if rc.Link == "" {
			return ""
		}
		return "/dronedash/races#race-" + url.QueryEscape(s.stages.linkedRaceID(rc.Link, s.currentMeeting()))
	case "void":
		return ""
	}
	return "/dronedash/upcoming"
}

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
	"net/http"
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
	writeSetup(w, "Brackets", b.String())
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

func (s *liveServer) stageNew(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	errMsg := ""
	if r.Method == http.MethodPost {
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
		st, err := s.stages.create(r.FormValue("name"), kind, pilots, setup, r.FormValue("autoSend") == "on")
		if err == nil {
			http.Redirect(w, r, "/stages/"+st.ID, http.StatusSeeOther)
			return
		}
		errMsg = err.Error()
	}

	seeded := seedingFromMeeting(s.currentMeeting())
	seedJSON, _ := json.Marshal(seeded)

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

<div class="card"><h2>2 · Pilots, top seed first</h2>
<p class="mut">Seeded from this meeting's results so far: best consecutive laps, then laps flown. Drag to reorder, untick to leave a pilot out, or add pilots below.</p>
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
<p><label><input type="checkbox" name="autoSend" checked> Send each race to ` + esc(timingSystem) + ` as soon as its line-up is known</label></p>
</div>
<p><button type="submit">Create bracket</button> <a href="/stages" class="mut">Cancel</a></p>
</form>
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
</style>
<script>
(function(){
 const seeded = ` + string(seedJSON) + `;
 const list = document.getElementById('seedlist');
 let items = seeded.map(n => ({name:n, on:true}));
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
 }
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
 showOpts(); draw();
})();
</script>`)
	writeSetup(w, "Create bracket", b.String())
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
	if err != nil {
		fmt.Fprintf(b, `<div class="card"><p class="warn">%s</p></div>`, esc(err.Error()))
		writeSetup(w, st.Name, b.String())
		return
	}
	ready := 0
	for _, rc := range races {
		if rc.State == "ready" {
			ready++
		}
	}

	fmt.Fprintf(b, `<div class="card stagehead"><div><p class="mut"><a href="/stages">Brackets</a></p><h1>%s</h1><p class="mut">%s · %d pilots</p>`,
		esc(st.Name), esc(formatLabel(st.Format)), len(st.Pilots))
	if c := f.Champion(); c != "" {
		fmt.Fprintf(b, `<p class="champ">🏆 %s</p>`, esc(c))
	}
	b.WriteString(`</div>`)
	if admin {
		b.WriteString(`<form method="post" action="/stages/` + esc(st.ID) + `/action" class="acts">`)
		if ready > 0 {
			fmt.Fprintf(b, `<button name="do" value="send">Send %d ready race%s to %s</button>`, ready, plural(ready), esc(timingSystem))
		}
		b.WriteString(`<button class="sec" name="do" value="sync">Check results</button>`)
		fmt.Fprintf(b, `<button class="sec" name="do" value="autosend">Auto-send: %s</button>`, onOff(st.AutoSend))
		fmt.Fprintf(b, `<button class="sec" name="do" value="autoresults">Results from timing: %s</button>`, onOff(st.AutoResults))
		b.WriteString(`<button class="sec" name="do" value="delete" onclick="return confirm('Delete this bracket? Races already on the timer stay there.')">Delete</button></form>`)
	}
	b.WriteString(`</div>`)
	if st.LastError != "" && admin {
		fmt.Fprintf(b, `<div class="card"><p class="warn">%s</p></div>`, esc(st.LastError))
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
		b.WriteString(`<div class="card">`)
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
		b.WriteString(`<div class="grid">`)
		for _, n := range nums {
			fmt.Fprintf(b, `<div class="col"><div class="colh">Round %d</div>`, n)
			for _, rc := range sec.rounds[n] {
				s.raceCard(b, st, rc, admin)
			}
			b.WriteString(`</div>`)
		}
		b.WriteString(`</div></div>`)
	}

	if rows := standings(f, st.seeds()); len(rows) > 0 && (f.IsComplete() || st.Format == raceformat.KindMains || st.Format == raceformat.KindLadder) {
		b.WriteString(`<div class="card"><h2>Standings</h2><table><tr><th>#</th><th>Pilot</th><th></th></tr>`)
		for _, row := range rows {
			tied := ""
			if row.Tied {
				tied = ` <span class="badge">not decided on track</span>`
			}
			fmt.Fprintf(b, `<tr><td>%d</td><td>%s%s</td><td class="mut">%s</td></tr>`, row.Rank, esc(row.Pilot), tied, esc(row.Detail))
		}
		b.WriteString(`</table></div>`)
	}
	b.WriteString(stageCSS + stageRefresh)
	writeSetup(w, st.Name, b.String())
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (s *liveServer) raceCard(b *strings.Builder, st stageRecord, rc stageRace, admin bool) {
	fmt.Fprintf(b, `<div class="race st-%s"><div class="rh"><b>%s</b><span class="badge">%s</span></div><ol>`,
		esc(rc.State), esc(rc.Title), esc(stateLabel[rc.State]))
	won := map[string]bool{}
	for _, p := range rc.Winners {
		won[p] = true
	}
	order := rc.Pilots
	for _, p := range order {
		cls := ""
		if rc.IsComplete {
			if won[p] {
				cls = ` class="win"`
			} else {
				cls = ` class="out"`
			}
		}
		fmt.Fprintf(b, `<li%s><span class="sd">%d</span>%s</li>`, cls, rc.Seeds[p], esc(p))
	}
	if len(order) == 0 {
		b.WriteString(`<li class="mut">to be decided</li>`)
	}
	b.WriteString(`</ol>`)
	if rc.Locked && rc.IsComplete {
		b.WriteString(`<p class="mut small">Result entered by hand</p>`)
	}
	if admin && !rc.IsVoid && len(rc.Pilots) > 0 {
		act := `/stages/` + esc(st.ID) + `/action`
		b.WriteString(`<div class="ra">`)
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

func ordinalPlace(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
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
		m.delete(id)
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
	}
	if err != nil {
		_ = m.update(id, func(st *stageRecord) error { st.LastError = err.Error(); return nil })
	}
	http.Redirect(w, r, "/stages/"+id, http.StatusSeeOther)
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
 main{max-width:96rem}
 .stagehead{display:flex;justify-content:space-between;gap:1rem;flex-wrap:wrap;align-items:center}
 .stagehead h1{margin:.1rem 0;font-size:clamp(1.6rem,3.5vw,2.3rem);letter-spacing:-.03em}
 .stagehead .champ{font-size:1.4rem;font-weight:800;color:var(--acc);margin:.2rem 0}
 .acts{display:flex;gap:.5rem;flex-wrap:wrap}
 .grid{display:flex;gap:.9rem;overflow-x:auto;padding-bottom:.4rem}
 .col{min-width:14.5rem;flex:0 0 14.5rem;display:flex;flex-direction:column;gap:.7rem}
 .colh{color:var(--mut);font-size:.72rem;letter-spacing:.1em;text-transform:uppercase;font-weight:700}
 .race{border:1px solid var(--line);border-radius:16px;padding:.7rem .8rem;background:rgba(0,0,0,.3)}
 .race .rh{display:flex;justify-content:space-between;gap:.4rem;align-items:flex-start;font-size:.86rem}
 .race ol{list-style:none;margin:.5rem 0 0;padding:0;display:grid;gap:.2rem}
 .race li{display:flex;gap:.5rem;font-size:.92rem}
 .race li .sd{color:var(--mut);font-size:.75rem;min-width:1.3rem;text-align:right;padding-top:.12rem}
 .race li.win{color:var(--acc);font-weight:700}.race li.out{color:var(--mut)}
 .badge{font-size:.68rem;font-weight:700;padding:.12rem .45rem;border-radius:999px;background:rgba(255,255,255,.08);color:#c3c9d2;white-space:nowrap}
 .st-ready{border-color:rgba(198,255,61,.5)}.st-ready .badge{background:rgba(198,255,61,.16);color:var(--acc)}
 .st-running{border-color:#ff453a}.st-running .badge{background:rgba(255,69,58,.2);color:#ff6b61}
 .st-sent .badge,.st-flown .badge{background:rgba(61,224,255,.14);color:var(--cyan)}
 .st-void{opacity:.45}
 .ra{margin-top:.5rem;display:grid;gap:.35rem}
 .ra details summary{cursor:pointer;color:var(--acc);font-size:.84rem}
 .ra label{margin:.35rem 0 .1rem}
 button.small{padding:.3rem .75rem;font-size:.8rem}
 p.small{font-size:.78rem;margin:.35rem 0 0}
</style>`

// stageRefresh reloads the bracket while racing, unless someone is in the
// middle of entering a result.
const stageRefresh = `<script>
setInterval(function(){
 if(document.querySelector('details[open]')||document.activeElement&&/INPUT|SELECT|TEXTAREA/.test(document.activeElement.tagName))return;
 fetch(location.href,{headers:{'X-Fragment':'1'}}).then(r=>r.text()).then(t=>{
  const d=new DOMParser().parseFromString(t,'text/html');const m=d.querySelector('main');if(m)document.querySelector('main').innerHTML=m.innerHTML;
 }).catch(()=>{});
},4000);
</script>`

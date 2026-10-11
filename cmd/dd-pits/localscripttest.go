package main

// Comparing what each writer actually produces.
//
// The page can say a model is downloaded and the right size, and none of that
// tells you the thing that matters: does it get the RACE right. A small model
// will happily attribute a lap time to the wrong pilot in fluent, confident
// prose — I hit exactly that in the replay commentary and had to fix it, and it
// is why the 8B and the 4B carry a warning rather than a recommendation.
//
// So this runs every writer over the SAME fixed heat and shows the output next
// to the data it was given. A wrong attribution is then obvious at a glance
// rather than something you would have to notice on a race day.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bob9/dd-pits/internal/qwentts"
	"github.com/bob9/dd-pits/internal/tts"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/localchat"
)

// scriptTestRace is the fixed heat every writer is asked to describe.
//
// Fixed rather than live, so two models can be compared on identical input —
// and hand-built so the right answer is unambiguous:
//
//	bob9 leads, and set the fastest lap (10.88)
//	Willman is second and took the holeshot (2.41, the smallest)
//	Fizz is third
//	Wizz retired after one lap
//
// The numbers are deliberately far apart. A model that muddles pilots here is
// not going to do better on a real meeting.
func scriptTestRace() (ddapi.Race, *ddapi.Meeting) {
	// A meeting leaderboard, not just a heat.
	//
	// Without one the result-wrap beat has no standings to read, and it was
	// asked for exactly that: "give a proper standings update... who this run
	// moved and by how many places". An audition against a heat with no board
	// tests the wrong thing — the beat looked as though it ignored the brief
	// when in truth it had nothing to work with.
	//
	// Positioned so the heat CHANGES it: bob9's 10.88 is a new fastest lap of
	// the meeting, and his run lifts him over Willman at the top.
	mv := &ddapi.Meeting{Name: "Club Round 3", Window: 3,
		Board: []ddapi.Standing{
			{Pos: 1, Name: "Willman", ChannelLabel: "R2", Laps: 12, BestLap: 11.60, BestConsec: 35.40},
			{Pos: 2, Name: "bob9", ChannelLabel: "R1", Laps: 15, BestLap: 10.88, BestConsec: 33.35},
			{Pos: 3, Name: "Fizz", ChannelLabel: "R4", Laps: 11, BestLap: 12.88, BestConsec: 39.34},
			{Pos: 4, Name: "Wizz", ChannelLabel: "R7", Laps: 4, BestLap: 14.22, BestConsec: 0},
		}}
	r := ddapi.Race{
		ID: "test", Label: "Race 4", TypeLabel: "Race", TargetLaps: 3,
		State: "running", Round: 4, Number: 2,
		Pilots: []ddapi.Pilot{
			{Name: "bob9", ChannelLabel: "R1", Position: 1, Holeshot: 2.88,
				Laps: []float64{11.42, 10.88, 11.05}, Total: 36.23},
			{Name: "Willman", ChannelLabel: "R2", Position: 2, Holeshot: 2.41,
				Laps: []float64{12.10, 11.95, 12.30}, Total: 38.76},
			{Name: "Fizz", ChannelLabel: "R4", Position: 3, Holeshot: 3.32,
				Laps: []float64{13.05, 12.88, 13.41}, Total: 42.66},
			{Name: "Wizz", ChannelLabel: "R7", Position: 4, Holeshot: 3.01,
				Laps: []float64{14.22}, Total: 17.23},
		},
	}
	return r, mv
}

// scriptTestTruth is what a correct call must agree with, shown beside the
// output so the reader is checking against something rather than judging vibes.
func scriptTestTruth() []string {
	return []string{
		"bob9 leads — laps 11.42, 10.88, 11.05 (fastest lap of the heat: 10.88)",
		"Willman 2nd — laps 12.10, 11.95, 12.30, and took the holeshot (2.41)",
		"Fizz 3rd — laps 13.05, 12.88, 13.41",
		"Wizz 4th — one lap only (14.22), appears to have stopped",
		"",
		"Meeting leaderboard before this heat: 1 Willman (best 11.60, consec 35.40), " +
			"2 bob9 (10.88 / 33.35), 3 Fizz (12.88 / 39.34), 4 Wizz (14.22)",
		"So bob9's run should put him top on best-consecutive, and 10.88 is the " +
			"quickest lap in the meeting.",
	}
}

// testScript generates one call with one writer and returns it as JSON.
func (s *liveServer) testScript(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	race, mv := scriptTestRace()

	bc := localBooth{}
	if s.cfg != nil {
		bc = s.cfg.BoothConfig()
	}
	style := boothpkg.Style{
		Theme: bc.Theme, Humour: bc.Humour, Energy: bc.Energy,
		Language: bc.Language, Accent: boothpkg.SanitiseAccent(bc.Accent),
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	if kind == "" {
		kind = scriptTestDefaultKind
	}
	// Race or time trial changes the call completely — staggered starts, no
	// holeshot to win, best-consecutive rather than first across the line. A
	// model can be sound on one and describe the other as the wrong sport.
	if r.FormValue("format") == "tt" {
		race.TypeLabel, race.IsTT = "Time Trial", true
	}
	prompt := boothpkg.PromptStyled(kind, scriptTestContext(kind, race, mv),
		"", bc.Mode == "solo", style, "")

	// A local model has to be loaded before it can answer, and a large one takes
	// minutes — so the budget here is generous rather than a request timeout.
	ctx, cancel := timeoutCtx(6 * time.Minute)
	defer cancel()

	started := time.Now()
	var (
		text  string
		err   error
		where string
	)
	switch {
	case model == "" || model == "cloud":
		where = "the cloud"
		text, err = s.cloudWriterText(ctx, prompt)
	default:
		where = "this Mac"
		py := venvPython()
		if !fileExists(py) {
			py = lookupPython()
		}
		if py == "" {
			jsonError(w, http.StatusPreconditionRequired,
				"No Python 3 — install a local model above first.")
			return
		}
		var cl *localchat.Client
		cl, err = warmWriter(ctx, py, model)
		if err == nil {
			text, err = cl.GenerateText(ctx, prompt)
		}
	}
	took := time.Since(started)

	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	lines := boothpkg.ParseLines(text)
	if len(lines) == 0 && strings.TrimSpace(text) != "" {
		lines = []string{strings.TrimSpace(text)}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   modelLabel(model),
		"kind":    scriptKindLabel(kind),
		"where":   where,
		"seconds": fmt.Sprintf("%.1f", took.Seconds()),
		"lines":   lines,
		"raw":     text,
	})
}

// cloudWriterText asks whichever cloud writer is configured.
func (s *liveServer) cloudWriterText(ctx context.Context, prompt string) (string, error) {
	if s == nil || s.booth == nil {
		return "", fmt.Errorf("the booth is not running, so there is no cloud writer set up")
	}
	s.booth.mu.Lock()
	wr := s.booth.script
	if wr == nil {
		wr = s.booth.mainScript
	}
	if wr == nil && s.booth.gem != nil {
		wr = s.booth.gem
	}
	s.booth.mu.Unlock()
	if wr == nil {
		return "", fmt.Errorf("no cloud writer configured — add an API key above")
	}
	return wr.GenerateText(ctx, prompt)
}

// modelLabel is the friendly name, so results are comparable at a glance.
func modelLabel(id string) string {
	if id == "" || id == "cloud" {
		return "Cloud"
	}
	for _, m := range scriptModels {
		if m.ID == id {
			return m.Label
		}
	}
	return id
}

// scriptTestPanelHTML is the compare-the-writers card.
func scriptTestPanelHTML(m machineState, bc localBooth) string {
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Compare how the writers sound</h2>
 <p class="mut">Every writer is given the SAME heat, so you can read them side by side.
 The data it was handed is shown underneath &mdash; <strong>check the names against the
 times</strong>. A small model will attribute a lap to the wrong pilot in completely
 convincing prose, which is the one failure that matters here.</p>
 <p>`)

	fmt.Fprintf(&b, `<button type="button" class="st" data-model="cloud"
 style="padding:.35rem .7rem;margin:0 .3rem .3rem 0;border:0;border-radius:7px;font-weight:600;
 background:var(--accent,#b6e925);color:#111;cursor:pointer">Cloud</button>`)
	for _, st := range m.Scripts {
		if !st.Downloaded || !st.FitsRAM {
			continue
		}
		fmt.Fprintf(&b, `<button type="button" class="st" data-model="%s"
 style="padding:.35rem .7rem;margin:0 .3rem .3rem 0;border:0;border-radius:7px;font-weight:600;
 background:var(--accent,#b6e925);color:#111;cursor:pointer">%s</button>`,
			template.HTMLEscapeString(st.Spec.ID), template.HTMLEscapeString(st.Spec.Label))
	}
	b.WriteString(`</p>
 <p class="mut" style="font-size:.82rem">A local model has to load first, so the very first
 run can take a few minutes. Results stack up below so you can compare them.</p>
 <p><label style="display:inline">Format</label>
  <select id="stfmt" style="width:auto">
   <option value="race" selected>Race — wheel-to-wheel</option>
   <option value="tt">Time trial — solo against the clock</option>
  </select>
  <span class="mut" style="font-size:.8rem;margin-left:.4rem">Changes it completely: a time trial
   has staggered starts, no holeshot to win, and is scored on best consecutive laps.</span></p>
 <p><label style="display:inline">Which moment</label>
  ` + scriptKindPicker() + `
  <span class="mut" style="font-size:.8rem;margin-left:.4rem">Same heat, different moment &mdash;
   a model can be sharp on a lap call and hopeless at a result wrap.</span></p>
 <p><label style="display:inline">Speak the results in</label>
  <select id="stvoice" style="width:auto;margin-left:.4rem">` + voicePickerOptions(bc) + `</select>
  <span class="mut" style="font-size:.8rem;margin-left:.4rem">Each result gets a
   &#128266; button &mdash; the same words in a different voice land very differently.</span></p>
 <div id="stout"></div>


 <details style="margin-top:.6rem"><summary class="mut" style="cursor:pointer">
  The heat they are all describing</summary>
  <ul class="mut" style="font-size:.86rem">`)
	for _, l := range scriptTestTruth() {
		fmt.Fprintf(&b, `<li>%s</li>`, template.HTMLEscapeString(l))
	}
	b.WriteString(`</ul></details></div>` + scriptTestScript())
	return b.String()
}

func scriptTestScript() string {
	return `<script>
(function(){
 var out=document.getElementById('stout');
 document.querySelectorAll('.st').forEach(function(btn){
   btn.addEventListener('click', function(){
     var was=btn.textContent;
     btn.disabled=true; btn.textContent='Writing…';
     var card=document.createElement('div');
     card.style.cssText='border:1px solid var(--line);border-radius:9px;padding:.7rem;margin:.6rem 0';
     var kn=(document.getElementById('stkind')||{});
     var kl=kn.options?kn.options[kn.selectedIndex].text:'';
     card.innerHTML='<div class="mut">'+was+' — '+kl+' — working…</div>';
     out.insertBefore(card,out.firstChild);
     fetch('/setup/models/test-script',{method:'POST',
       headers:{'Content-Type':'application/x-www-form-urlencoded'},
       body:'model='+encodeURIComponent(btn.dataset.model)+
            '&kind='+encodeURIComponent((document.getElementById('stkind')||{}).value||'')+
            '&format='+encodeURIComponent((document.getElementById('stfmt')||{}).value||'race')})
      .then(function(r){ return r.json().then(function(j){ if(!r.ok) throw new Error(j.error||'failed'); return j; }); })
      .then(function(j){
        var h='<div style="display:flex;justify-content:space-between;align-items:baseline">'+
              '<strong>'+j.model+'</strong> <span class="mut" style="font-size:.8rem">'+
              (j.kind||'')+'</span>'+
              '<span class="mut" style="font-size:.8rem">on '+j.where+' · '+j.seconds+'s</span></div>';
        h+='<div style="margin-top:.4rem">';
        (j.lines||[]).forEach(function(l){
          h+='<div style="padding:.15rem 0">'+l.replace(/[<>&]/g,function(c){
            return {'<':'&lt;','>':'&gt;','&':'&amp;'}[c]; })+'</div>';
        });
        h+='</div>';
        // Speak THIS result, in whichever voice is picked, without saving.
        h+='<button type="button" class="sp" style="margin-top:.4rem;padding:.25rem .6rem;'+
           'border:1px solid var(--line);border-radius:6px;font-size:.78rem;background:transparent;'+
           'color:var(--fg);cursor:pointer">&#128266; Hear it</button>'+
           '<span class="mut spm" style="margin-left:.4rem;font-size:.78rem"></span>'+
           '<div class="spa" style="margin-top:.4rem"></div>';
        card.innerHTML=h;
        card.dataset.text=(j.lines||[]).join('\n');
        btn.disabled=false; btn.textContent=was;
      })
      .catch(function(e){
        card.innerHTML='<strong>'+was+'</strong><div style="color:var(--warn,#c88)">'+
          (e.message||'failed')+'</div>';
        btn.disabled=false; btn.textContent=was;
      });
   });
 });
 // One delegated handler: result cards are created after this script runs.
 document.addEventListener('click', function(ev){
   var btn=ev.target.closest && ev.target.closest('.sp');
   if(!btn) return;
   var card=btn.closest('div'); while(card && !card.dataset.text) card=card.parentElement;
   if(!card) return;
   var msg=card.querySelector('.spm'), slot=card.querySelector('.spa');
   var voice=(document.getElementById('stvoice')||{}).value||'';
   btn.disabled=true; msg.textContent='speaking…';
   fetch('/setup/models/speak',{method:'POST',
     headers:{'Content-Type':'application/x-www-form-urlencoded'},
     body:'text='+encodeURIComponent(card.dataset.text)+'&voice='+encodeURIComponent(voice)})
    .then(function(r){ if(!r.ok) return r.text().then(function(t){ throw new Error(t); }); return r.blob(); })
    .then(function(bl){ msg.textContent='';
      slot.innerHTML=''; var au=document.createElement('audio');
      au.controls=true; au.autoplay=true; au.src=URL.createObjectURL(bl); slot.appendChild(au);
      btn.disabled=false; })
    .catch(function(e){ msg.textContent='✗ '+(e.message||'failed').slice(0,120); btn.disabled=false; });
 });
})();
</script>`
}

// voicePickerOptions is the voice list for the compare panel.
// voicePickerOptions lists the cloud voices AND the venue's own recordings.
//
// The recordings were missing, which made the compare panel unable to audition
// the one voice a club actually cares about — the commentator they recorded.
// They are marked so it is clear they only work when the voice runs locally:
// the cloud has no idea what a reference recording is.
func voicePickerOptions(bc localBooth) string {
	var b strings.Builder
	if len(bc.VoiceRefs) > 0 {
		b.WriteString(`<optgroup label="Your own recordings (local voice only)">`)
		for _, v := range bc.VoiceRefs {
			sel := ""
			if v.Path == bc.VoiceRef {
				sel = " selected"
			}
			fmt.Fprintf(&b, `<option value="ref:%s"%s>%s</option>`,
				template.HTMLEscapeString(v.Path), sel, template.HTMLEscapeString(v.Name))
		}
		b.WriteString(`</optgroup><optgroup label="Qwen cloud voices">`)
		b.WriteString(qwenVoiceOptions(bc.VoicePBP, qwentts.DefaultVoicePBP, nil))
		b.WriteString(`</optgroup>`)
		return b.String()
	}
	return qwenVoiceOptions(bc.VoicePBP, qwentts.DefaultVoicePBP, nil)
}

// jsonError answers a failed test as JSON, so the panel can show the reason
// inline rather than the browser swallowing it as a bare status code.
func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ── keeping a writer warm ───────────────────────────────────────────────────
//
// Loading a model is most of the cost. Measured on a 14B: 9.3s cold, 6.5s with
// the file cache warm, against roughly 2s of actual generation — so four or five
// of those seconds were spent reading weights that had just been discarded.
//
// This is also the answer to "would llama.cpp or Ollama be faster": the problem
// was never which runtime, it was unloading the model between uses. The booth
// already keeps its own writer resident; only comparing them didn't.
//
// One model at a time, released after an idle period, because these are several
// gigabytes and a venue laptop is also running the timing.
var (
	warmMu    sync.Mutex
	warmCl    *localchat.Client
	warmModel string
	warmTimer *time.Timer
)

const warmIdle = 10 * time.Minute

func warmWriter(ctx context.Context, py, model string) (*localchat.Client, error) {
	warmMu.Lock()
	defer warmMu.Unlock()

	if warmCl != nil && warmModel == model {
		armWarmRelease()
		return warmCl, nil
	}
	// A different model: let the old one go first. Two large models resident at
	// once is how a 16GB machine starts swapping mid-comparison.
	releaseWarmLocked()

	cl, err := localchat.Start(ctx, localchat.Config{
		Python: py, Script: defaultLocalVoiceScript(), Model: model,
	})
	if err != nil {
		return nil, err
	}
	warmCl, warmModel = cl, model
	armWarmRelease()
	return cl, nil
}

// armWarmRelease frees the model after a spell of not being asked for.
func armWarmRelease() {
	if warmTimer != nil {
		warmTimer.Stop()
	}
	warmTimer = time.AfterFunc(warmIdle, func() {
		warmMu.Lock()
		defer warmMu.Unlock()
		releaseWarmLocked()
	})
}

func releaseWarmLocked() {
	if warmCl != nil {
		_ = warmCl.Close()
		warmCl, warmModel = nil, ""
	}
}

// ── hearing a generated script ──────────────────────────────────────────────

// speakText renders arbitrary text with a chosen voice.
//
// The point of comparing writers is to pick one, and reading three transcripts
// only tells you half of it — the same words land very differently in a
// different voice. So each result can be spoken, in any voice, without saving
// anything first.
func (s *liveServer) speakText(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		jsonError(w, http.StatusBadRequest, "nothing to say")
		return
	}
	// Bounded: this is an audition, and a whole meeting's transcript pasted in
	// would be a long wait and a real bill.
	if len(text) > 1200 {
		text = text[:1200]
	}
	// Speaker tags are KEPT for a cloud render.
	//
	// Stripping them and speaking the lot as one voice is what made a two-hander
	// come out as a monologue — the analyst's words were spoken, in the caller's
	// voice, so it sounded as though he never appeared. The tags are how the
	// engine knows who says what.

	voice := strings.TrimSpace(r.FormValue("voice"))
	pcm, rate, err := s.renderSpeech(r, text, voice)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(pcm) == 0 {
		jsonError(w, http.StatusInternalServerError, "it returned no audio")
		return
	}
	if rate <= 0 {
		rate = boothSampleRate
	}

	// out=speakers plays it on the LIVE bus instead of in the browser.
	//
	// The distinction has bitten before: the browser test worked perfectly for
	// a tester whose speakers were silent, and pointed the whole search away
	// from the audio path. This is the same render — same engine, same settings
	// read fresh this call, same picked voice — pushed through the same mixer
	// and device every race call uses, so what you hear is what a heat sounds
	// like, and changing the settings changes the very next test.
	if r.FormValue("out") == "speakers" {
		if s.mixer == nil {
			jsonError(w, http.StatusServiceUnavailable,
				"no audio bus — commentary or race calls have to be on first")
			return
		}
		s.mixer.enqueue(&boothCue{
			bornAt:   time.Now(),
			Kind:     "say_test",
			Text:     text,
			callKey:  SoundKey("SayTest"),
			priority: callPriorityRaceState,
			PCM:      pcm,
			Rate:     rate,
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "where": s.audioWhere(),
			"seconds": float64(len(pcm)) / float64(rate*2),
		})
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(wavFromPCM(pcm, rate))
}

// renderSpeech speaks text with whichever engine is configured, honouring a
// requested voice.
//
// Reuses the engine the booth is actually set up with, so an audition sounds
// like the real thing rather than like some other path's defaults.
func (s *liveServer) renderSpeech(r *http.Request, text, voice string) ([]byte, int, error) {
	bc := localBooth{}
	if s.cfg != nil {
		bc = s.cfg.BoothConfig()
	}
	// UNSAVED settings win when the form sends them.
	//
	// The point of an audition is hearing a change before committing to it, so
	// the Voices panel posts its controls' CURRENT values as ov_ fields and they
	// overlay the saved config for this one render. Presence-based, not
	// non-empty: clearing the accent box and pressing play must audition "no
	// accent", not silently keep the saved one.
	_ = r.ParseForm()
	ov := func(key string, dst *string) {
		if vs, ok := r.Form["ov_"+key]; ok && len(vs) > 0 {
			*dst = strings.TrimSpace(vs[0])
		}
	}
	ov("mode", &bc.Mode)
	ov("voice_color", &bc.VoiceColor)
	ov("language", &bc.Language)
	ov("accent", &bc.Accent)
	ov("energy", &bc.Energy)
	ov("style", &bc.Style)
	ctx, cancel := timeoutCtx(4 * time.Minute)
	defer cancel()

	// A "ref:" voice names one of the venue's own recordings. Chosen here, it
	// wins over the saved one — the point of an audition is trying something
	// without committing to it.
	refPath, refText := bc.VoiceRef, bc.VoiceRefText
	if strings.HasPrefix(voice, "ref:") {
		want := strings.TrimPrefix(voice, "ref:")
		for _, v := range bc.VoiceRefs {
			if v.Path == want {
				refPath, refText = v.Path, v.Text
			}
		}
		// A recording only means anything to the LOCAL model.
		bc.Engine = "local"
		voice = ""
	}
	// Picking a cloud voice means the cloud, whatever the booth is set to.
	//
	// Otherwise the audition silently ignored the choice: the local branch takes
	// no voice at all, so selecting a cloned commentator produced the local
	// model's own voice — a stranger — with nothing saying why. Symmetric with
	// "ref:" above, which forces local for the same reason in reverse.
	useLocal := bc.Engine == "local"
	if voice != "" && !strings.HasPrefix(voice, "ref:") {
		if strings.TrimSpace(bc.QwenKey) == "" {
			return nil, 0, fmt.Errorf("that is a cloud voice and no API key is saved — " +
				"add one above, or pick one of your own recordings instead")
		}
		useLocal = false
	}
	if useLocal {
		py := venvPython()
		if !fileExists(py) {
			py = lookupPython()
		}
		if py == "" {
			return nil, 0, fmt.Errorf("no Python 3 for the local voice")
		}
		// A WARM engine, held between calls.
		//
		// Starting one per render and closing it meant every audition paid the
		// model load — measured at ~3.2s for a two-second lap line, which made
		// local voicing look far slower than it is. The live announcer keeps its
		// sidecar running and streams, hitting 65ms to first audio.
		eng, err := warmSpeech(ctx, py, refPath, refText)
		if err != nil {
			return nil, 0, err
		}
		// The local model has no voice names at all, so a two-hander cannot be
		// cast: both parts would come out identical. Tags are stripped so they
		// are not read aloud, and the result is honestly a single voice.
		text = strings.Join(boothpkg.StripSpeakerTags(strings.Split(text, "\n")), "\n")
		return eng.Speak(ctx, tts.Request{Text: text})
	}

	// Cloud: the requested voice, not the saved one, so you can audition.
	if strings.TrimSpace(bc.QwenKey) == "" {
		return nil, 0, fmt.Errorf("no API key — add one above, or switch the voice to this Mac")
	}
	qc, err := qwentts.New(qwentts.Config{APIKey: bc.QwenKey, BaseURL: bc.QwenBase})
	if err != nil {
		return nil, 0, err
	}
	eng := tts.FromQwen(qc)
	if voice == "" {
		voice = bc.VoicePBP
	}
	voice = qwentts.ResolveVoice(voice, qwentts.DefaultVoicePBP)

	// Two speakers when the script has two and the venue asked for two.
	var speakers []tts.Speaker
	if bc.Mode != "solo" && strings.Contains(text, boothpkg.SpeakerColor+":") {
		speakers = []tts.Speaker{
			{Speaker: boothpkg.SpeakerPBP, Voice: voice},
			{Speaker: boothpkg.SpeakerColor,
				Voice: qwentts.ResolveVoice(bc.VoiceColor, qwentts.DefaultVoiceColor)},
		}
	} else {
		// Solo, or a one-speaker script: the tags are direction, not words, and
		// would otherwise be read out.
		text = strings.Join(boothpkg.StripSpeakerTags(strings.Split(text, "\n")), "\n")
	}
	// The beat emotion is auditionable too — it now owns the TEMPO as well as
	// the mood, so hearing calm against big-finish is hearing the pacing the
	// live booth applies per beat, not a special demo mode.
	emotion := strings.TrimSpace(r.FormValue("ov_emotion"))
	if emotion == "" {
		emotion = "tense"
	}
	return eng.Speak(ctx, tts.Request{
		Text: text, Voice: voice, Speakers: speakers, Language: bc.Language,
		Instruction: boothpkg.TTSInstructionStyled(emotion, bc.Style, true,
			boothpkg.Style{Energy: bc.Energy, Language: bc.Language,
				Accent: boothpkg.SanitiseAccent(bc.Accent)}, ""),
	})
}

// ── which moment is being written ───────────────────────────────────────────
//
// The same heat reads completely differently depending on the moment: a lineup
// before the start, a call mid-flight, a result wrap afterwards. Comparing
// writers on one beat only tells you a fraction of it — a model can be sharp
// on a lap call and hopeless at a standings summary.

const scriptTestDefaultKind = "progress"

// scriptTestKinds are the moments worth auditioning, in the order they happen.
//
// A subset of the full beat list on purpose: pilot_out, pilot_back and qual_move
// only fire on a specific event mid-heat, so asking for one against fixed data
// would produce something the booth would never actually say.
var scriptTestKinds = []struct{ Kind, Label, Note string }{
	{"meeting_open", "Before the meeting — welcome", "Opens the day."},
	{"staged", "Before the heat — the line-up", "Who is on the grid, and what is at stake."},
	{"launch", "The launch", "The gate going green."},
	{"holeshot", "The holeshot", "First to gate one."},
	{"progress", "During the heat — lap calls", "The running commentary. The most-used beat."},
	{"finish", "After the heat — the result", "Standings and what it changes. The longest call."},
	{"meeting_talk", "Between heats — standings talk", "Meeting-level conversation."},
	{"qual_move", "The qualifying leaderboard", "Board only — who moved and where they now sit."},
	{"filler", "Between heats — filler", "Talking when nothing is happening."},
}

func scriptKindLabel(kind string) string {
	for _, k := range scriptTestKinds {
		if k.Kind == kind {
			return k.Label
		}
	}
	return kind
}

// scriptTestContext assembles the data a given beat is actually handed.
//
// Mirrors what the live booth passes for each kind, so an audition is not
// flattered by richer context than the real thing gets. The lineup beat in
// particular is given lineupOnly data — no lap times exist before the start,
// and handing them over would let a model describe a race that has not run.
// scriptTestContext hands the audition to the SAME assembler the live booth
// uses, so the two cannot drift. The only difference is the extras, which are
// empty here exactly as they are live for a meeting running no bracket.
func scriptTestContext(kind string, race ddapi.Race, mv *ddapi.Meeting) string {
	x := beatExtrasCtx{}
	if kind == "qual_move" {
		// bob9's run is the move this fixture describes: 33.35 lifts him past
		// Willman's 35.40, from second to first.
		x.LadderMove = ladderMoveLine("bob9", "33.35s (best 3 consecutive)", 2, 1)
	}
	if kind == "finish" {
		// The fixture's board is stated as "before this heat", and bob9's run is
		// what changes it: 33.35 lifts him past Willman's 35.40.
		x.BoardMove = boardMoveLine([]boardMove{
			{Name: "bob9", From: 2, To: 1},
			{Name: "Willman", From: 1, To: 2},
		})
	}
	if kind == "staged" || kind == "launch" {
		// Live, these fire before the flag, when the heat genuinely has no times.
		race = preStart(race)
	}
	return beatContext(kind, race, mv, x)
}

// preStart is the heat as it stands on the grid: pilots and channels, no times.
func preStart(r ddapi.Race) ddapi.Race {
	out := r
	out.State = "staged"
	out.Pilots = make([]ddapi.Pilot, len(r.Pilots))
	for i, p := range r.Pilots {
		p.Laps = nil
		p.Total = 0
		p.Holeshot = 0
		p.Position = 0
		out.Pilots[i] = p
	}
	return out
}

// scriptKindPicker renders the moment selector.
func scriptKindPicker() string {
	var b strings.Builder
	b.WriteString(`<select id="stkind" style="width:auto">`)
	for _, k := range scriptTestKinds {
		sel := ""
		if k.Kind == scriptTestDefaultKind {
			sel = " selected"
		}
		fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`,
			template.HTMLEscapeString(k.Kind), sel, template.HTMLEscapeString(k.Label))
	}
	b.WriteString(`</select>`)
	return b.String()
}

// boothCfg is the venue's booth settings, or an empty one.
func (s *liveServer) boothCfg() localBooth {
	if s == nil || s.cfg == nil {
		return localBooth{}
	}
	return s.cfg.BoothConfig()
}

// ── a warm local voice for auditions ────────────────────────────────────────
//
// Same reasoning as warmWriter: loading the model is most of the cost, and
// throwing it away after one line means paying that every time. Keyed by the
// reference recording, so switching voices rebuilds it and nothing is ever
// spoken in the wrong one.
var (
	warmSpMu     sync.Mutex
	warmSpEng    *tts.LocalEngine
	warmSpShared bool
	warmSpRef    string
	warmSpText   string
	warmSpTimer  *time.Timer
)

const warmSpeechIdle = 10 * time.Minute

func warmSpeech(ctx context.Context, py, refPath, refText string) (*tts.LocalEngine, error) {
	warmSpMu.Lock()
	defer warmSpMu.Unlock()

	if warmSpEng != nil && warmSpRef == refPath && warmSpText == refText {
		armWarmSpeechRelease()
		return warmSpEng, nil
	}
	releaseWarmSpeechLocked() // a different voice: let the old one go first

	cfg := tts.LocalConfig{
		Python: py, Script: defaultLocalVoiceScript(), Model: voiceModel.ID,
		ReferenceAudio: refPath, ReferenceText: refText,
	}
	// The booth's engine answers auditions of the SAME voice — a fourth copy of
	// the model is how the machine ended up 11GB into swap. A different voice
	// still gets its own (that is the point of an audition), and the shared one
	// is never idle-released or closed from here: the booth owns it.
	if shared := sharedLongformEngine(cfg); shared != nil {
		warmSpEng, warmSpRef, warmSpText = shared, refPath, refText
		warmSpShared = true
		return shared, nil
	}
	eng, err := tts.StartLocal(ctx, cfg)
	if err != nil {
		return nil, err
	}
	warmSpShared = false
	warmSpEng, warmSpRef, warmSpText = eng, refPath, refText
	armWarmSpeechRelease()
	return eng, nil
}

func armWarmSpeechRelease() {
	if warmSpTimer != nil {
		warmSpTimer.Stop()
	}
	// Released when idle: this is a couple of gigabytes on a machine also running
	// the timing.
	warmSpTimer = time.AfterFunc(warmSpeechIdle, func() {
		warmSpMu.Lock()
		defer warmSpMu.Unlock()
		releaseWarmSpeechLocked()
	})
}

func releaseWarmSpeechLocked() {
	if warmSpEng != nil {
		// The booth's shared engine is only BORROWED — closing it here would
		// silence the booth mid-meeting to save memory the booth is using.
		if !warmSpShared {
			_ = warmSpEng.Close()
		}
		warmSpEng, warmSpRef, warmSpText = nil, "", ""
		warmSpShared = false
	}
}

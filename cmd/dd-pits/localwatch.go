package main

// The ▶ Watch player.
//
// Opens the stream at the moment a heat started, and — when commentary has been
// generated for that heat — offers to play it OVER the video instead of the
// broadcast's own sound. That is the local counterpart of the platform's
// in-page player, and the reason it is worth having: a club that streamed a day
// with nobody on the microphone can add the commentary afterwards.
//
// The sync is a plain mapping: the video's position minus the heat's offset is
// the position in the heat, which is what the commentary is timed against. So
// scrubbing the video scrubs the commentary with it, rather than the audio
// running off on its own clock.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/dronedash"
)

func (s *liveServer) watchPlayer(w http.ResponseWriter, r *http.Request) {
	raceID := r.URL.Query().Get("race")
	if raceID == "" {
		http.NotFound(w, r)
		return
	}

	var race *ddapi.Race
	for _, rr := range s.finishedRaces() {
		if rr.ID == raceID {
			cp := rr
			race = &cp
			break
		}
	}
	if race == nil {
		http.Error(w, "that heat is not in the current meeting", http.StatusNotFound)
		return
	}

	v := s.cfg.VideoConfig()
	off := v.offsetFor(raceID, raceStartUnix(*race))
	vid := dronedash.ParseYouTubeID(v.StreamURL)
	if vid == "" || off < 0 {
		http.Error(w, "no video is linked to this heat yet — set the stream up on the Video page",
			http.StatusPreconditionRequired)
		return
	}

	hasCommentary := false
	if s.commentary != nil {
		if c, ok := s.commentary.get(raceID); ok && c.Status == "ready" && len(c.Segments) > 0 {
			hasCommentary = true
		}
	}

	commentaryUI := `<p class="mut">No commentary has been made for this heat yet —
<a href="/setup/video">make some</a>.</p>`
	if hasCommentary {
		commentaryUI = `<label style="display:inline-flex;align-items:center;gap:.4rem;margin:0">
 <input type="checkbox" id="useCommentary" checked style="width:auto">
 <strong>🎙 Play the generated commentary</strong>
 <span class="mut">instead of the stream's own sound</span></label>`
	}

	writeSetup(w, r, "Watch", fmt.Sprintf(`
<div class="card">
 <h2>%s</h2>
 <p class="mut">Opening the stream at <code>%s</code>.</p>
 <div id="player" style="aspect-ratio:16/9;background:#000;border-radius:10px;overflow:hidden"></div>
 <p style="margin-top:.7rem">%s</p>
 <p class="mut" id="cmLine" style="min-height:1.2em"></p>
 <p><a href="/setup/video">← back to the video page</a>
  · <a href="%s" target="_blank" rel="noreferrer">open on YouTube ↗</a></p>
</div>
%s`,
		esc(chapterLabel(*race)), hhmmss(off), commentaryUI,
		esc(v.watchURLFor(raceID, raceStartUnix(*race))),
		watchScript(vid, off, raceID, hasCommentary)))
}

// watchScript embeds the YouTube player and keeps the commentary on the video's
// clock.
func watchScript(videoID string, offset int, raceID string, hasCommentary bool) string {
	return fmt.Sprintf(`
<script src="https://www.youtube.com/iframe_api"></script>
<script>
(function(){
 var OFFSET=%d, RACE=%q, HAS=%t;
 var player=null, segs=[], leadIn=0, current=null, audio=null, ready=false;

 // The commentary is timed against the HEAT, and the video is timed against the
 // broadcast. The heat starts OFFSET seconds into the video, so the position in
 // the heat is simply the video's position minus that — which is what makes
 // scrubbing the video scrub the commentary with it.
 function heatMs(){
   if(!player||!player.getCurrentTime) return -1;
   return (player.getCurrentTime()-OFFSET)*1000 - leadIn;
 }

 function stopAudio(){ if(audio){ audio.pause(); audio=null; } current=null; }

 function tick(){
   if(!ready||!HAS) return;
   var use=document.getElementById('useCommentary');
   if(use && !use.checked){ stopAudio(); return; }

   var t=heatMs();
   if(t<0){ stopAudio(); return; }

   // The latest cue whose moment has passed.
   var want=null;
   for(var i=0;i<segs.length;i++){ if(segs[i].at_ms<=t) want=segs[i]; else break; }
   if(!want){ stopAudio(); return; }
   if(current===want) return;

   current=want;
   if(audio) audio.pause();
   audio=new Audio('/commentary/'+want.file);
   // Started from where the video ALREADY is inside this line, so joining
   // mid-call or scrubbing backwards does not replay it from the top.
   var into=(t-want.at_ms)/1000;
   audio.currentTime = into>0 && into<30 ? into : 0;
   audio.play().catch(function(){});
   var line=document.getElementById('cmLine');
   if(line) line.textContent=want.text||'';
 }

 window.onYouTubeIframeAPIReady=function(){
   player=new YT.Player('player',{
     videoId:%q,
     playerVars:{start:OFFSET, rel:0, modestbranding:1},
     events:{
       onReady:function(){ ready=true; },
       onStateChange:function(e){
         // Muting the broadcast is what makes this commentary rather than two
         // voices at once.
         var use=document.getElementById('useCommentary');
         if(use&&use.checked&&player.mute) player.mute();
         if(e.data===YT.PlayerState.PAUSED||e.data===YT.PlayerState.ENDED) stopAudio();
       }
     }
   });
 };

 var use=document.getElementById('useCommentary');
 if(use) use.addEventListener('change', function(){
   if(!player) return;
   if(use.checked){ player.mute(); } else { player.unMute(); stopAudio(); }
 });

 if(HAS){
   fetch('/commentary/'+RACE+'.json')
     .then(function(r){ return r.json(); })
     .then(function(j){
       segs=(j.segments||[]).slice().sort(function(a,b){ return a.at_ms-b.at_ms; });
       leadIn=j.lead_in_ms||0;
     })
     .catch(function(){});
 }
 setInterval(tick, 250);
})();
</script>`, offset, raceID, hasCommentary, videoID)
}

// watchLinkFor is what the dashboard race cards use.
func (s *liveServer) watchLinkFor(raceID string, startMs int64) string {
	if s.cfg == nil {
		return ""
	}
	v := s.cfg.VideoConfig()
	if v.offsetFor(raceID, startMs/1000) < 0 {
		return ""
	}
	return "/watch?race=" + raceID
}

var _ = strings.TrimSpace

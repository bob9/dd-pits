package main

import (
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

// overlayGallery is /overlays: every overlay with a live preview, its settings,
// its URL with a copy button, and one-click Add to OBS.
//
// Add to OBS runs in the browser, not here: the page talks to obs-websocket on
// the operator's own machine. That is the machine OBS is on, which is the only
// place a server cannot reach — and it works the same from a public demo as
// from the timing laptop.
func (s *liveServer) overlayGallery(w http.ResponseWriter, r *http.Request) {
	base := overlayBase(r)
	msg := r.URL.Query().Get("msg")
	var pilots []string
	if mv, _, ok := s.meeting(); ok {
		for _, b := range mv.Board {
			pilots = append(pilots, b.Name)
		}
	}
	overlaySpotlight.Lock()
	pushed := append([]string(nil), overlaySpotlight.names...)
	overlaySpotlight.Unlock()
	esc := template.HTMLEscapeString

	var b strings.Builder
	b.WriteString(galleryCSS)
	if msg != "" {
		b.WriteString(`<div class="flash">` + esc(msg) + `</div>`)
	}
	b.WriteString(`<div class="card" id="obs"><h2>Add overlays to OBS</h2>
<p><b>One click:</b> connect to OBS below, then press <b>Add to OBS</b> on any overlay. It lands in the
scene you are on, full screen and transparent. <b>Or</b> press <b>Copy</b> and in OBS add a
<b>Browser</b> source, paste the URL and set it to <b>1920 × 1080</b>.</p>
<p class="mut">Open <b>Customise</b> on any overlay to change it; the preview, the URL and Add to OBS all follow.
In OBS, turn on Tools → WebSocket Server Settings → Enable WebSocket server.</p>
<div class="row" style="align-items:flex-end">
 <div><label>OBS websocket address (on this computer)</label><input type="text" id="obs-url" value="ws://localhost:4455"></div>
 <div><label>Password (blank if none)</label><input type="password" id="obs-pw"></div>
 <div style="flex:0"><button type="button" id="obs-test">Connect</button></div>
</div>
<p id="obs-status" class="mut" style="margin-top:.6rem">Not connected yet.</p></div>`)

	group := ""
	for i, k := range overlayKinds {
		if k.Group != group {
			if group != "" {
				b.WriteString(`</div>`)
			}
			group = k.Group
			b.WriteString(`<div class="grp">` + esc(group) + `</div><div class="ovg">`)
		}
		path := "/overlay/" + k.Slug + k.Ref + k.Query
		b.WriteString(`<div class="card ovc" data-slug="` + esc(k.Slug) + `" data-name="dd-pits ` + esc(k.Title) + `" id="ov-` + strconv.Itoa(i) + `">
<div class="ovp"><iframe loading="lazy" src="` + esc(path) + `" title="` + esc(k.Title) + ` preview"></iframe></div>
<div class="ovm"><span class="reg">` + esc(k.Region) + `</span><h2>` + esc(k.Title) + `</h2><p class="mut">` + esc(k.What) + `</p>`)
		if len(k.Params) > 0 {
			b.WriteString(`<details class="cust"><summary>Customise</summary><div class="cf">`)
			for _, p := range k.Params {
				def := p.Default
				if p.Key == "_ref" && def == "" {
					def = strings.TrimPrefix(k.Ref, "/")
				}
				b.WriteString(`<label>` + esc(p.Label))
				switch p.Kind {
				case "select":
					b.WriteString(`<select data-k="` + esc(p.Key) + `">`)
					for _, o := range p.Options {
						sel := ""
						if o == def {
							sel = " selected"
						}
						label := o
						if o == "" {
							label = "Default"
						}
						b.WriteString(`<option value="` + esc(o) + `"` + sel + `>` + esc(label) + `</option>`)
					}
					b.WriteString(`</select>`)
				case "check":
					b.WriteString(` <input type="checkbox" data-k="` + esc(p.Key) + `" value="1" style="width:auto">`)
				case "color":
					b.WriteString(`<input type="text" data-k="` + esc(p.Key) + `" placeholder="#c6ff3d" value="` + esc(def) + `">`)
				case "number":
					b.WriteString(`<input type="number" min="0" data-k="` + esc(p.Key) + `" placeholder="` + esc(def) + `">`)
				case "rows":
					b.WriteString(`<textarea rows="4" data-k="` + esc(p.Key) + `" data-rows="1">` + esc(strings.ReplaceAll(def, `\n`, "\n")) + `</textarea>`)
				case "pilots":
					b.WriteString(`<span class="pl" data-k="` + esc(p.Key) + `" data-pilots="1">`)
					for _, n := range pilots {
						b.WriteString(`<label class="chk"><input type="checkbox" value="` + esc(n) + `">` + esc(n) + `</label>`)
					}
					if len(pilots) == 0 {
						b.WriteString(`<span class="mut">Pilots appear once a meeting is open.</span>`)
					}
					b.WriteString(`</span>`)
				default:
					b.WriteString(`<input type="text" data-k="` + esc(p.Key) + `" value="` + esc(def) + `">`)
				}
				if p.Hint != "" {
					b.WriteString(`<small>` + esc(p.Hint) + `</small>`)
				}
				b.WriteString(`</label>`)
			}
			b.WriteString(`</div></details>`)
		}
		b.WriteString(`<div class="ovu"><code data-url>` + esc(base+path) + `</code></div>
<div class="acts"><button type="button" class="sec" data-act="copy">Copy</button><button type="button" data-act="obs">Add to OBS</button>
<a class="btn" data-act="open" href="` + esc(base+path) + `" target="_blank" rel="noopener">Open</a></div></div></div>`)
		if k.Slug == "spotlight" {
			b.WriteString(`<div class="card" style="padding:1rem 1.1rem"><h2>Spotlight control</h2>
<p class="mut">Tick one pilot for a profile or two for head to head, then push. Every spotlight overlay without pilots in its URL follows this.</p>
<form method="post" action="/overlays/spotlight">`)
			on := map[string]bool{}
			for _, p := range pushed {
				on[strings.ToUpper(p)] = true
			}
			if len(pilots) == 0 {
				b.WriteString(`<p class="mut">Pilots appear here once FPVTrackside has a meeting open.</p>`)
			}
			for _, p := range pilots {
				chk := ""
				if on[strings.ToUpper(p)] {
					chk = " checked"
				}
				b.WriteString(`<label class="chk"><input type="checkbox" name="p" value="` + esc(p) + `"` + chk + `>` + esc(p) + `</label>`)
			}
			b.WriteString(`<p style="margin-top:.7rem"><button>📡 Push to stream</button> <button class="sec" name="clear" value="1">Take off air</button></p></form></div>`)
		}
	}
	b.WriteString(`</div>
<div class="card"><h2>Not in dd-pits yet</h2><p class="mut">The platform's bracket overlays (bracket diagram, mains and division standings)
need race brackets, and the 3D track needs a 3D course. Neither exists in dd-pits yet.</p></div>
<script>var DD_BASE=` + jsString(base) + `;</script>` + galleryJS)
	writeSetup(w, r, "OBS overlays", b.String())
}

func jsString(s string) string {
	return `"` + template.JSEscapeString(s) + `"`
}

const galleryCSS = `<style>
.ovg{display:grid;gap:1rem;grid-template-columns:repeat(auto-fill,minmax(20rem,1fr));margin-bottom:1.2rem}
.ovg .card{margin:0;padding:0;overflow:hidden;display:flex;flex-direction:column}
.ovp{position:relative;aspect-ratio:16/9;background:#000 url(/assets/stream.webp) center/cover;overflow:hidden}
.ovp iframe{position:absolute;left:0;top:0;width:1920px;height:1080px;border:0;transform-origin:0 0;pointer-events:none;background:transparent}
.ovm{padding:.9rem 1rem 1rem;display:flex;flex-direction:column;flex:1}
.ovm h2{margin:0 0 .25rem}
.ovm .reg{color:var(--acc);font-size:.72rem;font-weight:700;letter-spacing:.08em;text-transform:uppercase}
.ovu{display:flex;gap:.4rem;margin:.6rem 0 .1rem;align-items:center}
.ovu code{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:.8rem;padding:.4rem .5rem}
.ovm .acts{display:flex;gap:.4rem;margin-top:auto;padding-top:.6rem;flex-wrap:wrap}
.ovm button,.ovm a.btn{padding:.42rem .85rem;font-size:.85rem}
a.btn{display:inline-block;border-radius:999px;background:rgba(255,255,255,.08);color:#e6e9ee;border:1px solid var(--line);text-decoration:none;font-weight:700}
.grp{font-size:.78rem;letter-spacing:.14em;text-transform:uppercase;color:var(--mut);font-weight:700;margin:1.4rem 0 .6rem}
.flash{background:rgba(198,255,61,.12);border:1px solid rgba(198,255,61,.35);color:#e9ffc0;padding:.7rem 1rem;border-radius:14px;margin-bottom:1rem}
.chk{display:inline-flex!important;align-items:center;gap:.35rem;margin:.2rem .7rem .2rem 0!important;color:var(--fg)!important;font-size:.88rem!important}
.chk input{width:auto}
.cust{margin:.4rem 0 .2rem;border:1px solid var(--line);border-radius:12px;padding:.4rem .7rem}
.cust summary{cursor:pointer;font-weight:700;color:var(--acc);font-size:.9rem}
.cf label{margin:.55rem 0 0;font-size:.82rem}
.cf small{display:block;color:var(--mut);font-size:.75rem;margin-top:.15rem}
.cf input,.cf select,.cf textarea{margin-top:.2rem;padding:.4rem .55rem;font-size:.88rem}
#obs-status.ok{color:var(--acc)}#obs-status.bad{color:#ff8a7a}
</style>`

const galleryJS = `<script>
(function(){
/* Previews: scale the 1920×1080 overlays into their cards. */
function fit(){document.querySelectorAll('.ovp iframe').forEach(function(f){f.style.transform='scale('+(f.parentElement.clientWidth/1920)+')'})}
addEventListener('resize',fit);fit();

/* Each card builds its URL from its settings. */
function build(card){
  var slug=card.dataset.slug,ref='',q=new URLSearchParams();
  card.querySelectorAll('[data-k]').forEach(function(el){
    var k=el.dataset.k;
    if(el.dataset.pilots){el.querySelectorAll('input:checked').forEach(function(c){q.append(k,c.value)});return}
    if(el.dataset.rows){el.value.split('\n').forEach(function(l){l=l.trim();if(l)q.append(k,l)});return}
    var v=el.type==='checkbox'?(el.checked?'1':''):el.value.trim();
    if(k==='_ref'){ref=v;return}
    if(v!=='')q.append(k,v);
  });
  var qs=q.toString();
  return '/overlay/'+slug+(ref?'/'+ref:'')+(qs?'?'+qs:'');
}
var timers=new WeakMap();
function update(card){
  var path=build(card),full=DD_BASE+path;
  card.dataset.path=path;
  card.querySelector('[data-url]').textContent=full;
  card.querySelector('[data-act=open]').href=full;
  clearTimeout(timers.get(card));
  timers.set(card,setTimeout(function(){card.querySelector('iframe').src=path},350));
}
document.querySelectorAll('.ovc').forEach(function(card){
  card.querySelectorAll('[data-k],[data-k] input').forEach(function(el){
    el.addEventListener('input',function(){update(card)});el.addEventListener('change',function(){update(card)});
  });
  update(card);
});

/* Copy. */
document.addEventListener('click',async function(e){
  var b=e.target.closest('[data-act=copy]');if(!b)return;
  var t=b.closest('.ovc').querySelector('[data-url]').textContent;
  try{await navigator.clipboard.writeText(t)}catch(_){var i=document.createElement('textarea');i.value=t;document.body.append(i);i.select();document.execCommand('copy');i.remove()}
  var o=b.textContent;b.textContent='Copied ✓';setTimeout(function(){b.textContent=o},1600);
});

/* ── obs-websocket v5, from the browser ── */
function sha256(bytes){
  var K=[0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2];
  var H=[0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19];
  var l=bytes.length,n=((l+9+63)>>6)<<6,m=new Uint8Array(n);m.set(bytes);m[l]=0x80;
  var bits=l*8;for(var i=0;i<8;i++)m[n-1-i]=(bits/Math.pow(2,8*i))&0xff;
  var w=new Int32Array(64),r=function(x,c){return (x>>>c)|(x<<(32-c))};
  for(var o=0;o<n;o+=64){
    for(i=0;i<16;i++)w[i]=(m[o+4*i]<<24)|(m[o+4*i+1]<<16)|(m[o+4*i+2]<<8)|m[o+4*i+3];
    for(i=16;i<64;i++){var s0=r(w[i-15],7)^r(w[i-15],18)^(w[i-15]>>>3),s1=r(w[i-2],17)^r(w[i-2],19)^(w[i-2]>>>10);w[i]=(w[i-16]+s0+w[i-7]+s1)|0}
    var a=H[0],b=H[1],c=H[2],d=H[3],e=H[4],f=H[5],g=H[6],h=H[7];
    for(i=0;i<64;i++){var S1=r(e,6)^r(e,11)^r(e,25),ch=(e&f)^(~e&g),t1=(h+S1+ch+K[i]+w[i])|0,S0=r(a,2)^r(a,13)^r(a,22),mj=(a&b)^(a&c)^(b&c),t2=(S0+mj)|0;
      h=g;g=f;f=e;e=(d+t1)|0;d=c;c=b;b=a;a=(t1+t2)|0}
    H[0]=(H[0]+a)|0;H[1]=(H[1]+b)|0;H[2]=(H[2]+c)|0;H[3]=(H[3]+d)|0;H[4]=(H[4]+e)|0;H[5]=(H[5]+f)|0;H[6]=(H[6]+g)|0;H[7]=(H[7]+h)|0}
  var out=new Uint8Array(32);for(i=0;i<8;i++){out[4*i]=H[i]>>>24;out[4*i+1]=(H[i]>>>16)&255;out[4*i+2]=(H[i]>>>8)&255;out[4*i+3]=H[i]&255}
  return out;
}
function b64(u8){var s='';u8.forEach(function(x){s+=String.fromCharCode(x)});return btoa(s)}
function utf8(s){return new TextEncoder().encode(s)}
function obsConnect(url,pw){
  return new Promise(function(res,rej){
    var ws;try{ws=new WebSocket(url,'obswebsocket.json')}catch(e){rej(e);return}
    var pend={},seq=0,done=false;
    var to=setTimeout(function(){if(!done){done=true;rej(new Error('OBS did not answer at '+url));try{ws.close()}catch(_){}}},6000);
    ws.onerror=function(){if(!done){done=true;clearTimeout(to);rej(new Error('could not reach OBS at '+url))}};
    ws.onmessage=function(ev){var m=JSON.parse(ev.data);
      if(m.op===0){var d={rpcVersion:1,eventSubscriptions:0};
        if(m.d.authentication){var a=m.d.authentication,sec=b64(sha256(utf8(pw+a.salt)));d.authentication=b64(sha256(utf8(sec+a.challenge)))}
        ws.send(JSON.stringify({op:1,d:d}))}
      else if(m.op===2){done=true;clearTimeout(to);
        res({req:function(type,data){return new Promise(function(ok,bad){var id='dd'+(++seq);pend[id]=[ok,bad];ws.send(JSON.stringify({op:6,d:{requestType:type,requestId:id,requestData:data||{}}}))})},close:function(){ws.close()}})}
      else if(m.op===7){var p=pend[m.d.requestId];if(!p)return;delete pend[m.d.requestId];
        if(m.d.requestStatus.result)p[0](m.d.responseData||{});else{var er=new Error(m.d.requestStatus.comment||('code '+m.d.requestStatus.code));er.code=m.d.requestStatus.code;p[1](er)}}};
    ws.onclose=function(ev){if(!done){done=true;clearTimeout(to);rej(new Error(ev.code===4009?'wrong OBS password':'OBS closed the connection ('+ev.code+')'))}};
  });
}
var urlEl=document.getElementById('obs-url'),pwEl=document.getElementById('obs-pw'),st=document.getElementById('obs-status');
try{urlEl.value=localStorage.getItem('dd-obs-url')||urlEl.value;pwEl.value=localStorage.getItem('dd-obs-pw')||''}catch(_){}
function save(){try{localStorage.setItem('dd-obs-url',urlEl.value.trim());localStorage.setItem('dd-obs-pw',pwEl.value)}catch(_){}}
function say(t,c){st.textContent=t;st.className=c||'mut'}
document.getElementById('obs-test').addEventListener('click',async function(){
  save();say('Connecting…');
  try{var o=await obsConnect(urlEl.value.trim(),pwEl.value);var v=await o.req('GetVersion');o.close();
    say('Connected to OBS '+(v.obsVersion||'')+'. Press Add to OBS on any overlay.','ok')}
  catch(e){say(e.message+'. In OBS: Tools → WebSocket Server Settings → Enable WebSocket server, and check the password.','bad')}
});
document.addEventListener('click',async function(e){
  var b=e.target.closest('[data-act=obs]');if(!b)return;
  var card=b.closest('.ovc'),name=card.dataset.name,src=location.origin+card.dataset.path,o0=b.textContent;
  save();b.textContent='Adding…';
  try{
    var o=await obsConnect(urlEl.value.trim(),pwEl.value);
    var sc=await o.req('GetCurrentProgramScene');var scene=sc.currentProgramSceneName||sc.sceneName;
    var settings={url:src,width:1920,height:1080,shutdown:false,restart_when_active:false};
    try{await o.req('CreateInput',{sceneName:scene,inputName:name,inputKind:'browser_source',inputSettings:settings,sceneItemEnabled:true});
      say('Added “'+name+'” to the “'+scene+'” scene.','ok')}
    catch(err){
      await o.req('SetInputSettings',{inputName:name,inputSettings:settings});
      try{await o.req('GetSceneItemId',{sceneName:scene,sourceName:name})}catch(_){await o.req('CreateSceneItem',{sceneName:scene,sourceName:name,sceneItemEnabled:true})}
      say('“'+name+'” is in the “'+scene+'” scene, updated to these settings.','ok')}
    o.close();b.textContent='Added ✓';
  }catch(err){b.textContent=o0;say('Could not add it: '+err.message+'. Check the OBS connection at the top of this page.','bad');document.getElementById('obs').scrollIntoView({behavior:'smooth'});return}
  setTimeout(function(){b.textContent=o0},1800);
});
})();
</script>`

package main

import (
	"net"
)

// lanIP is this machine's first private IPv4 address, for overlay URLs that
// must work from a streaming PC on the venue network.
func lanIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip := ipn.IP.To4(); ip != nil && !ip.IsLoopback() && ip.IsPrivate() {
				return ip.String()
			}
		}
	}
	return ""
}

// overlayTemplates are parsed alongside pageTemplates. Every overlay is an
// "ov-<slug>" page plus its "ov-<slug>-body" partial for the soft refresh.
const overlayTemplates = `
{{define "ov-head"}}<!doctype html><html><head><meta charset="utf-8"><title>dd-pits overlay</title>{{template "css" .}}{{template "ov-css" .}}</head>
<body class="overlay ovb-{{.Slug}}{{if eq (.Q.Get "still") "1"}} still sbshow{{end}}{{if eq .Bg "scrim"}} bg-scrim{{else if eq .Bg "transparent"}} bg-clear{{else if .Bg}} bg-scene{{end}}">{{end}}

{{define "ov-foot"}}<script>
/* The wall clock ticks outside #app, so the soft refresh never resets it. */
(function(){
  function pad(n){return (n<10?'0':'')+n}
  var D=['SUNDAY','MONDAY','TUESDAY','WEDNESDAY','THURSDAY','FRIDAY','SATURDAY'];
  function tick(){var d=new Date(),h=d.getHours(),ap=h<12?'AM':'PM',h12=h%12||12;
    document.querySelectorAll('[data-wall]').forEach(function(e){e.textContent=pad(h)+':'+pad(d.getMinutes())+':'+pad(d.getSeconds())});
    document.querySelectorAll('[data-wall12]').forEach(function(e){e.textContent=pad(h12)+':'+pad(d.getMinutes())+':'+pad(d.getSeconds())+' '+ap});
    document.querySelectorAll('[data-day]').forEach(function(e){e.textContent=D[d.getDay()]});}
  tick();setInterval(tick,250);
  /* Long lists scroll by themselves: down, pause, back up. */
  var dir={};setInterval(function(){document.querySelectorAll('[data-autoscroll]').forEach(function(el,i){
    var max=el.scrollHeight-el.clientHeight;if(max<=4)return;var s=dir[i]||{v:1,hold:60};
    if(s.hold>0){s.hold--;dir[i]=s;return}el.scrollTop+=s.v;
    if(el.scrollTop>=max-1&&s.v>0){s.v=-1;s.hold=90}else if(el.scrollTop<=0&&s.v<0){s.v=1;s.hold=90}dir[i]=s})},33);
})();
</script>{{template "js" (dictRefresh 800 .)}}</body></html>{{end}}

{{define "ov-css"}}<style>
html{font-size:calc(100vw / 120)}
:root{--ova:{{if .OvAccent}}{{.OvAccent}}{{else}}var(--accent){{end}}}
body.overlay{margin:0;overflow:hidden;font-variant-numeric:tabular-nums;-webkit-font-smoothing:antialiased;color:#f4f6f8}
body.bg-scene{background:radial-gradient(70rem 40rem at 50% 0,color-mix(in srgb,var(--ova) 14%,transparent),transparent 70%),#0a0c10!important}
body.bg-scrim{background:rgba(5,6,8,.72)!important}
.ovx{position:fixed;font-size:1rem}
.glass{background:linear-gradient(180deg,rgba(16,18,23,.94),rgba(8,9,12,.9));border:1px solid rgba(255,255,255,.1);
  box-shadow:0 1.2rem 3rem -1rem rgba(0,0,0,.85),0 0 0 1px color-mix(in srgb,var(--ova) 8%,transparent)}
.ovx .pos{min-width:1.9rem;font-size:.95rem;border-radius:.5rem;padding:.08rem .3rem}
.lv{font-size:.62rem;font-weight:800;letter-spacing:.12em;color:#07080a;background:var(--ova);padding:.14rem .45rem;border-radius:.35rem}
.clk{font-weight:800;color:var(--ova);font-size:1.25rem}
.cb{width:.28rem;height:1.35rem;border-radius:.15rem;flex:none;display:inline-block}
.mut{color:#98a1ae}
img.lg{width:1.5rem;height:1.5rem;border-radius:.4rem}
.acc{color:var(--ova)}
.av{width:2.6rem;height:2.6rem;border-radius:50%;display:grid;place-items:center;font-weight:800;font-size:1.1rem;flex:none;
  background:radial-gradient(circle at 30% 25%,rgba(255,255,255,.18),rgba(255,255,255,.03));box-shadow:0 0 0 .2rem var(--cc,#888),0 0 1rem color-mix(in srgb,var(--cc,#888) 45%,transparent)}
.chp{font-weight:800;font-size:.8rem;padding:.1rem .45rem;border-radius:.4rem;color:#07080a;background:var(--cc,#888)}
.st-live{color:#ff4d4d}.st-live::before{content:"";display:inline-block;width:.55rem;height:.55rem;border-radius:50%;background:#ff4d4d;margin-right:.4rem;animation:ovpulse 1.4s infinite;vertical-align:.05rem}
@keyframes ovpulse{0%{box-shadow:0 0 0 0 rgba(255,77,77,.7)}70%{box-shadow:0 0 0 .55rem rgba(255,77,77,0)}100%{box-shadow:0 0 0 0 rgba(255,77,77,0)}}
/* ?still=1 freezes every animation, for a static frame or a slow streaming PC. */
body.still *,body.still *::before{animation:none!important;transition:none!important;opacity:1}
@keyframes walkon{from{opacity:0;transform:translateY(1.6rem) scale(.96)}to{opacity:1;transform:none}}
.walk{animation:walkon .7s cubic-bezier(.2,.7,.2,1) both}
/* scorebug */
.sb{left:2.2rem;top:2.2rem}
.sb .badge{display:flex;align-items:stretch;border-radius:1rem;overflow:hidden}
.sb .badge .lgw{display:grid;place-items:center;padding:.6rem .8rem;background:rgba(255,255,255,.04)}
.sb .badge .lgw img{width:2.6rem;height:2.6rem;border-radius:.6rem}
.sb .badge .tt{padding:.55rem 1.1rem .45rem}
.sb .badge .tt b{display:block;font-size:1.25rem;letter-spacing:-.01em;white-space:nowrap}
.sb .badge .bar{display:flex;gap:.6rem;margin-top:.3rem;font-size:.78rem;font-weight:800;letter-spacing:.08em;color:#07080a;background:var(--ova);padding:.18rem .55rem;border-radius:.35rem;width:max-content}
.sb .strip{margin-top:.5rem;border-radius:.9rem;padding:.55rem 1rem;display:flex;align-items:center;gap:.7rem;
  transform:translateX(-120%);opacity:0;transition:transform .6s cubic-bezier(.2,.7,.2,1),opacity .6s}
body.sbshow .sb .strip{transform:none;opacity:1}
.sb .strip .k{font-size:.72rem;font-weight:800;letter-spacing:.12em;text-transform:uppercase}
.sb .strip b{font-size:1.05rem}
/* top bar */
.tb{left:0;right:0;top:0;height:5.75rem;display:grid;grid-template-columns:1fr auto 1fr;align-items:center;padding:0 1.8rem;border-radius:0;border-width:0 0 1px}
.tb .l{display:flex;align-items:center;gap:.9rem}
.tb .pill{font-size:.85rem;font-weight:800;letter-spacing:.1em;text-transform:uppercase;padding:.35rem .8rem;border-radius:999px;background:rgba(255,255,255,.07)}
.tb .clk{font-size:1.9rem}
.tb .c{text-align:center}
.tb .c .ev{color:#98a1ae;font-size:.85rem;font-weight:700;letter-spacing:.1em;text-transform:uppercase}
.tb .c b{display:block;font-size:1.6rem;letter-spacing:-.02em}
.tb .r{display:flex;align-items:center;gap:.7rem;justify-content:flex-end;font-weight:800;font-size:1.1rem}
.tb .r img{width:2.6rem;height:2.6rem;border-radius:.6rem}
/* corner */
.co{left:2rem;top:2rem;border-radius:.9rem;padding:.55rem 1rem;display:flex;align-items:center;gap:.8rem;border-left:.3rem solid var(--ova)}
.co img{width:2rem;height:2rem;border-radius:.5rem}
.co .w{font-weight:800;font-size:1.25rem}
.co .h{font-weight:700;font-size:1rem;padding-left:.8rem;border-left:1px solid rgba(255,255,255,.12)}
.co .h.done{color:#98a1ae}.co .h.next{color:var(--ova)}
/* race grid */
.rg{inset:0;display:grid;gap:0}
.rg .tile{position:relative;border:.2rem solid var(--cc);box-shadow:inset 0 0 0 1px rgba(0,0,0,.4)}
.rg .tile .bt{position:absolute;left:0;right:0;bottom:0;display:flex;align-items:center;gap:.7rem;padding:.6rem .9rem;
  background:linear-gradient(0deg,rgba(6,7,9,.92),rgba(6,7,9,.6));}
.rg .tile .bt b{font-size:1.3rem}
.rg .tile .bt .ps{margin-left:auto;font-size:1.6rem;font-weight:800}
.rg .tile .lps{position:absolute;right:.8rem;top:.8rem;display:flex;flex-direction:column;gap:.3rem;align-items:flex-end}
.rg .tile .lp{display:flex;gap:.5rem;align-items:baseline;padding:.25rem .55rem;border-radius:.5rem;background:rgba(6,7,9,.78);font-size:1rem;animation:walkon .4s both}
.rg .tile .lp em{font-style:normal;font-size:.75rem;color:#98a1ae}
.rg .tile .lp.best{color:var(--ova);font-weight:800}
.rg .tick{position:fixed;left:0;right:0;bottom:0;height:2.4rem;overflow:hidden;background:rgba(6,7,9,.9);border-top:1px solid rgba(255,255,255,.1);display:none}
.rg-on .rg .tick{display:block}
.rg .tick .tr{display:inline-flex;gap:2.5rem;white-space:nowrap;padding:.55rem 0 0 100%;animation:marq 28s linear infinite;font-weight:700}
@keyframes marq{to{transform:translateX(-100%)}}
/* node */
.nd{left:1.5rem;bottom:1.5rem;border-radius:1rem;overflow:hidden;display:flex;align-items:stretch;border:1px solid var(--cc);animation:walkon .6s both}
.nd .ps{display:grid;place-items:center;padding:0 1rem;font-size:1.9rem;font-weight:800;line-height:1}
.nd .ps small{display:block;font-size:.7rem;color:#98a1ae;text-align:center}
.nd .ch{display:grid;place-items:center;padding:0 .8rem;font-weight:800;font-size:1.1rem;color:#07080a;background:var(--cc)}
.nd .bd{padding:.55rem 1rem}
.nd .bd b{font-size:1.5rem;display:block;line-height:1.1}
.nd .bd .m{display:flex;gap:1.1rem;color:#98a1ae;font-size:.85rem;margin-top:.25rem}
.nd .bd .m span b{display:inline;font-size:1rem;color:#f4f6f8}
/* strip */
.sp2{bottom:2rem;border-radius:1.1rem;padding:.8rem 1rem;display:flex;align-items:center;gap:1rem}
.sp2.cur{right:2rem}.sp2.nxt{left:2rem}
.sp2 .hd{display:flex;flex-direction:column;gap:.25rem;padding-right:1rem;border-right:1px solid rgba(255,255,255,.1)}
.sp2 .hd .k{font-size:.72rem;font-weight:800;letter-spacing:.12em;text-transform:uppercase}
.sp2 .hd b{font-size:1.1rem;white-space:nowrap}
.sp2 .p{display:flex;flex-direction:column;align-items:center;gap:.35rem;min-width:5.5rem}
.sp2 .p b{font-size:.95rem}
/* lap chart */
.lc{right:2rem;top:50%;transform:translateY(-50%);border-radius:1.1rem;overflow:hidden;min-width:30rem}
.lc .h{display:flex;align-items:center;gap:.6rem;padding:.6rem .9rem;border-bottom:1px solid rgba(255,255,255,.08)}
.lc .h .clk{margin-left:auto}
.lc .r{display:flex;align-items:center;gap:.55rem;padding:.5rem .9rem;border-top:1px solid rgba(255,255,255,.05)}
.lc .r b{width:8rem;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.lc .ls{display:flex;gap:.3rem;flex:1}
.lc .l{font-size:.85rem;padding:.12rem .4rem;border-radius:.4rem;background:rgba(255,255,255,.06);min-width:3.6rem;text-align:center}
.lc .l.best{background:color-mix(in srgb,var(--ova) 18%,transparent);color:var(--ova);font-weight:800}
/* fastest */
.fl{right:2rem;bottom:2rem;border-radius:1.2rem;padding:1rem 1.3rem;min-width:20rem;border-left:.3rem solid var(--ova)}
.fl .k{font-size:.72rem;letter-spacing:.14em;font-weight:800;color:var(--ova);text-transform:uppercase}
.fl .t{font-size:2.6rem;font-weight:800;letter-spacing:-.03em;line-height:1.05;margin:.2rem 0}
.fl .w{display:flex;align-items:center;gap:.5rem;font-size:1.1rem}
/* lower third */
.l3{left:2rem;right:2rem;bottom:2rem;display:grid;grid-template-columns:repeat(4,1fr);gap:.8rem}
.l3 .c{border-radius:1rem;padding:.75rem 1rem;display:grid;grid-template-columns:auto 1fr auto;align-items:center;gap:.25rem .7rem;border-top:.25rem solid var(--cc)}
.l3 .c b{font-size:1.3rem;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.l3 .c .lt{grid-column:1/-1;display:flex;justify-content:space-between;color:#98a1ae;font-size:.85rem}
.l3 .c .lt span:last-child{color:#f4f6f8;font-weight:800;font-size:1.05rem}
/* lineup */
.lu{inset:0;padding:3.5rem 4.5rem;display:flex;flex-direction:column}
.lu .hd{display:flex;align-items:center;gap:1.2rem;margin-bottom:2.2rem}
.lu .hd img{width:4rem;height:4rem;border-radius:1rem}
.lu .hd .ev{color:#98a1ae;font-weight:700;letter-spacing:.12em;text-transform:uppercase;font-size:.9rem}
.lu .hd h1{margin:.1rem 0 0;font-size:3.4rem;letter-spacing:-.045em;line-height:1}
.lu .hd .pill{margin-left:auto;font-size:1rem;font-weight:800;letter-spacing:.12em;text-transform:uppercase;padding:.5rem 1.1rem;border-radius:999px;background:rgba(255,255,255,.08)}
.lu .g{display:grid;grid-template-columns:repeat(var(--n,4),1fr);gap:1.2rem;flex:1;align-items:stretch}
.lu .cd{border-radius:1.6rem;padding:1.8rem 1.4rem;display:flex;flex-direction:column;align-items:center;text-align:center;border-top:.35rem solid var(--cc)}
.lu .cd .av{width:9rem;height:9rem;font-size:4rem;margin-bottom:1.2rem;box-shadow:0 0 0 .35rem var(--cc),0 0 3rem color-mix(in srgb,var(--cc) 50%,transparent)}
.lu .cd b{font-size:2.1rem;letter-spacing:-.03em}
.lu .cd .chp{margin:.6rem 0 1.2rem;font-size:1rem}
.lu .cd .stats{display:grid;grid-template-columns:1fr 1fr;gap:.6rem;width:100%;margin-top:auto}
.lu .cd .stats div{background:rgba(255,255,255,.05);border-radius:.8rem;padding:.6rem}
.lu .cd .stats small{display:block;color:#98a1ae;font-size:.72rem;letter-spacing:.08em;text-transform:uppercase}
.lu .cd .stats span{font-size:1.3rem;font-weight:800}
.lu .cd .big{font-size:4.2rem;font-weight:800;letter-spacing:-.04em;line-height:1;color:var(--ova)}
.lu .cd .chips{display:flex;gap:.35rem;flex-wrap:wrap;justify-content:center;margin-top:.8rem}
.lu .cd .chips span{font-size:.9rem;padding:.15rem .45rem;border-radius:.4rem;background:rgba(255,255,255,.07)}
.lu .cd .chips span.best{color:var(--ova);background:color-mix(in srgb,var(--ova) 16%,transparent);font-weight:800}
/* up now + banner */
.un{left:50%;top:50%;transform:translate(-50%,-50%);display:flex;gap:1.2rem}
.un .cd,.bn .cd{border-radius:1.4rem;padding:1.4rem 1.3rem;width:16rem;display:flex;flex-direction:column;align-items:center;text-align:center;border-bottom:.3rem solid var(--cc);
  animation:upnow 12s cubic-bezier(.2,.7,.2,1) infinite both}
@keyframes upnow{0%{opacity:0;transform:translateY(2.4rem) scale(.95)}6%{opacity:1;transform:none}80%{opacity:1;transform:none}88%,100%{opacity:0;transform:translateY(-1.5rem)}}
.un .cd .av{width:6rem;height:6rem;font-size:2.6rem;margin-bottom:.9rem}
.un .cd b{font-size:1.6rem}
.un .cd .chp{margin:.5rem 0 .9rem}
.un .cd .m{display:flex;gap:1rem;color:#98a1ae;font-size:.8rem}
.un .cd .m span b{display:block;color:#f4f6f8;font-size:1.05rem}
.un .k,.bn .k{position:fixed;left:50%;transform:translateX(-50%);font-size:.9rem;font-weight:800;letter-spacing:.2em;color:var(--ova);text-transform:uppercase}
.un .k{top:calc(50% - 13rem)}
.bn{left:2rem;right:2rem;bottom:2rem;display:flex;gap:.9rem;justify-content:center}
.bn .k{bottom:12.5rem}
.bn .cd{flex-direction:row;width:auto;min-width:16rem;padding:.8rem 1.1rem;gap:.8rem;text-align:left}
.bn .cd b{font-size:1.25rem;display:block}
.bn .cd small{color:#98a1ae}
/* leaderboard */
.lbx{inset:3rem 4rem;border-radius:1.6rem;display:flex;flex-direction:column;overflow:hidden}
.lbx .h{display:flex;align-items:center;gap:.9rem;padding:1.2rem 1.6rem;background:linear-gradient(90deg,color-mix(in srgb,var(--ova) 20%,transparent),transparent 70%)}
.lbx .h img{width:2.6rem;height:2.6rem;border-radius:.6rem}
.lbx .h b{font-size:1.9rem;letter-spacing:-.03em}
.lbx .h .mut{margin-left:auto;font-weight:700}
.lbx .th,.lbx .r{display:grid;grid-template-columns:5.5rem 4rem 1fr 4.5rem 6rem 9rem 9rem;align-items:center;gap:.8rem;padding:.55rem 1.6rem}
.lbx .th{color:#98a1ae;font-size:.75rem;letter-spacing:.1em;text-transform:uppercase;font-weight:700;border-bottom:1px solid rgba(255,255,255,.08)}
.lbx .v{overflow:hidden;flex:1}
.lbx .r{border-top:1px solid rgba(255,255,255,.05);font-size:1.15rem}
.lbx .r b{display:flex;align-items:center;gap:.7rem}
.lbx .r .av{width:2.1rem;height:2.1rem;font-size:.9rem}
.lbx .num{text-align:right}
.lbx .r .num.hi{font-weight:800;color:#f4f6f8}
.mv{font-size:.72rem;font-weight:800;margin-left:.35rem}
.mv.up{color:#5df08a}.mv.down{color:#ff6b6b}.mv.same{color:#5b6370}.mv.new{color:var(--ova)}
.gain{display:block;font-size:.72rem;color:#5df08a;font-weight:700}
/* laps rail */
.lr{left:2.5rem;top:2.5rem;bottom:2.5rem;width:24rem;border-radius:1.3rem;display:flex;flex-direction:column;overflow:hidden}
.lr .h{padding:1rem 1.2rem;display:flex;align-items:center;gap:.6rem;border-bottom:1px solid rgba(255,255,255,.08)}
.lr .h b{font-size:1.15rem}
.lr .v{overflow:hidden;flex:1}
.lr .r{display:flex;align-items:center;gap:.6rem;padding:.55rem 1.2rem;border-top:1px solid rgba(255,255,255,.04)}
.lr .r b{flex:1;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.lr .r .av{width:2rem;height:2rem;font-size:.85rem}
.lr .r.imp{background:linear-gradient(90deg,rgba(93,240,138,.14),transparent)}
.lr .r .t{font-weight:800}
/* spotlight */
.spx{left:50%;bottom:2.5rem;transform:translateX(-50%);border-radius:1.6rem;padding:1.4rem 1.8rem;display:flex;gap:1.6rem;align-items:stretch}
.spx .pc{min-width:22rem}
.spx .pc .top{display:flex;align-items:center;gap:1rem;margin-bottom:1rem}
.spx .pc .top .av{width:4.4rem;height:4.4rem;font-size:1.9rem}
.spx .pc .top b{font-size:2rem;letter-spacing:-.03em;display:block}
.spx .pc .rows div{display:flex;justify-content:space-between;gap:2rem;padding:.4rem 0;border-top:1px solid rgba(255,255,255,.06);font-size:1.1rem}
.spx .pc .rows div span:first-child{color:#98a1ae}
.spx .pc .rows div span:last-child{font-weight:800}
.spx .pc.lead .top b{color:var(--ova)}
.spx .vs{display:flex;flex-direction:column;align-items:center;justify-content:center;gap:.4rem;padding:0 .4rem}
.spx .vs b{font-size:2.4rem;font-weight:900;color:var(--ova)}
.spx .vs small{color:#98a1ae;text-align:center}
.spx .kk{position:absolute;top:-1.1rem;left:1.8rem;font-size:.75rem;font-weight:800;letter-spacing:.16em;text-transform:uppercase;color:#07080a;background:var(--ova);padding:.25rem .7rem;border-radius:999px}
/* standings & results */
.sx{inset:0;padding:3.2rem 4.5rem;display:flex;flex-direction:column}
.sx .hd{display:flex;align-items:center;gap:1.1rem;margin-bottom:1.8rem}
.sx .hd img{width:3.4rem;height:3.4rem;border-radius:.9rem}
.sx .hd h1{margin:0;font-size:3rem;letter-spacing:-.045em}
.sx .hd .sub{color:#98a1ae;font-size:1.1rem}
.sx .hd .pill{margin-left:auto;font-weight:800;letter-spacing:.12em;padding:.45rem 1rem;border-radius:999px;background:rgba(255,77,77,.16);color:#ff6b6b}
.sx .hd .pill.final{background:color-mix(in srgb,var(--ova) 18%,transparent);color:var(--ova)}
.sx .heroes{display:grid;grid-template-columns:1.4fr 1fr 1fr;gap:1.2rem;margin-bottom:1.4rem}
.sx .hero{border-radius:1.5rem;padding:1.4rem 1.6rem;display:flex;align-items:center;gap:1.2rem}
.sx .hero .av{width:5.5rem;height:5.5rem;font-size:2.4rem}
.sx .hero.p1 .av{width:7.5rem;height:7.5rem;font-size:3.2rem;--cc:#ffd54a}
.sx .hero.p2 .av{--cc:#dfe5ec}.sx .hero.p3 .av{--cc:#eb9b60}
.sx .hero small{color:#98a1ae;font-weight:800;letter-spacing:.14em;text-transform:uppercase;font-size:.75rem}
.sx .hero b{display:block;font-size:2rem;letter-spacing:-.03em}
.sx .hero.p1 b{font-size:2.8rem}
.sx .hero .d{color:#c9ced6;font-size:1.05rem}
.sx .rest{overflow:hidden;flex:1;display:grid;grid-template-columns:repeat(3,1fr);gap:.6rem;align-content:start}
.sx .rest div{display:flex;align-items:center;gap:.8rem;padding:.65rem 1rem;border-radius:1rem;background:rgba(255,255,255,.04);font-size:1.15rem}
.sx .rest div .n{color:#98a1ae;font-weight:800;width:2rem}
.sx .rest div b{flex:1}
.sx .rest div .d{color:#c9ced6}
/* podium */
.pdx{inset:0;display:flex;flex-direction:column;align-items:center;justify-content:flex-end;padding-bottom:4rem}
.pdx .hd{position:absolute;top:3rem;left:4.5rem;right:4.5rem;display:flex;align-items:center;gap:1rem}
.pdx .hd img{width:3.2rem;height:3.2rem;border-radius:.8rem}
.pdx .hd h1{margin:0;font-size:2.6rem;letter-spacing:-.04em}
.pdx .hd .pill{margin-left:auto;font-weight:800;letter-spacing:.12em;padding:.45rem 1rem;border-radius:999px;background:color-mix(in srgb,var(--ova) 18%,transparent);color:var(--ova)}
.pdx .steps{display:flex;align-items:flex-end;gap:1.2rem}
.pdx .s{width:19rem;text-align:center;animation:walkon .8s both}
.pdx .s .av{width:7rem;height:7rem;font-size:3rem;margin:0 auto .8rem}
.pdx .s b{display:block;font-size:2rem;letter-spacing:-.03em}
.pdx .s .d{color:#98a1ae;margin:.2rem 0 .9rem}
.pdx .s .stp{border-radius:1.2rem 1.2rem .4rem .4rem;display:grid;place-items:center;font-size:4rem;font-weight:900}
.pdx .s.p1 .stp{height:16rem;background:linear-gradient(180deg,rgba(255,213,74,.38),rgba(255,213,74,.05));border:1px solid rgba(255,213,74,.5);color:#ffd54a}
.pdx .s.p2 .stp{height:11.5rem;background:linear-gradient(180deg,rgba(220,226,234,.3),rgba(220,226,234,.05));border:1px solid rgba(220,226,234,.4);color:#dfe5ec}
.pdx .s.p3 .stp{height:8.5rem;background:linear-gradient(180deg,rgba(232,150,90,.32),rgba(232,150,90,.05));border:1px solid rgba(232,150,90,.45);color:#eb9b60}
.pdx .s.p4 .stp{height:6rem;background:rgba(255,255,255,.05);border:1px solid rgba(255,255,255,.12);color:#98a1ae}
.pdx .s.p1 .av{--cc:#ffd54a}.pdx .s.p2 .av{--cc:#dfe5ec}.pdx .s.p3 .av{--cc:#eb9b60}.pdx .s.p4 .av{--cc:#5b6370}
.pdx .crown{font-size:2.6rem;display:block;margin-bottom:-.4rem}
/* notice + panel */
.nt{left:50%;bottom:3rem;transform:translateX(-50%);border-radius:999px;padding:.9rem 2rem .9rem 1rem;display:flex;align-items:center;gap:.9rem;max-width:80rem}
.nt img{width:2.2rem;height:2.2rem;border-radius:.6rem}
.nt b{font-size:1.5rem;letter-spacing:-.01em}
</style>{{end}}

{{/* ── scorebug: the badge is static, the heat strip slides ── */}}
{{define "ov-scorebug"}}{{template "ov-head" .}}
<div class="ovx sb">
  {{if ne (.Q.Get "badge") "0"}}<div class="badge glass">
    <div class="lgw"><img src="/assets/icon-256.webp" alt=""></div>
    <div class="tt"><b>{{if .Title}}{{.Title}}{{else}}{{.Event}}{{end}}</b>
      {{if ne (.Q.Get "clock") "0"}}<div class="bar"><span data-day></span><span data-wall12></span></div>{{end}}</div>
  </div>{{end}}
  {{if ne (.Q.Get "strip") "0"}}<div id="app">{{template "ov-scorebug-body" .}}</div>{{else}}<div id="app"></div>{{end}}
</div>
<script>
/* Heat strip: slides in when a heat is cued (6s), stays up while it runs
   (unless live=0), and after the flag waits 5s then holds for 10s. */
(function(){
  var q=new URLSearchParams(location.search),live=q.get('live')!=='0',upnext=q.get('upnext')!=='0';
  if(q.get('still')==='1')return;
  var NEXT=+(q.get('next')||6)*1000,WAIT=+(q.get('endwait')||5)*1000,HOLD=+(q.get('hold')||10)*1000;
  var last={race:'',state:''},showUntil=0,showFrom=0;
  setInterval(function(){
    var el=document.querySelector('[data-sb]');var now=Date.now();
    if(!el){document.body.classList.remove('sbshow');return}
    var race=el.getAttribute('data-race'),st=el.getAttribute('data-state');
    if(race!==last.race||st!==last.state){
      if(st==='pending'&&upnext){showFrom=now;showUntil=now+NEXT}
      else if(st==='ended'){showFrom=now+WAIT;showUntil=now+WAIT+HOLD}
      last={race:race,state:st};
    }
    var on=(st==='running'&&live)||(now>=showFrom&&now<showUntil);
    document.body.classList.toggle('sbshow',on);
  },200);
})();
</script>
{{template "ov-foot" .}}{{end}}
{{define "ov-scorebug-body"}}{{with .R}}<div class="strip glass" data-sb data-key="sb" data-race="{{.ID}}" data-state="{{.State}}">
  <span data-key="k" class="k{{if eq .State "running"}} st-live{{else}} acc{{end}}">{{if eq .State "running"}}Live{{else if eq .State "ended"}}Just raced{{else}}Next up{{end}}</span>
  <b data-key="l">{{.Label}}</b>
  {{if eq .State "running"}}<span data-key="clk" class="clk" data-clock-start="{{.StartMs}}">0.0</span>{{end}}
</div>{{end}}{{end}}

{{/* ── top bar ── */}}
{{define "ov-topbar"}}{{template "ov-head" .}}<div id="app">{{template "ov-topbar-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-topbar-body"}}<div class="ovx glass tb">
  <div class="l" data-key="l">{{with .R}}<span data-key="p" class="pill{{if eq .State "running"}} st-live{{else}} acc{{end}}">{{.Status}}</span>{{if eq .State "running"}}<span data-key="clk" class="clk" data-clock-start="{{.StartMs}}">0.0</span>{{end}}{{else}}<span data-key="p" class="pill mut">Standing by</span>{{end}}</div>
  <div class="c" data-key="c"><div data-key="ev" class="ev">{{.Event}}</div><b data-key="t">{{with .R}}{{.Label}}{{else}}Between heats{{end}}</b></div>
  <div class="r" data-key="r"><span data-key="n">{{.Brand.Name}}</span><img data-key="i" src="/assets/icon-256.webp" alt=""></div>
</div>{{end}}

{{/* ── corner ── */}}
{{define "ov-corner"}}{{template "ov-head" .}}
<div class="ovx glass co"><img src="/assets/icon-256.webp" alt=""><span class="w" data-wall></span><div id="app">{{template "ov-corner-body" .}}</div></div>
{{template "ov-foot" .}}{{end}}
{{define "ov-corner-body"}}{{with .R}}<span data-key="h" class="h{{if eq .State "running"}} st-live{{else if eq .State "ended"}} done{{else}} next{{end}}">{{if .RoundName}}{{.RoundName}} · {{end}}Race {{.Number}}</span>{{else}}<span data-key="h" class="h done">Standing by</span>{{end}}{{end}}

{{/* ── race grid: transparent tiles over the camera feeds ── */}}
{{define "ov-race"}}{{template "ov-head" .}}<div id="app">{{template "ov-race-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-race-body"}}{{with .R}}{{$cols := gridCols $.Q (len .Seats)}}<div class="ovx rg" style="grid-template-columns:repeat({{$cols}},1fr)">
  {{range .Seats}}<div class="tile" data-key="{{.Name}}" style="--cc:{{.Color}}">
    <div data-key="lps" class="lps">{{range .Recent}}<span data-key="{{.Key}}" class="lp{{if .Best}} best{{end}}"><em>{{.Key}}</em>{{f3 .T}}{{if .Delta}}<em>{{signed .Delta}}</em>{{end}}</span>{{end}}</div>
    <div data-key="bt" class="bt"><span data-key="av" class="av">{{.Initial}}</span><span data-key="ch" class="chp">{{.Channel}}</span><b data-key="n">{{.Name}}</b><span data-key="ps" class="ps">P{{.Pos}}</span></div>
  </div>{{end}}
</div>{{end}}{{end}}

{{/* ── pilot cam tag (one per seat) ── */}}
{{define "ov-node"}}{{template "ov-head" .}}<div id="app">{{template "ov-node-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-node-body"}}{{with .P}}<div class="ovx glass nd" style="--cc:{{.Color}}">
  <div data-key="ps" class="ps">{{.Pos}}<small>{{$.Ord}}</small></div>
  <div data-key="ch" class="ch">{{.Channel}}</div>
  <div data-key="bd" class="bd"><b data-key="n">{{.Name}}</b><div data-key="m" class="m">
    {{if $.R.IsTT}}<span data-key="a">Best {{$.R.Window}} <b>{{f2 .Consec}}</b></span><span data-key="b">Best lap <b>{{f3 .Best}}</b></span><span data-key="c">Laps <b>{{.Laps}}</b></span>
    {{else}}<span data-key="a">Lap <b>{{.Laps}}{{if .Target}}/{{.Target}}{{end}}</b></span><span data-key="b">Last <b>{{f3 .Last}}</b></span><span data-key="c">Best <b>{{f3 .Best}}</b></span><span data-key="d">Total <b>{{f3 .Total}}</b></span>{{end}}
  </div></div>
</div>{{end}}{{end}}

{{/* ── strip: racing now (bottom right) or up next (bottom left) ── */}}
{{define "ov-strip"}}{{template "ov-head" .}}<div id="app">{{template "ov-strip-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-strip-body"}}{{with .R}}<div class="ovx glass sp2 {{if eq $.Ref "next"}}nxt{{else}}cur{{end}}">
  <div data-key="hd" class="hd"><span data-key="k" class="k{{if eq .State "running"}} st-live{{else}} acc{{end}}">{{if eq $.Ref "next"}}Up next{{else if eq .State "running"}}Racing now{{else}}{{.Status}}{{end}}</span><b data-key="l">{{.Label}}</b></div>
  {{range .Seats}}<div class="p" data-key="{{.Name}}" style="--cc:{{.Color}}"><span data-key="av" class="av">{{.Initial}}</span><b data-key="n">{{.Name}}</b><span data-key="ch" class="chp">{{.Channel}}</span></div>{{end}}
</div>{{else}}<div class="ovx glass sp2 {{if eq $.Ref "next"}}nxt{{else}}cur{{end}}"><div class="hd" data-key="hd"><span data-key="k" class="k mut">No heat lined up yet</span></div></div>{{end}}{{end}}

{{/* ── lap chart ── */}}
{{define "ov-lapchart"}}{{template "ov-head" .}}<div id="app">{{template "ov-lapchart-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-lapchart-body"}}{{with .R}}<div class="ovx glass lc">
  <div class="h" data-key="h"><img data-key="lg" class="lg" src="/assets/icon-256.webp" alt=""><b data-key="rl">{{.Label}}</b>{{if eq .State "running"}}<span data-key="clk" class="clk" data-clock-start="{{.StartMs}}">0.0</span>{{end}}</div>
  <div data-key="rows">{{range .Pilots}}<div class="r" data-key="{{.Name}}">
    <span data-key="pos" class="pos{{if le .Pos 3}} p{{.Pos}}{{end}}">{{.Pos}}</span><i data-key="cb" class="cb" style="background:{{.Color}}"></i><b data-key="n">{{.Name}}</b>
    <span data-key="ls" class="ls">{{range .Chips}}<span data-key="{{.Key}}" class="l{{if .Best}} best{{end}}">{{f3 .T}}</span>{{end}}</span>
  </div>{{end}}</div>
</div>{{end}}{{end}}

{{/* ── fastest lap ── */}}
{{define "ov-fastest"}}{{template "ov-head" .}}<div id="app">{{template "ov-fastest-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-fastest-body"}}{{with fastestLap .R}}<div class="ovx glass fl">
  <div data-key="k" class="k">⚡ Fastest lap</div><div data-key="t" class="t">{{f3 .Best}}</div>
  <div data-key="w" class="w"><i data-key="cb" class="cb" style="background:{{.Color}}"></i><b data-key="n">{{.Name}}</b><span data-key="ch" class="mut">{{.Channel}}</span></div>
</div>{{end}}{{end}}

{{/* ── race panel (also /overlay) ── */}}
{{define "ov-panel"}}{{template "ov-head" .}}<div id="app">{{template "ov-panel-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-panel-body"}}{{template "overlay-body" .}}{{end}}

{{/* ── lower third ── */}}
{{define "ov-lowerthird"}}{{template "ov-head" .}}<div id="app">{{template "ov-lowerthird-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-lowerthird-body"}}{{with .R}}<div class="ovx l3">{{range .Pilots}}<div class="c glass" data-key="{{.Name}}" style="--cc:{{.Color}}">
  <span data-key="pos" class="pos{{if le .Pos 3}} p{{.Pos}}{{end}}">{{.Pos}}</span><b data-key="n">{{.Name}}</b><span data-key="ch" class="mut">{{.Channel}}</span>
  <div data-key="lt" class="lt"><span data-key="a">Lap {{.Laps}} · last {{f3 .Last}}</span><span data-key="b">{{f3 .Total}}</span></div>
</div>{{end}}</div>{{end}}{{end}}

{{/* ── lineup (full screen) ── */}}
{{define "ov-lineup"}}{{template "ov-head" .}}<div id="app">{{template "ov-lineup-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-lineup-body"}}{{with .R}}{{$r := .}}<div class="ovx lu">
  <div class="hd" data-key="hd"><img data-key="i" src="/assets/icon-256.webp" alt=""><div data-key="t"><div class="ev">{{$.Event}}</div><h1>{{.Label}}</h1></div>
    <span data-key="p" class="pill{{if eq .State "running"}} st-live{{else}} acc{{end}}">{{if eq .State "ended"}}Results{{else}}{{.Status}}{{end}}</span></div>
  <div class="g" data-key="g" style="--n:{{len .Pilots}}">{{range $i, $p := .Pilots}}<div class="cd glass walk" data-key="{{.Name}}" style="--cc:{{.Color}};animation-delay:{{delayS $i 0.3}}">
    <span data-key="av" class="av">{{.Initial}}</span><b data-key="n">{{.Name}}</b><span data-key="ch" class="chp">{{.Channel}}</span>
    {{if eq $r.State "pending"}}<div data-key="s" class="stats">
      <div data-key="a"><small>Meeting rank</small><span>{{if .Rank}}P{{.Rank}}{{else}}—{{end}}</span></div>
      <div data-key="b"><small>Best lap</small><span>{{f3 .MBest}}</span></div>
      {{if $r.IsTT}}<div data-key="c"><small>Best {{$r.Window}}</small><span>{{f2 .MConsec}}</span></div>{{else}}<div data-key="c"><small>Fastest race</small><span>{{f3 .MFastest}}</span></div>{{end}}
      <div data-key="d"><small>Laps flown</small><span>{{.MLaps}}</span></div></div>
    {{else}}<div data-key="big" class="big">P{{.Pos}}</div>
      <div data-key="s" class="stats"><div data-key="a"><small>Best</small><span>{{f3 .Best}}</span></div><div data-key="b"><small>Total</small><span>{{f3 .Total}}</span></div></div>
      <div data-key="chips" class="chips">{{range .Chips}}<span data-key="{{.Key}}"{{if .Best}} class="best"{{end}}>{{f3 .T}}</span>{{end}}</div>{{end}}
  </div>{{end}}</div>
</div>{{end}}{{end}}

{{/* ── Up Now and its banner ── */}}
{{define "ov-upnow"}}{{template "ov-head" .}}<div id="app">{{template "ov-upnow-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-upnow-body"}}{{with .R}}<div class="ovx un"><div data-key="k" class="k">{{.Status}} · {{.Label}}</div>{{range $i, $p := .Seats}}<div class="cd glass" data-key="{{.Name}}" style="--cc:{{.Color}};animation-delay:{{delayS $i 0.35}}">
  <span data-key="av" class="av">{{.Initial}}</span><b data-key="n">{{.Name}}</b><span data-key="ch" class="chp">{{.Channel}}</span>
  <div data-key="m" class="m"><span data-key="a">Rank<b>{{if .Rank}}P{{.Rank}}{{else}}—{{end}}</b></span><span data-key="b">Best lap<b>{{f3 .MBest}}</b></span><span data-key="c">Best consec<b>{{f2 .MConsec}}</b></span></div>
</div>{{end}}</div>{{end}}{{end}}
{{define "ov-banner"}}{{template "ov-head" .}}<div id="app">{{template "ov-banner-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-banner-body"}}{{with .R}}<div class="ovx bn"><div data-key="k" class="k">{{.Status}} · {{.Label}}</div>{{range $i, $p := .Seats}}<div class="cd glass" data-key="{{.Name}}" style="--cc:{{.Color}};animation-delay:{{delayS $i 0.3}}">
  <span data-key="av" class="av">{{.Initial}}</span><div data-key="t"><b>{{.Name}}</b><small>{{.Channel}} · {{if .Rank}}P{{.Rank}}{{else}}new{{end}} · best {{f3 .MBest}}</small></div>
</div>{{end}}</div>{{end}}{{end}}

{{/* ── leaderboard (full screen, scrolls long fields) ── */}}
{{define "ov-leaderboard"}}{{template "ov-head" .}}<div id="app">{{template "ov-leaderboard-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-leaderboard-body"}}{{if .Rows}}<div class="ovx glass lbx">
  <div class="h" data-key="h"><img data-key="i" src="/assets/icon-256.webp" alt=""><b data-key="t">Leaderboard</b><span data-key="e" class="mut">{{.Event}}</span></div>
  <div class="th" data-key="th"><span data-key="a">#</span><span data-key="b">Chan</span><span data-key="c">Pilot</span><span data-key="d" class="num">Laps</span><span data-key="e" class="num">Hole shot</span><span data-key="f" class="num">Top lap</span><span data-key="g" class="num">{{if .M.IsTT}}Top {{.M.Window}} consec{{else}}Fastest race{{end}}</span></div>
  <div class="v" data-key="v" data-autoscroll>{{range .Rows}}<div class="r" data-key="{{.Name}}">
    <span data-key="a"><span class="pos{{if le .Pos 3}} p{{.Pos}}{{end}}">{{.Pos}}</span><span class="mv {{.MoveDir}}">{{.Move}}</span></span>
    <span data-key="b"><span class="chp" style="--cc:{{.ChannelColor}}">{{.ChannelLabel}}</span></span>
    <b data-key="c"><span class="av" style="--cc:{{.ChannelColor}}">{{.Initial}}</span>{{.Name}}</b>
    <span data-key="d" class="num">{{.Laps}}</span><span data-key="e" class="num">{{f3 .Holeshot}}</span>
    <span data-key="f" class="num{{if not $.M.IsTT}} hi{{end}}">{{f3 .BestLap}}{{if and .Improved (not $.M.IsTT)}}<span class="gain">-{{printf "%.2f" .Gain}}</span>{{end}}</span>
    <span data-key="g" class="num{{if $.M.IsTT}} hi{{end}}">{{if $.M.IsTT}}{{f2 .BestConsec}}{{if .Improved}}<span class="gain">-{{printf "%.2f" .Gain}}</span>{{end}}{{else}}{{f3 .FastestRace}}{{end}}</span>
  </div>{{end}}</div>
</div>{{end}}{{end}}

{{/* ── laps rail ── */}}
{{define "ov-laps"}}{{template "ov-head" .}}<div id="app">{{template "ov-laps-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-laps-body"}}{{if .Rows}}<div class="ovx glass lr">
  <div class="h" data-key="h"><img data-key="i" class="lg" src="/assets/icon-256.webp" alt=""><b data-key="t">{{if eq .Mode "consec"}}Best {{.M.Window}} consecutive{{else}}Best lap{{end}}</b></div>
  <div class="v" data-key="v" data-autoscroll>{{range .Rows}}<div class="r{{if .Improved}} imp{{end}}" data-key="{{.Name}}">
    <span data-key="a" class="pos{{if le .Pos 3}} p{{.Pos}}{{end}}">{{.Pos}}</span><span data-key="m" class="mv {{.MoveDir}}">{{.Move}}</span>
    <span data-key="av" class="av" style="--cc:{{.ChannelColor}}">{{.Initial}}</span><b data-key="n">{{.Name}}</b>
    <span data-key="t" class="t">{{if eq $.Mode "consec"}}{{f2 .BestConsec}}{{else}}{{f3 .BestLap}}{{end}}{{if .Improved}} 🔥{{end}}</span>
  </div>{{end}}</div>
</div>{{end}}{{end}}

{{/* ── spotlight: one pilot, or two head to head ── */}}
{{define "ov-spotlight"}}{{template "ov-head" .}}<div id="app">{{template "ov-spotlight-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-spotlight-body"}}{{if .Spot}}<div class="ovx glass spx" data-key="sp"><span data-key="kk" class="kk">{{if eq (len .Spot) 2}}Head to head{{else}}Pilot spotlight{{end}}</span>
  {{range $i, $p := .Spot}}{{if eq $i 1}}<div data-key="vs" class="vs"><b>VS</b><small>{{if $.M.IsTT}}{{f2 $.GapConsec}} apart{{else}}{{f3 $.GapLap}} apart{{end}}<br>on best {{if $.M.IsTT}}{{$.M.Window}}{{else}}lap{{end}}</small></div>{{end}}
  <div class="pc{{if and (eq (len $.Spot) 2) (eq $i 0) (lt .Pos (index $.Spot 1).Pos)}} lead{{else if and (eq (len $.Spot) 2) (eq $i 1) (lt .Pos (index $.Spot 0).Pos)}} lead{{end}}" data-key="{{.Name}}">
    <div data-key="top" class="top"><span class="av" style="--cc:{{.ChannelColor}}">{{.Initial}}</span><div><b>{{.Name}}</b><span class="mut">P{{.Pos}} of {{.Of}} · {{.ChannelLabel}}</span></div></div>
    <div data-key="rows" class="rows">
      <div><span>Best lap</span><span>{{f3 .BestLap}}</span></div>
      <div><span>Best {{$.M.Window}} consecutive</span><span>{{f2 .BestConsec}}</span></div>
      <div><span>Fastest race</span><span>{{f3 .FastestRace}}</span></div>
      <div><span>Holeshot</span><span>{{f3 .Holeshot}}</span></div>
      <div><span>Laps flown</span><span>{{.Laps}}</span></div>
    </div>
  </div>{{end}}
</div>{{end}}{{end}}

{{/* ── standings ── */}}
{{define "ov-standings"}}{{template "ov-head" .}}<div id="app">{{template "ov-standings-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-standings-body"}}{{if .Rows}}<div class="ovx sx">
  {{if ne (.Q.Get "head") "0"}}<div class="hd" data-key="hd"><img data-key="i" src="/assets/icon-256.webp" alt=""><div data-key="t"><h1>{{if .Title}}{{.Title}}{{else}}Standings{{end}}</h1><div class="sub">{{.Event}}</div></div>
    <span data-key="p" class="pill{{if .Final}} final{{end}}">{{if .Final}}FINAL{{else}}LIVE{{end}}</span></div>{{end}}
  <div class="heroes" data-key="heroes">{{range $i, $r := firstN 3 .Rows}}<div class="hero glass p{{.Pos}}" data-key="{{.Name}}">
    <span class="av" style="--cc:{{.ChannelColor}}">{{.Initial}}</span>
    <div><small>{{if eq .Pos 1}}{{if $.Final}}Champion{{else}}Leading{{end}}{{else}}{{.Pos}}{{if eq .Pos 2}}nd{{else}}rd{{end}}{{end}}</small><b>{{.Name}}</b>{{if ne ($.Q.Get "detail") "0"}}<span class="d">{{if $.M.IsTT}}{{f2 .BestConsec}} best {{$.M.Window}}{{else}}{{f3 .BestLap}} best lap{{end}}</span>{{end}}</div>
  </div>{{end}}</div>
  <div class="rest" data-key="rest" data-autoscroll style="grid-template-columns:repeat({{.Cols}},1fr)">{{range afterN 3 .Rows}}<div data-key="{{.Name}}"><span class="n">{{.Pos}}</span><b>{{.Name}}</b>{{if ne ($.Q.Get "detail") "0"}}<span class="d">{{if $.M.IsTT}}{{f2 .BestConsec}}{{else}}{{f3 .BestLap}}{{end}}</span>{{end}}</div>{{end}}</div>
</div>{{end}}{{end}}

{{/* ── podium ── */}}
{{define "ov-podium"}}{{template "ov-head" .}}<div id="app">{{template "ov-podium-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-podium-body"}}{{if .Rows}}<div class="ovx pdx">
  <div class="hd" data-key="hd"><img data-key="i" src="/assets/icon-256.webp" alt=""><h1 data-key="t">{{if .Title}}{{.Title}}{{else}}{{.Event}}{{end}}</h1><span data-key="p" class="pill">{{if .Final}}FINAL{{else}}LIVE{{end}}</span></div>
  <div class="steps" data-key="steps">{{range podiumOrder .Rows}}<div class="s p{{.Pos}}" data-key="{{.Name}}">
    {{if eq .Pos 1}}<span class="crown">👑</span>{{end}}<span class="av">{{.Initial}}</span><b>{{.Name}}</b>
    <div class="d">{{if $.M.IsTT}}{{f2 .BestConsec}}{{else}}{{f3 .BestLap}}{{end}}</div><div class="stp">{{.Pos}}</div>
  </div>{{end}}</div>
</div>{{end}}{{end}}

{{/* ── results card, from the URL alone ── */}}
{{define "ov-results"}}{{template "ov-head" .}}<div id="app">{{template "ov-results-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-results-body"}}{{if .Results}}<div class="ovx sx">
  <div class="hd" data-key="hd"><img data-key="i" src="/assets/icon-256.webp" alt=""><div data-key="t"><h1>{{if .Title}}{{.Title}}{{else}}Results{{end}}</h1>{{if .Sub}}<div class="sub">{{.Sub}}</div>{{end}}</div><span data-key="p" class="pill final">FINAL</span></div>
  <div class="heroes" data-key="heroes">{{range $i, $r := .Results}}{{if lt $i 3}}<div class="hero glass p{{.Pos}}" data-key="h{{$i}}"><span class="av">{{.Initial}}</span>
    <div><small>{{if eq .Pos 1}}Winner{{else if eq .Pos 2}}2nd{{else}}3rd{{end}}</small><b>{{.Name}}</b><span class="d">{{.Detail}}</span></div></div>{{end}}{{end}}</div>
  <div class="rest" data-key="rest">{{range $i, $r := .Results}}{{if ge $i 3}}<div data-key="r{{$i}}"><span class="n">{{.Pos}}</span><b>{{.Name}}</b><span class="d">{{.Detail}}</span></div>{{end}}{{end}}</div>
</div>{{end}}{{end}}

{{/* ── notice ── */}}
{{define "ov-notice"}}{{template "ov-head" .}}<div id="app">{{template "ov-notice-body" .}}</div>{{template "ov-foot" .}}{{end}}
{{define "ov-notice-body"}}{{if .Text}}<div class="ovx glass nt"><img data-key="lg" src="/assets/icon-256.webp" alt=""><b data-key="t">{{.Text}}</b></div>{{end}}{{end}}
`

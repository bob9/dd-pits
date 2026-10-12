package main

// Logging in as the race director from another device.
//
// Everyone on the network (or the internet, behind a proxy) can watch; only
// the race director may change things, which from any device but the timing
// machine means presenting the setup PIN. This page is where that happens:
//
//   - with RotorHazard, "Log in with RotorHazard" goes through the plugin's
//     /dd-pits/setup, which asks for RotorHazard's admin login and hands the
//     PIN back, landing on the page you came from;
//   - on any timer, the PIN can be typed in.
//
// Either way the PIN is kept in a cookie (rememberSetupPIN), so the controls
// stay unlocked in that browser.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// rotorhazardPort is the RotorHazard server's port, for building the login
// link at a venue, where dd-pits and RotorHazard are reached on the same
// host but different ports. Empty when the timer is FPVTrackside.
var rotorhazardPort string

// safeNext keeps a post-login destination on this site and in dd-pits. A
// form field rewritten for a sub-path (/pits/stages/s1) comes back with the
// prefix on; redirects get it added again, so it is taken off here.
func safeNext(r *http.Request, v string) string {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.ContainsAny(v, "\\\r\n") {
		return "/"
	}
	if p := forwardedPrefix(r); p != "" && (v == p || strings.HasPrefix(v, p+"/")) {
		v = strings.TrimPrefix(v, p)
		if v == "" {
			v = "/"
		}
	}
	return v
}

func (s *liveServer) registerLogin(mux *http.ServeMux) {
	mux.HandleFunc("/login", s.loginPage)
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: setupPINCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		http.Redirect(w, r, safeNext(r, r.FormValue("next")), http.StatusSeeOther)
	})
}

func (s *liveServer) loginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r, r.FormValue("next"))
	if demoMode {
		setupDenied(w)
		return
	}
	msg := ""
	if r.Method == http.MethodPost {
		if setupPIN != "" && strings.TrimSpace(r.FormValue("pin")) == setupPIN {
			http.SetCookie(w, &http.Cookie{Name: setupPINCookie, Value: setupPIN, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		msg = `<p class="warn">That PIN isn't right.</p>`
	}
	if localSetupAllowed(r) && r.Method == http.MethodGet {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}

	b := &strings.Builder{}
	b.WriteString(`<div class="card"><h2>Race director</h2>
<p class="mut">Anyone can watch. Starting and stopping races, entering results and marshalling laps need the race director's login.</p>`)
	if timingSystem == "RotorHazard" {
		// Built in the browser: behind a proxy RotorHazard is this same site;
		// at a venue it is this host on RotorHazard's own port. Written as a
		// data attribute so the sub-path rewriting leaves it alone.
		fmt.Fprintf(b, `<p><a id="rh-login" class="rhbtn" data-port="%s" data-next="%s"><button type="button">Log in with RotorHazard</button></a></p>
<p class="mut">Uses RotorHazard's admin login (the one for its Settings page).</p>
<script>(function(){
 // data-next carries no leading slash so the sub-path rewriting can't touch it.
 var a=document.getElementById('rh-login'), next='/'+a.dataset.next;
 // The page's own path tells us where dd-pits lives on this site (e.g. /pits).
 var base=location.pathname.replace(/\/login$/,'');
 var origin=location.origin;
 if(base===''&&a.dataset.port&&location.port!==a.dataset.port){ origin=location.protocol+'//'+location.hostname+':'+a.dataset.port; }
 a.href=origin+'/dd'+'-pits/setup?next='+encodeURIComponent(next);
})();</script>`, esc(rotorhazardPort), esc(strings.TrimPrefix(next, "/")))
	}
	if setupPIN != "" {
		fmt.Fprintf(b, `<form method="post" class="row" style="align-items:flex-end;margin-top:.6rem">
<input type="hidden" name="next" value="%s">
<div><label>Or enter the setup PIN</label><input type="password" name="pin" inputmode="numeric" autocomplete="current-password" required></div>
<div style="flex:0"><button class="sec">Log in</button></div></form>%s`, esc(next), msg)
		if timingSystem == "RotorHazard" {
			b.WriteString(`<p class="mut">The PIN is in RotorHazard: Settings → dd-pits → Set up dd-pits.</p>`)
		}
	} else if timingSystem != "RotorHazard" {
		b.WriteString(`<p>No setup PIN is set, so changes can only be made on the timing laptop itself.
Start dd-pits with <code>-setup-pin 1234</code> to allow them from a phone or another computer.</p>`)
	}
	b.WriteString(`</div>`)
	writeSetup(w, "Log in", b.String())
}

// loginBar invites a viewer who can't change things to log in, from a page
// that has race director controls. Empty for someone who already can.
func loginBar(r *http.Request) string {
	if localSetupAllowed(r) || demoMode {
		return ""
	}
	return `<div class="card loginbar"><span>Race director? Log in to start and stop races, enter results and marshal laps.</span>
<a href="/login?next=` + url.QueryEscape(r.URL.RequestURI()) + `"><button>Log in</button></a></div>`
}

// logoutLink is shown to a logged-in race director on another device.
func logoutLink(r *http.Request) string {
	if !localSetupAllowed(r) || setupPIN == "" {
		return ""
	}
	if c, err := r.Cookie(setupPINCookie); err != nil || c.Value == "" {
		return ""
	}
	return ` · <a href="/logout?next=` + url.QueryEscape(r.URL.RequestURI()) + `">Log out</a>`
}

// rhBase is where RotorHazard's own pages are, seen from this request, for
// links back to them; "" when the timer isn't RotorHazard. Behind a proxy that
// serves dd-pits under a sub-path, RotorHazard is the rest of the same site;
// at a venue it is the same host on RotorHazard's port. Absolute, so the
// sub-path rewriting leaves it alone.
func rhBase(r *http.Request) string {
	if timingSystem != "RotorHazard" {
		return ""
	}
	scheme := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if forwardedPrefix(r) != "" {
		return scheme + "://" + host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if rotorhazardPort != "" {
		return scheme + "://" + host + ":" + rotorhazardPort
	}
	return scheme + "://" + host
}

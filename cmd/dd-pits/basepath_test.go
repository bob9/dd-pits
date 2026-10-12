package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrefixLinks(t *testing.T) {
	in := `<a href="/">Home</a><a href="/live">Live</a><a href="//cdn.example/x">cdn</a>
<a href="https://buymeacoffee.com/bobnine">x</a><img src="/assets/a.webp">
<style>.x{background:url(/assets/b.webp)}</style>
<script>new EventSource('/events');fetch('/?fragment=1');var u=location.origin+'/overlay/scorebug';
var parts=path.split('/');location.href='/stages/s1'</script>`
	got := string(prefixLinks([]byte(in), "/pits"))
	for _, want := range []string{`href="/pits/"`, `href="/pits/live"`, `href="//cdn.example/x"`,
		`href="https://buymeacoffee.com/bobnine"`, `src="/pits/assets/a.webp"`, `url(/pits/assets/b.webp)`,
		`EventSource('/pits/events')`, `fetch('/pits/?fragment=1')`, `'/pits/overlay/scorebug'`,
		`split('/')`, `location.href='/pits/stages/s1'`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/pits/pits") {
		t.Errorf("doubled prefix:\n%s", got)
	}
}

func TestWithBasePath(t *testing.T) {
	h := withBasePath(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/go":
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
		case "/live":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<a href="/races">races</a>`))
		default:
			http.NotFound(w, r)
		}
	}))
	get := func(path, prefix string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if prefix != "" {
			r.Header.Set("X-Forwarded-Prefix", prefix)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get("/pits/live", "/pits"); !strings.Contains(w.Body.String(), `href="/pits/races"`) {
		t.Fatalf("prefixed page = %q", w.Body.String())
	}
	if w := get("/pits/go", "/pits"); w.Header().Get("Location") != "/pits/setup" {
		t.Fatalf("redirect = %q", w.Header().Get("Location"))
	}
	// Straight to the port: untouched.
	if w := get("/live", ""); !strings.Contains(w.Body.String(), `href="/races"`) {
		t.Fatalf("direct page = %q", w.Body.String())
	}
	// A hostile header is ignored.
	if w := get("/live", `"><script>`); !strings.Contains(w.Body.String(), `href="/races"`) {
		t.Fatalf("bad prefix used: %q", w.Body.String())
	}
}

func TestProxiedRequestsNeedThePIN(t *testing.T) {
	old := setupPIN
	setupPIN = "4321"
	defer func() { setupPIN = old }()
	r := httptest.NewRequest("GET", "/setup", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	if !localSetupAllowed(r) {
		t.Fatal("this machine should be allowed")
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if localSetupAllowed(r) {
		t.Fatal("a visitor through the proxy must not count as this machine")
	}
	r2 := httptest.NewRequest("GET", "/setup?pin=4321", nil)
	r2.RemoteAddr = "127.0.0.1:5000"
	r2.Header.Set("X-Forwarded-For", "203.0.113.9")
	if !localSetupAllowed(r2) {
		t.Fatal("the PIN should still work through the proxy")
	}
}

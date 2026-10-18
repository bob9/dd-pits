package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeHome is a stand-in dd-pits website recording what it was sent.
type fakeHome struct {
	mu     sync.Mutex
	pings  []map[string]any
	regs   []map[string]any
	status int
	reply  string
}

func newFakeHome(t *testing.T) (*fakeHome, *httptest.Server) {
	t.Helper()
	h := &fakeHome{status: 200, reply: `{"ok":true}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &m)
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/ping":
			h.pings = append(h.pings, m)
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/register":
			h.regs = append(h.regs, m)
			w.WriteHeader(h.status)
			io.WriteString(w, h.reply)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DD_PITS_HOME_URL", srv.URL)
	t.Setenv("DD_PITS_NO_USAGE_PING", "")
	return h, srv
}

func testConfig(t *testing.T) *localConfig {
	t.Helper()
	cfg, err := loadLocalConfig(filepath.Join(t.TempDir(), "dd-pits-local.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestUsagePingPayload(t *testing.T) {
	home, _ := newFakeHome(t)
	cfg := testConfig(t)
	if err := sendUsagePing(context.Background(), cfg, "binary", false); err != nil {
		t.Fatal(err)
	}
	if len(home.pings) != 1 {
		t.Fatalf("got %d pings", len(home.pings))
	}
	p := home.pings[0]
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(p["id"].(string)) {
		t.Fatalf("id %q is not 32 hex", p["id"])
	}
	if len(p) != 6 || p["app"] != "binary" || p["timer"] != timingSystem || p["os"] == "" || p["arch"] == "" || p["version"] == "" {
		t.Fatalf("payload %v: want exactly id, version, timer, os, arch, app", p)
	}
	// The ID is kept: a second ping (and a reload) uses the same one.
	sendUsagePing(context.Background(), cfg, "binary", false)
	again, _ := loadLocalConfig(cfg.Path())
	if home.pings[1]["id"] != p["id"] || again.InstallID != p["id"] {
		t.Fatal("install id not kept")
	}
}

func TestUsagePingSwitchedOff(t *testing.T) {
	home, _ := newFakeHome(t)
	cfg := testConfig(t)
	if err := sendUsagePing(context.Background(), cfg, "binary", true); err != errUsagePingOff {
		t.Fatalf("flag: %v", err)
	}
	t.Setenv("DD_PITS_NO_USAGE_PING", "1")
	if err := sendUsagePing(context.Background(), cfg, "binary", false); err != errUsagePingOff {
		t.Fatalf("env: %v", err)
	}
	t.Setenv("DD_PITS_NO_USAGE_PING", "")
	cfg.SetUsagePing(false)
	cfg.Save()
	reloaded, _ := loadLocalConfig(cfg.Path())
	if err := sendUsagePing(context.Background(), reloaded, "binary", false); err != errUsagePingOff {
		t.Fatalf("setting: %v", err)
	}
	if len(home.pings) != 0 {
		t.Fatalf("sent %d pings while switched off", len(home.pings))
	}
	if reloaded.InstallID != "" {
		t.Fatal("an install id was made while switched off")
	}
}

func TestAppKind(t *testing.T) {
	cases := map[string]string{
		"/Applications/dd-pits.app/Contents/MacOS/dd-pits": "mac-app",
		`C:\dd-pits\dd-pits-windows-amd64.exe`:             "binary",
		"/home/pi/dd-pits":                                 "binary",
	}
	for exe, want := range cases {
		if got := appKind(exe, false); got != want {
			t.Errorf("%s = %s, want %s", exe, got, want)
		}
	}
	if appKind("/home/pi/dd-pits", true) != "plugin" {
		t.Error("plugin not detected")
	}
}

func postStatusRegister(t *testing.T, s *liveServer, form url.Values) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/status/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.statusRegister(rec, req)
	return rec.Body.String()
}

func TestStatusRegister(t *testing.T) {
	home, _ := newFakeHome(t)
	s := &liveServer{cfg: testConfig(t)}
	form := url.Values{"name": {"Sam"}, "email": {"sam@example.com"}, "club": {"Night Owls FPV"}, "city": {"Adelaide"}, "timer": {"RotorHazard"}}

	// No consent: refused locally, nothing sent.
	if body := postStatusRegister(t, s, form); !strings.Contains(body, "Tick the box") || len(home.regs) != 0 {
		t.Fatal("registered without consent")
	}

	form.Set("consent", "1")
	body := postStatusRegister(t, s, form)
	if !strings.Contains(body, "Registered as <b>Night Owls FPV</b>") {
		t.Fatalf("no thanks shown: %s", body)
	}
	got := home.regs[0]
	want := map[string]any{"name": "Sam", "email": "sam@example.com", "club": "Night Owls FPV", "city": "Adelaide",
		"timer": "RotorHazard", "consent": true, "source": "app"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if id, _ := got["install_id"].(string); len(id) != 32 {
		t.Errorf("install_id %q", got["install_id"])
	}
	if reg, _ := loadLocalConfig(s.cfg.Path()); reg.Registered == nil || reg.Registered.Club != "Night Owls FPV" {
		t.Fatal("registration not remembered")
	}

	// The website's own refusal is shown in its words.
	home.status, home.reply = 400, `{"error":"That email address doesn't look right."}`
	if body := postStatusRegister(t, s, form); !strings.Contains(body, "doesn&#39;t look right") {
		t.Fatalf("website error not shown: %s", body)
	}
}

func TestStatusRegisterOffline(t *testing.T) {
	t.Setenv("DD_PITS_HOME_URL", "http://127.0.0.1:1") // nothing listens there
	s := &liveServer{cfg: testConfig(t)}
	form := url.Values{"name": {"Sam"}, "email": {"sam@example.com"}, "club": {"Owls"}, "consent": {"1"}}
	if body := postStatusRegister(t, s, form); !strings.Contains(body, "Couldn&#39;t reach the dd-pits website") {
		t.Fatalf("offline message missing: %s", body)
	}
	if s.cfg.Registration() != nil {
		t.Fatal("remembered a registration that never reached the website")
	}
}

func TestStatusRegisterRefusesOtherSites(t *testing.T) {
	newFakeHome(t)
	s := &liveServer{cfg: testConfig(t)}
	req := httptest.NewRequest(http.MethodPost, "/status/register", strings.NewReader("name=x&email=x@y&club=c&consent=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.statusRegister(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site post = %d, want 403", rec.Code)
	}
}

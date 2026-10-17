package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestHomeRenders(t *testing.T) {
	h := newHandler()
	// The site's content is spread over the home page and the topic pages;
	// check it across all of them.
	body := ""
	for _, p := range []string{"/", "/race-directors", "/pilots", "/hardware", "/best-setup"} {
		res, b := get(t, h, p)
		if res.StatusCode != 200 {
			t.Fatalf("GET %s = %d", p, res.StatusCode)
		}
		body += b
	}
	for _, want := range []string{
		"Race day,",
		"https://github.com/bob9/dd-pits/releases",
		"https://github.com/bob9/elrs-netpack",
		"https://github.com/i-am-grub/elrs-netpack",
		"https://buymeacoffee.com/bobnine",
		"Sounds.xml",
		"/booth.wav",
		"Model host mode",
		"DVR recordings named after the heat",
		"Everything at once",
		"Three Digital Llamas",
		"How to use dd-pits",
		"https://demo.dd-pits.threedigitalllamas.com.au",
		"/setup/backpack",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing %q", want)
		}
	}
	if strings.Contains(body, "style=\"") {
		t.Error("inline style attribute would be blocked by the CSP")
	}
}

// Every /static reference on the page, and in the stylesheet, must resolve.
func TestStaticAssetsExist(t *testing.T) {
	h := newHandler()
	_, body := get(t, h, "/")
	re := regexp.MustCompile(`/static/[^"' )?]+(\?v=[0-9a-f]+)?`)
	refs := re.FindAllString(body, -1)
	if len(refs) < 20 {
		t.Fatalf("only %d static references found — regexp broken?", len(refs))
	}
	for _, ref := range refs {
		res, _ := get(t, h, ref)
		if res.StatusCode != 200 {
			t.Errorf("%s = %d", ref, res.StatusCode)
		}
		if strings.Contains(ref, "?v=") && !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: versioned asset not cached immutably", ref)
		}
	}
}

func TestMetaRoutes(t *testing.T) {
	h := newHandler()
	for _, p := range []string{"/healthz", "/robots.txt", "/sitemap.xml"} {
		if res, _ := get(t, h, p); res.StatusCode != 200 {
			t.Errorf("%s = %d", p, res.StatusCode)
		}
	}
	if res, _ := get(t, h, "/nope"); res.StatusCode != 404 {
		t.Errorf("/nope = %d, want 404", res.StatusCode)
	}
}

func TestSetupGuideRenders(t *testing.T) {
	h := newHandler()
	res, body := get(t, h, "/best-setup")
	if res.StatusCode != 200 {
		t.Fatalf("GET /best-setup = %d", res.StatusCode)
	}
	for _, want := range []string{
		"The best", "Update Goggle", "Update ESP32", "Naming Scheme", "ELRS",
		"https://github.com/bob9/hdzero-goggle/releases/tag/v9.5.45",
		"https://github.com/bob9/Backpack", "/setup/pilots", "Race channels as each race is cued",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("setup guide missing %q", want)
		}
	}
	if strings.Contains(body, "style=\"") {
		t.Error("inline style attribute would be blocked by the CSP")
	}
}

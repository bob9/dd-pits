package ddapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSource struct{}

func (fakeSource) Meeting(*http.Request, string) (*Meeting, bool)      { return nil, false }
func (fakeSource) CurrentMeeting(*http.Request) (string, string, bool) { return "", "", false }
func (fakeSource) Subscribe(*http.Request) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

func serverURL(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("GET %s: status %d", path, rec.Code)
	}
	var spec struct {
		Servers []struct{ URL string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi.json invalid: %v", err)
	}
	if len(spec.Servers) != 1 {
		t.Fatalf("want 1 server, got %d", len(spec.Servers))
	}
	return spec.Servers[0].URL
}

// TestOpenAPIServerBase verifies the spec's server URL tracks the path the API is
// mounted under — "/" at the root (local dd-pits) and the prefix when mounted
// behind http.StripPrefix (the cloud at /dronedash/api). This is the bug that
// made Swagger's "Try it out" hit /current.json instead of /dronedash/api/...
func TestOpenAPIServerBase(t *testing.T) {
	root := Handler(fakeSource{})
	if got := serverURL(t, root, "/openapi.json"); got != "/" {
		t.Fatalf("root mount: want server url \"/\", got %q", got)
	}

	mounted := http.StripPrefix("/dronedash/api", Handler(fakeSource{}))
	if got := serverURL(t, mounted, "/dronedash/api/openapi.json"); got != "/dronedash/api" {
		t.Fatalf("prefixed mount: want server url \"/dronedash/api\", got %q", got)
	}
}

// TestSentinelsAndCORS spot-checks the no-meeting sentinels + CORS through the
// registered handler.
func TestSentinelsAndCORS(t *testing.T) {
	h := Handler(fakeSource{})
	for path, want := range map[string]string{
		"/current.json":         `{"waiting":true}`,
		"/current-meeting.json": `{"loaded":false}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Fatalf("GET %s: want %s, got %s", path, want, got)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("GET %s: missing CORS header", path)
		}
	}
}

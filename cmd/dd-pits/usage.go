package main

// Telling the dd-pits website that an install is in use, and registering a
// club from the Status page.
//
// The install count is one small message a day: a random install ID, the
// version, the timer, and the operating system. No names, no pilots, no race
// data. It shows how many installs are actually in use, which is what keeps
// dd-pits' website sponsored. It is easy to switch off (Settings, the
// -no-usage-ping flag or DD_PITS_NO_USAGE_PING=1), and dd-pits works the
// same with no internet at all: a failed message is simply dropped.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	defaultHomeURL   = "https://dd-pits.threedigitalllamas.com.au"
	privacyURL       = defaultHomeURL + "/privacy"
	usagePingDelay   = 60 * time.Second
	usagePingEvery   = 24 * time.Hour
	usageHTTPTimeout = 10 * time.Second
)

// homeURL is the dd-pits website, overridable for testing.
func homeURL() string {
	if u := strings.TrimRight(os.Getenv("DD_PITS_HOME_URL"), "/"); u != "" {
		return u
	}
	return defaultHomeURL
}

// usagePingOffByEnv: DD_PITS_NO_USAGE_PING=1 (or any value but 0/false).
func usagePingOffByEnv() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("DD_PITS_NO_USAGE_PING")))
	return v != "" && v != "0" && v != "false"
}

// appKind says how this dd-pits was installed: the Mac app, the RotorHazard
// plugin (which starts it with -rotorhazard and a timer token), or a plain
// program.
func appKind(exe string, startedByRotorHazardPlugin bool) string {
	switch {
	case startedByRotorHazardPlugin:
		return "plugin"
	case strings.Contains(strings.ReplaceAll(exe, `\`, "/"), ".app/Contents/MacOS/"):
		return "mac-app"
	}
	return "binary"
}

func newInstallID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// usagePing is exactly what is sent; nothing else is.
type usagePing struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Timer   string `json:"timer"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	App     string `json:"app"`
}

func buildUsagePing(id, app string) usagePing {
	return usagePing{ID: id, Version: version, Timer: timingSystem, OS: runtime.GOOS, Arch: runtime.GOARCH, App: app}
}

var usageClient = &http.Client{Timeout: usageHTTPTimeout}

func postHomeJSON(ctx context.Context, url string, v any) (*http.Response, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dd-pits/"+version)
	return usageClient.Do(req)
}

// sendUsagePing sends one ping if it is switched on. Errors are returned for
// tests; the loop ignores them.
func sendUsagePing(ctx context.Context, cfg *localConfig, app string, offByFlag bool) error {
	if offByFlag || usagePingOffByEnv() || !cfg.UsagePingEnabled() {
		return errUsagePingOff
	}
	id := cfg.EnsureInstallID()
	if id == "" {
		return errors.New("no install id")
	}
	res, err := postHomeJSON(ctx, homeURL()+"/api/v1/ping", buildUsagePing(id, app))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("ping: %s", res.Status)
	}
	return nil
}

var errUsagePingOff = errors.New("usage ping switched off")

// runUsagePing pings a minute after start, then once a day. Venue laptops are
// often offline, so failures stay quiet. Never runs under go test.
func runUsagePing(ctx context.Context, cfg *localConfig, app string, offByFlag bool) {
	if testing.Testing() || cfg == nil {
		return
	}
	wait := usagePingDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = usagePingEvery
		if err := sendUsagePing(ctx, cfg, app, offByFlag); err != nil && !errors.Is(err, errUsagePingOff) && debugLogging() {
			log.Printf("dd-pits: install count not sent: %v", err)
		}
	}
}

// debugLogging: DD_PITS_DEBUG=1 shows the quiet messages.
func debugLogging() bool { return os.Getenv("DD_PITS_DEBUG") != "" }

// ── club registration ───────────────────────────────────────────────────────

type clubRegistration struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	Club      string `json:"club"`
	City      string `json:"city"`
	Timer     string `json:"timer"`
	Consent   bool   `json:"consent"`
	Source    string `json:"source"`
	InstallID string `json:"install_id"`
}

// errOffline: the website couldn't be reached at all.
var errOffline = errors.New("offline")

// forwardRegistration sends a club registration to the dd-pits website and
// returns the website's own plain-words error when it refuses one.
func forwardRegistration(ctx context.Context, reg clubRegistration) error {
	res, err := postHomeJSON(ctx, homeURL()+"/api/v1/register", reg)
	if err != nil {
		return errOffline
	}
	defer res.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out)
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	return fmt.Errorf("the dd-pits website said %s", res.Status)
}

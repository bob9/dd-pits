package dronedash

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Client fetches FPVTrackside live race-day endpoints. Endpoint shapes mirror
// drone-dashboard/backend/ingest/client.go (FPVTrackside Browser API).
//
// Exported so the venue-side dd-pits agent can reuse it instead of
// reimplementing the HTTP plumbing.
type Client struct {
	base *url.URL
	http *http.Client
}

// NewClient parses baseURL (e.g. "http://localhost:8080") and returns a Client.
func NewClient(baseURL string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("dronedash: parse FPVTrackside base url: %w", err)
	}
	return &Client{
		base: u,
		http: &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	u := *c.base
	u.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("dronedash: 404 %s", u.String())
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("dronedash: %s -> HTTP %d", u.String(), resp.StatusCode)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("dronedash: decode %s: %w", u.String(), err)
	}
	return nil
}

var eventIDRE = regexp.MustCompile(`var eventManager = new EventManager\("events\/([a-z0-9-]+)"`)

// FetchCurrentEventID scrapes the FPVTrackside index HTML for the active event.
// Same regex as drone-dashboard/backend/ingest/client.go and the FPVTrackside frontend.
func (c *Client) FetchCurrentEventID(ctx context.Context) (string, error) {
	u := *c.base
	u.Path = "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	m := eventIDRE.FindStringSubmatch(string(body))
	if m == nil {
		return "", fmt.Errorf("dronedash: no current event id in FPVTrackside index")
	}
	return m[1], nil
}

// FetchSelectedRace returns the FPVTrackside operator's currently-selected race
// id (RaceManager.CurrentRace) from the patched /current endpoint — the cued
// heat, surfaced while it's still live (un-run or running). Returns "" when
// nothing is selected. On a stock FPVTrackside (no /current route) this 404s and
// returns an error the caller treats as "no selection".
func (c *Client) FetchSelectedRace(ctx context.Context) (string, error) {
	var out struct {
		CurrentRace string `json:"CurrentRace"`
	}
	err := c.getJSON(ctx, "/current", &out)
	return out.CurrentRace, err
}

// FetchEvent returns the FPVTrackside event payload for eventID.
// FPVTrackside wraps single-event payloads in a one-element array.
func (c *Client) FetchEvent(ctx context.Context, eventID string) ([]ftEvent, error) {
	var out []ftEvent
	err := c.getJSON(ctx, fmt.Sprintf("/events/%s/Event.json", eventID), &out)
	return out, err
}

func (c *Client) FetchPilots(ctx context.Context, eventID string) ([]ftPilot, error) {
	var out []ftPilot
	err := c.getJSON(ctx, fmt.Sprintf("/events/%s/Pilots.json", eventID), &out)
	return out, err
}

func (c *Client) FetchChannels(ctx context.Context) ([]ftChannel, error) {
	var out []ftChannel
	err := c.getJSON(ctx, "/httpfiles/Channels.json", &out)
	return out, err
}

func (c *Client) FetchRounds(ctx context.Context, eventID string) ([]ftRound, error) {
	var out []ftRound
	err := c.getJSON(ctx, fmt.Sprintf("/events/%s/Rounds.json", eventID), &out)
	return out, err
}

func (c *Client) FetchRace(ctx context.Context, eventID, raceID string) ([]ftRace, error) {
	var out []ftRace
	err := c.getJSON(ctx, fmt.Sprintf("/events/%s/%s/Race.json", eventID, raceID), &out)
	return out, err
}

func (c *Client) FetchResults(ctx context.Context, eventID string) ([]ftResult, error) {
	var out []ftResult
	err := c.getJSON(ctx, fmt.Sprintf("/events/%s/Results.json", eventID), &out)
	return out, err
}

// FetchRaceResults returns the finishing positions for a single heat.
// FPVTrackside writes results per-race to {eventID}/{raceID}/Result.json; the
// event-level Results.json is not reliably populated, so the per-race file is
// the source of truth for a heat's finish order.
func (c *Client) FetchRaceResults(ctx context.Context, eventID, raceID string) ([]ftResult, error) {
	var out []ftResult
	err := c.getJSON(ctx, fmt.Sprintf("/events/%s/%s/Result.json", eventID, raceID), &out)
	return out, err
}

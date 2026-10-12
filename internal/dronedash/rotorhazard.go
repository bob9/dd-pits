package dronedash

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RHClient reads a RotorHazard timer through the dd-pits RotorHazard plugin
// (rotorhazard/dd_pits). The plugin translates RotorHazard's pilots, heats,
// rounds, laps and results into the same Snapshot shape FPVTrackside's API is
// read into, so everything downstream of the snapshot works unchanged.
//
// Two endpoints:
//
//	GET /dd-pits/api/version   {"version": "..."} — changes whenever anything visible does
//	GET /dd-pits/api/snapshot  the whole meeting as a Snapshot
//
// The version is a few bytes, so polling it at the active-race cadence costs
// the timer almost nothing; the snapshot is only fetched when it moves.
type RHClient struct {
	c *Client // reuses the HTTP plumbing; base is the RotorHazard server
}

// NewRHClient parses baseURL (e.g. "http://localhost:5000").
func NewRHClient(baseURL string) (*RHClient, error) {
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("dronedash: parse RotorHazard base url: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return &RHClient{c: &Client{base: u, http: &http.Client{Timeout: 5 * time.Second}}}, nil
}

// prefix keeps a RotorHazard served under a sub-path working.
func (r *RHClient) path(p string) string { return r.c.base.Path + p }

// FetchVersion returns the plugin's change token.
func (r *RHClient) FetchVersion(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := r.c.getJSON(ctx, r.path("/dd-pits/api/version"), &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// FetchSnapshot returns the whole meeting. Times arrive as UTC RFC3339, which
// normalizeSnapshotTimes passes through untouched.
func (r *RHClient) FetchSnapshot(ctx context.Context) (Snapshot, error) {
	var snap Snapshot
	if err := r.c.getJSON(ctx, r.path("/dd-pits/api/snapshot"), &snap); err != nil {
		return snap, err
	}
	if snap.Event.ID == "" {
		return snap, errors.New("dronedash: RotorHazard snapshot has no event")
	}
	normalizeSnapshotTimes(&snap)
	return snap, nil
}

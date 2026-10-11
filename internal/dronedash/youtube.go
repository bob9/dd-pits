package dronedash

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// YouTube stream "chapter" import. The stream description carries one
// timestamped line per heat, e.g.
//
//	00:06:38 Time Trial 1-2 | Bola 13.41 | Clog wog 15.60 | bob9 18.91
//	03:53:32 Race 1-1 | Mirella 1st | Enjenir 2nd | Gashy 3rd
//
// We parse the HH:MM:SS offset + the "<round>-<race>" key and attach a deep-link
// (video URL + &t=offset) to the matching dd_races row, so the dashboard race
// history can link each heat straight to the moment it starts in the stream.

var (
	chapterLineRe = regexp.MustCompile(`^\s*(\d{1,2}):(\d{2}):(\d{2})\s+(?:Time Trial|Race)\s+(\d+)-(\d+)\b(.*)$`)
	// Trailing per-pilot value: a lap time (13.41), a finishing position (1st),
	// or DNF — stripped so what's left is the pilot's name (which can itself end
	// in a digit, e.g. "bob9", so we anchor on the END not the first digit).
	trailingValRe = regexp.MustCompile(`\s*(?:DNF|\d+(?:st|nd|rd|th)|\d+(?:\.\d+)?)\s*$`)
	youTubeIDRe   = regexp.MustCompile(`(?:[?&]v=|youtu\.be/|/embed/|/live/)([A-Za-z0-9_-]{11})`)
)

type chapterEntry struct {
	OffsetSeconds int
	RoundNumber   int
	RaceNumber    int
	Pilots        []string // raw pilot names, used to disambiguate meetings
}

// ParseYouTubeID extracts the 11-char video id from a YouTube URL (watch?v=…,
// youtu.be/…, /embed/…, /live/…). Returns "" when none is found.
func ParseYouTubeID(u string) string {
	m := youTubeIDRe.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	return m[1]
}

// StreamLinkAt returns the stream URL with its playback start time set to secs
// (YouTube's ?t=). Any existing t/start parameter is replaced, and the rest of
// the URL is preserved — so it deep-links a bare *live-stream* URL (watch?v=…,
// youtu.be/…, /live/…) exactly as well as a finished VOD, which is what lets the
// per-race calculator anchor links from a stream that's still going.
func StreamLinkAt(raw string, secs int) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if secs < 0 {
		secs = 0
	}
	q := u.Query()
	q.Del("start")
	q.Set("t", fmt.Sprintf("%ds", secs))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ParseStreamOffset reads the playback start time (seconds) from a stream URL's
// t= (or legacy start=) parameter. Returns 0 when absent or unparseable.
func ParseStreamOffset(raw string) int {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	v := u.Query().Get("t")
	if v == "" {
		v = u.Query().Get("start")
	}
	v = strings.TrimSuffix(strings.TrimSpace(v), "s")
	n, _ := strconv.Atoi(v)
	if n < 0 {
		return 0
	}
	return n
}

// StripStreamTime returns the stream URL with any t/start parameter removed, so
// the editor can seed its shared "stream URL" field from an already-linked heat
// without carrying that one heat's offset.
func StripStreamTime(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Del("t")
	q.Del("start")
	u.RawQuery = q.Encode()
	return u.String()
}

// ── Stream start-time detection ─────────────────────────────────────────────
//
// YouTube embeds a broadcast's ACTUAL start instant in the watch page's player
// microformat — "liveBroadcastDetails":{"isLiveNow":…,"startTimestamp":"…"} —
// for a currently-live stream AND for the VOD it becomes. That lets the
// video-link calculator anchor every heat with no eyeballing: a heat's stream
// offset is simply its recorded start (server clock) minus the stream start.
// Plain uploads carry no liveBroadcastDetails, so they error cleanly.

var (
	streamStartRe = regexp.MustCompile(`"startTimestamp"\s*:\s*"([^"]+)"`)
	isLiveNowRe   = regexp.MustCompile(`"isLiveNow"\s*:\s*(true|false)`)
	// playabilityStatus tells us WHY metadata is missing — a datacenter IP
	// typically gets LOGIN_REQUIRED ("Sign in to confirm you're not a bot"),
	// which reads very differently from "this is a plain upload".
	playStatusRe = regexp.MustCompile(`"playabilityStatus"\s*:\s*\{\s*"status"\s*:\s*"([A-Z_]+)"`)
	playReasonRe = regexp.MustCompile(`"reason"\s*:\s*"([^"]*)"`)
)

// StreamStart is the broadcast metadata read off a YouTube watch page.
type StreamStart struct {
	Start  time.Time // instant the broadcast actually started
	IsLive bool      // still live now
}

// parseStreamStart extracts liveBroadcastDetails from a watch-page body.
func parseStreamStart(body string) (StreamStart, error) {
	m := streamStartRe.FindStringSubmatch(body)
	if m == nil {
		// Distinguish "YouTube refused to answer" (bot wall, region block…)
		// from "this genuinely isn't a live stream".
		if ps := playStatusRe.FindStringSubmatch(body); ps != nil && ps[1] != "OK" {
			msg := ps[1]
			if pr := playReasonRe.FindStringSubmatch(body); pr != nil && pr[1] != "" {
				msg += ": " + pr[1]
			}
			return StreamStart{}, fmt.Errorf("YouTube refused to return the video's metadata (%s)", msg)
		}
		return StreamStart{}, fmt.Errorf("the video has no stream start time in its metadata — only live streams (and their VODs) carry one, not plain uploads")
	}
	t, err := time.Parse(time.RFC3339, m[1])
	if err != nil {
		return StreamStart{}, fmt.Errorf("unparseable stream start time %q", m[1])
	}
	out := StreamStart{Start: t}
	if lm := isLiveNowRe.FindStringSubmatch(body); lm != nil && lm[1] == "true" {
		out.IsLive = true
	}
	return out, nil
}

// FetchStreamStart returns the broadcast's actual start time for the given
// YouTube URL — for a stream that's still live AND for its finished VOD (both
// carry liveBroadcastDetails / liveStreamingDetails; a plain upload has no
// broadcast start instant at all, which errors cleanly).
//
// apiKey (optional, YOUTUBE_API_KEY) selects the OFFICIAL Data API v3 path —
// the only one YouTube guarantees to answer from a datacenter/server IP. The
// keyless paths (watch-page scrape, then a ladder of InnerTube player clients)
// work from residential IPs but production servers routinely get bot-walled
// ("Sign in to confirm you're not a bot"), so a key is strongly recommended.
func FetchStreamStart(ctx context.Context, rawURL, apiKey string) (StreamStart, error) {
	vid := ParseYouTubeID(rawURL)
	if vid == "" {
		return StreamStart{}, fmt.Errorf("could not find a YouTube video id in %q", rawURL)
	}
	if apiKey != "" {
		return fetchStreamStartDataAPI(ctx, vid, apiKey)
	}
	ss, pageErr := fetchStreamStartWatchPage(ctx, vid)
	if pageErr == nil {
		return ss, nil
	}
	ss, apiErr := fetchStreamStartPlayerAPI(ctx, vid)
	if apiErr == nil {
		return ss, nil
	}
	return StreamStart{}, fmt.Errorf("%v (player API also failed: %v)", pageErr, apiErr)
}

// fetchStreamStartDataAPI uses the official YouTube Data API v3 — reliable
// from any IP. liveStreamingDetails.actualStartTime is set for a live stream
// the moment it goes live and stays on the VOD forever; actualEndTime appears
// once it ends (so its absence = still live). Absent entirely = plain upload.
func fetchStreamStartDataAPI(ctx context.Context, vid, key string) (StreamStart, error) {
	u := "https://www.googleapis.com/youtube/v3/videos?part=liveStreamingDetails&id=" +
		url.QueryEscape(vid) + "&key=" + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return StreamStart{}, err
	}
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return StreamStart{}, fmt.Errorf("could not reach the YouTube API: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Items []struct {
			LiveStreamingDetails *struct {
				ActualStartTime string `json:"actualStartTime"`
				ActualEndTime   string `json:"actualEndTime"`
			} `json:"liveStreamingDetails"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return StreamStart{}, fmt.Errorf("unreadable YouTube API response: %w", err)
	}
	if out.Error != nil {
		return StreamStart{}, fmt.Errorf("YouTube API: %s", out.Error.Message)
	}
	if len(out.Items) == 0 {
		return StreamStart{}, fmt.Errorf("video not found on YouTube (deleted or private?)")
	}
	d := out.Items[0].LiveStreamingDetails
	if d == nil || d.ActualStartTime == "" {
		return StreamStart{}, fmt.Errorf("the video has no stream start time in its metadata — only live streams (and their VODs) carry one, not plain uploads")
	}
	t, err := time.Parse(time.RFC3339, d.ActualStartTime)
	if err != nil {
		return StreamStart{}, fmt.Errorf("unparseable stream start time %q", d.ActualStartTime)
	}
	return StreamStart{Start: t, IsLive: d.ActualEndTime == ""}, nil
}

const ytDesktopUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

func fetchStreamStartWatchPage(ctx context.Context, vid string) (StreamStart, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.youtube.com/watch?v="+vid+"&hl=en", nil)
	if err != nil {
		return StreamStart{}, err
	}
	// Desktop UA + English locale keep the response shape predictable; the SOCS
	// cookie skips the EU consent interstitial that would hide the metadata.
	req.Header.Set("User-Agent", ytDesktopUA)
	req.Header.Set("Accept-Language", "en")
	req.AddCookie(&http.Cookie{Name: "SOCS", Value: "CAI"})
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return StreamStart{}, fmt.Errorf("could not reach YouTube: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return StreamStart{}, fmt.Errorf("YouTube returned HTTP %d for the video page", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	if err != nil {
		return StreamStart{}, err
	}
	return parseStreamStart(string(body))
}

// innerTubeClients is the ladder of player clients tried in order. Only the
// web-family clients return the microformat carrying liveBroadcastDetails
// (ANDROID/IOS answer but omit it entirely), so the rungs are WEB then MWEB —
// each classified differently by YouTube's bot wall, and MWEB returns the
// metadata even when playability is restricted. Verified live 2026-07.
var innerTubeClients = []struct{ name, payload, ua string }{
	{"web", `{"context":{"client":{"clientName":"WEB","clientVersion":"2.20250101.00.00","hl":"en"}},"videoId":%q}`, ytDesktopUA},
	{"mweb", `{"context":{"client":{"clientName":"MWEB","clientVersion":"2.20250311.03.00","hl":"en"}},"videoId":%q}`,
		"Mozilla/5.0 (iPhone; CPU iPhone OS 16_7_10 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.6 Mobile/15E148 Safari/604.1"},
}

// fetchStreamStartPlayerAPI asks the InnerTube player endpoint (the same call
// YouTube's own players make) for the video's microformat, which carries the
// identical liveBroadcastDetails fragment — parseStreamStart works on the JSON
// body unchanged. Tries each client in the ladder until one yields metadata.
func fetchStreamStartPlayerAPI(ctx context.Context, vid string) (StreamStart, error) {
	var lastErr error
	for _, c := range innerTubeClients {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://www.youtube.com/youtubei/v1/player?prettyPrint=false",
			strings.NewReader(fmt.Sprintf(c.payload, vid)))
		if err != nil {
			return StreamStart{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", c.ua)
		resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: could not reach YouTube: %w", c.name, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s: HTTP %d", c.name, resp.StatusCode)
			continue
		}
		if readErr != nil {
			lastErr = fmt.Errorf("%s: %w", c.name, readErr)
			continue
		}
		ss, err := parseStreamStart(string(body))
		if err == nil {
			return ss, nil
		}
		lastErr = fmt.Errorf("%s: %w", c.name, err)
	}
	return StreamStart{}, lastErr
}

func parseChapters(text string) []chapterEntry {
	var out []chapterEntry
	for _, line := range strings.Split(text, "\n") {
		m := chapterLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		hh, _ := strconv.Atoi(m[1])
		mm, _ := strconv.Atoi(m[2])
		ss, _ := strconv.Atoi(m[3])
		round, _ := strconv.Atoi(m[4])
		race, _ := strconv.Atoi(m[5])
		e := chapterEntry{OffsetSeconds: hh*3600 + mm*60 + ss, RoundNumber: round, RaceNumber: race}
		for _, seg := range strings.Split(m[6], "|") {
			seg = strings.ReplaceAll(seg, "🔥", "")
			seg = trailingValRe.ReplaceAllString(seg, "")
			if name := strings.TrimSpace(seg); name != "" {
				e.Pilots = append(e.Pilots, name)
			}
		}
		out = append(out, e)
	}
	return out
}

// normName lowercases and strips non-alphanumerics so "Clog wog" == "clogwog"
// and "Red2rotor" == "red2rotor" across the timing data and the chapter text.
func normName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

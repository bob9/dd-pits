package qwentts

// How fast is REMOTE speech, compared with generating on the venue laptop?
//
// The local model reaches first audio in ~110ms and needs a GPU that the
// commentary model also wants. A hosted renderer would free that GPU entirely —
// but it puts the Pacific between the lap crossing and the call, and a race call
// that arrives late is worth less than one that arrives at all.
//
// So the number that decides it is TIME TO FIRST AUDIO, not total render time:
// the caller streams, so what matters is when the first sample can be played.
//
// Opt-in and it calls a paid API on the operator's own key:
//   RUN_NET_TESTS=1 QWEN_API_KEY=… go test ./internal/platform/qwentts/ -run Latency -v

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// benchLapCalls are real race calls — short, which is the case that matters.
var benchLapCalls = []string{
	"bob9 lap 1 in 11.42 in 1st",
	"Fizz lap 2 in 11.50 in 2nd",
	"Holeshot Willman 2.08",
}

// firstAudioLatency times the gap between sending the request and the first
// audio bytes arriving, which is when playback could begin.
func firstAudioLatency(t *testing.T, c *Client, text, voice string) (first, total time.Duration, bytesOut int) {
	t.Helper()

	body := map[string]interface{}{
		"model": c.model,
		"input": map[string]interface{}{"text": text, "voice": voice},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		c.baseURL+"/api/v1/services/aigc/multimodal-generation/generation", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		t.Skipf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("API returned %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		// The first line carrying audio is the one that matters.
		if first == 0 && strings.Contains(line, "audio") && len(line) > 200 {
			first = time.Since(start)
		}
		bytesOut += len(line)
	}
	return first, time.Since(start), bytesOut
}

func TestRemoteVsLocalLatency(t *testing.T) {
	if os.Getenv("RUN_NET_TESTS") != "1" {
		t.Skip("set RUN_NET_TESTS=1 (calls a paid API)")
	}
	key := os.Getenv("QWEN_API_KEY")
	if key == "" {
		t.Skip("no QWEN_API_KEY")
	}

	c, err := New(Config{APIKey: key})
	if err != nil {
		t.Fatal(err)
	}

	fmt.Printf("\n  endpoint: %s\n\n", c.baseURL)

	var sumFirst, sumTotal time.Duration
	for i, call := range benchLapCalls {
		first, total, n := firstAudioLatency(t, c, call, DefaultVoicePBP)
		sumFirst += first
		sumTotal += total
		fmt.Printf("  call %d  first audio %6dms   complete %6dms   (%d bytes)\n",
			i+1, first.Milliseconds(), total.Milliseconds(), n)
	}

	n := time.Duration(len(benchLapCalls))
	meanFirst := sumFirst / n
	fmt.Printf("\n  REMOTE mean first audio : %dms\n", meanFirst.Milliseconds())
	fmt.Printf("  LOCAL  mean first audio : ~110ms (measured on this machine)\n")
	fmt.Printf("  difference              : %+dms\n\n", meanFirst.Milliseconds()-110)

	// No pass/fail threshold: the answer is the number, and whether it is
	// acceptable is a race-day judgement about a venue's connection, not
	// something a unit test should decide. Asserting one here would also make
	// this fail on any network but the one it was written on.
	switch {
	case meanFirst < 300*time.Millisecond:
		t.Logf("VERDICT: remote is competitive — it would free the GPU for commentary")
	case meanFirst < 800*time.Millisecond:
		t.Logf("VERDICT: remote is usable but noticeably slower than local")
	default:
		t.Logf("VERDICT: remote is too slow for a lap call at %dms", meanFirst.Milliseconds())
	}
}

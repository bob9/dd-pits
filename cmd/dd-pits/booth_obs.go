package main

// OBS control for broadcast sync. dd-pits realises the booth's broadcast buffer
// B as a VIDEO delay in OBS over obs-websocket v5, so the video and the held
// booth audio move together. OBS's built-in "Render Delay" filter (gpu_delay)
// caps at 20 s per filter, so a larger B is reached by STACKING filters on the
// source (2×20 = 40 s, …). The source's own audio is delayed to match via its
// audio sync offset; the booth media source is a SEPARATE source and is never
// delayed here.
//
// Everything is best-effort: OBS being down or a bad request only triggers a
// reconnect and a log line telling the operator the delay to set by hand. It
// never blocks the audio bus.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const (
	obsFilterPrefix = "ddsync"        // our stacked render-delay filters: ddsync-0, ddsync-1, …
	obsFilterCapMs  = 20000           // OBS gpu_delay (Render Delay) per-filter maximum
	obsAudioInput   = "dd-pits booth" // the media input we add to every scene
)

type boothOBS struct {
	url      string
	password string
	// sources to delay. Empty = EVERY scene (a scene is itself a filterable
	// source in OBS), so the video is delayed no matter which scene is live and
	// the operator never has to name one.
	sources     []string
	injectAudio bool   // add the booth media source to every scene
	audioURL    string // http://…/booth.wav for the injected media input

	mu        sync.Mutex
	targetMs  int64
	appliedMs int64
	connected bool
	wake      chan struct{}
}

func newBoothOBS(url, password string, sources []string, injectAudio bool, audioURL string) *boothOBS {
	return &boothOBS{
		url: url, password: password, sources: sources,
		injectAudio: injectAudio, audioURL: audioURL,
		wake: make(chan struct{}, 1),
	}
}

// setBufferMs updates the desired video delay; the run loop applies it to OBS.
func (o *boothOBS) setBufferMs(ms int64) {
	o.mu.Lock()
	changed := ms != o.targetMs
	o.targetMs = ms
	o.mu.Unlock()
	if changed {
		select {
		case o.wake <- struct{}{}:
		default:
		}
	}
}

func (o *boothOBS) status() (connected bool, appliedMs int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.connected, o.appliedMs
}

// run keeps a connection to OBS and applies the target delay on every change.
func (o *boothOBS) run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("booth/obs: recovered from panic: %v (video-delay control off)", r)
		}
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		err := o.session(ctx)
		o.mu.Lock()
		o.connected, o.appliedMs = false, 0
		want := o.targetMs
		o.mu.Unlock()
		if err != nil {
			log.Printf("booth/obs: %v — retrying in %s (set OBS video delay to %ds by hand meanwhile)", err, backoff, want/1000)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (o *boothOBS) session(ctx context.Context) error {
	cfg, err := websocket.NewConfig(o.url, "http://localhost/")
	if err != nil {
		return fmt.Errorf("bad -obs-url %q: %w", o.url, err)
	}
	cfg.Protocol = []string{"obswebsocket.json"}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer ws.Close()

	// Hello (op 0) → Identify (op 1) → Identified (op 2).
	var hello struct {
		Op int `json:"op"`
		D  struct {
			Authentication *struct {
				Challenge string `json:"challenge"`
				Salt      string `json:"salt"`
			} `json:"authentication"`
		} `json:"d"`
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := websocket.JSON.Receive(ws, &hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	identD := map[string]any{"rpcVersion": 1, "eventSubscriptions": 0}
	if hello.D.Authentication != nil {
		identD["authentication"] = obsAuth(o.password, hello.D.Authentication.Salt, hello.D.Authentication.Challenge)
	}
	if err := websocket.JSON.Send(ws, map[string]any{"op": 1, "d": identD}); err != nil {
		return fmt.Errorf("identify: %w", err)
	}
	var id struct {
		Op int `json:"op"`
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := websocket.JSON.Receive(ws, &id); err != nil {
		return fmt.Errorf("identify recv: %w", err)
	}
	if id.Op != 2 {
		return fmt.Errorf("authentication rejected (check -obs-password)")
	}
	o.mu.Lock()
	o.connected, o.appliedMs = true, -1
	o.mu.Unlock()
	scope := "every scene"
	if len(o.sources) > 0 {
		scope = strings.Join(o.sources, ", ")
	}
	audioNote := ""
	if o.injectAudio {
		audioNote = " + injecting commentary audio into every scene"
	}
	log.Printf("booth/obs: connected — delaying video on %s%s", scope, audioNote)

	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		if err := o.apply(ws); err != nil {
			return fmt.Errorf("apply: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-o.wake:
		case <-tick.C: // periodic re-assert (survives an OBS scene/filter edit)
		}
	}
}

// apply brings OBS in line with the current buffer: every target scene's video
// is delayed by the stacked render-delay filters, every OTHER audio input is
// sync-offset by the same amount so it stays with the delayed video, and (opt)
// the booth media source is present in every scene so the commentary is audible
// whatever scene is live. The booth audio is never offset — it's held live by
// the mixer to land on the mark.
func (o *boothOBS) apply(ws *websocket.Conn) error {
	o.mu.Lock()
	target, applied := o.targetMs, o.appliedMs
	o.mu.Unlock()
	if target == applied {
		return nil
	}
	delays := splitDelay(target, obsFilterCapMs)

	scenes, err := o.sceneList(ws)
	if err != nil {
		return err
	}
	targets := o.sources
	if len(targets) == 0 {
		targets = scenes // no source named → delay EVERY scene
	}
	for _, name := range targets {
		if err := o.delaySource(ws, name, delays); err != nil {
			return err
		}
	}
	o.syncAudioInputs(ws, target) // keep all non-booth audio with the delayed video
	if o.injectAudio {
		o.ensureAudioEverywhere(ws, scenes)
	}

	o.mu.Lock()
	o.appliedMs = target
	o.mu.Unlock()
	log.Printf("booth/obs: video delay %.1fs (%d filter%s) across %d target%s%s",
		float64(target)/1000, len(delays), plural(len(delays)), len(targets), plural(len(targets)),
		map[bool]string{true: " + audio injected", false: ""}[o.injectAudio])
	return nil
}

// sceneList returns every scene name.
func (o *boothOBS) sceneList(ws *websocket.Conn) ([]string, error) {
	raw, err := obsRequest(ws, "GetSceneList", map[string]any{})
	if err != nil {
		return nil, err
	}
	var d struct {
		Scenes []struct {
			SceneName string `json:"sceneName"`
		} `json:"scenes"`
	}
	json.Unmarshal(raw, &d)
	out := make([]string, 0, len(d.Scenes))
	for _, s := range d.Scenes {
		out = append(out, s.SceneName)
	}
	return out, nil
}

// delaySource makes one source's (or scene's) stacked render-delay filters sum
// to the buffer, creating/updating/enabling ddsync-N and dropping any surplus.
func (o *boothOBS) delaySource(ws *websocket.Conn, source string, delays []int64) error {
	existing := map[string]bool{}
	if raw, err := obsRequest(ws, "GetSourceFilterList", map[string]any{"sourceName": source}); err == nil {
		var lst struct {
			Filters []struct {
				FilterName string `json:"filterName"`
			} `json:"filters"`
		}
		json.Unmarshal(raw, &lst)
		for _, f := range lst.Filters {
			existing[f.FilterName] = true
		}
	}
	for i, d := range delays {
		name := fmt.Sprintf("%s-%d", obsFilterPrefix, i)
		if existing[name] {
			if _, err := obsRequest(ws, "SetSourceFilterSettings", map[string]any{
				"sourceName": source, "filterName": name,
				"filterSettings": map[string]any{"delay_ms": d}, "overlay": false,
			}); err != nil {
				return err
			}
		} else if _, err := obsRequest(ws, "CreateSourceFilter", map[string]any{
			"sourceName": source, "filterName": name,
			"filterKind": "gpu_delay", "filterSettings": map[string]any{"delay_ms": d},
		}); err != nil {
			return err
		}
		obsRequest(ws, "SetSourceFilterEnabled", map[string]any{
			"sourceName": source, "filterName": name, "filterEnabled": true,
		})
		delete(existing, name)
	}
	for name := range existing {
		if strings.HasPrefix(name, obsFilterPrefix+"-") {
			obsRequest(ws, "RemoveSourceFilter", map[string]any{"sourceName": source, "filterName": name})
		}
	}
	return nil
}

// syncAudioInputs delays every audio input EXCEPT the booth by the buffer, so
// live audio (mics, feed audio) stays with the now-delayed video. Best-effort.
func (o *boothOBS) syncAudioInputs(ws *websocket.Conn, target int64) {
	raw, err := obsRequest(ws, "GetInputList", map[string]any{})
	if err != nil {
		return
	}
	var d struct {
		Inputs []struct {
			InputName string `json:"inputName"`
		} `json:"inputs"`
	}
	json.Unmarshal(raw, &d)
	for _, in := range d.Inputs {
		if in.InputName == obsAudioInput {
			continue // the booth's own audio is held live to land on the mark
		}
		obsRequest(ws, "SetInputAudioSyncOffset", map[string]any{
			"inputName": in.InputName, "inputAudioSyncOffset": target,
		})
	}
}

// ensureAudioEverywhere makes the booth media source exist and sit in every
// scene, so the commentary is heard whatever scene is live. Best-effort.
func (o *boothOBS) ensureAudioEverywhere(ws *websocket.Conn, scenes []string) {
	if o.audioURL == "" || len(scenes) == 0 {
		return
	}
	// Create the input (also places it in the first scene); a no-op if it exists.
	obsRequest(ws, "CreateInput", map[string]any{
		"sceneName": scenes[0],
		"inputName": obsAudioInput,
		"inputKind": "ffmpeg_source",
		"inputSettings": map[string]any{
			"is_local_file":       false,
			"input":               o.audioURL,
			"reconnect_delay_sec": 2,
		},
		"sceneItemEnabled": true,
	})
	// Add it to any scene that doesn't have it yet.
	for _, sc := range scenes {
		if _, err := obsRequest(ws, "GetSceneItemId", map[string]any{
			"sceneName": sc, "sourceName": obsAudioInput,
		}); err == nil {
			continue // already present
		}
		obsRequest(ws, "CreateSceneItem", map[string]any{
			"sceneName": sc, "sourceName": obsAudioInput, "sceneItemEnabled": true,
		})
	}
}

// obsRequest sends one op:6 request and returns the responseData of its op:7
// reply, skipping any events/other replies that interleave.
func obsRequest(ws *websocket.Conn, reqType string, data map[string]any) (json.RawMessage, error) {
	reqID := fmt.Sprintf("%s-%d", reqType, time.Now().UnixNano())
	if err := websocket.JSON.Send(ws, map[string]any{
		"op": 6, "d": map[string]any{"requestType": reqType, "requestId": reqID, "requestData": data},
	}); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ws.SetReadDeadline(deadline)
		var msg struct {
			Op int             `json:"op"`
			D  json.RawMessage `json:"d"`
		}
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			return nil, err
		}
		if msg.Op != 7 {
			continue // event or other op — ignore
		}
		var d struct {
			RequestID     string `json:"requestId"`
			RequestStatus struct {
				Result  bool   `json:"result"`
				Code    int    `json:"code"`
				Comment string `json:"comment"`
			} `json:"requestStatus"`
			ResponseData json.RawMessage `json:"responseData"`
		}
		json.Unmarshal(msg.D, &d)
		if d.RequestID != reqID {
			continue
		}
		if !d.RequestStatus.Result {
			return nil, fmt.Errorf("%s: %s (code %d)", reqType, d.RequestStatus.Comment, d.RequestStatus.Code)
		}
		return d.ResponseData, nil
	}
}

// splitDelay breaks a total delay into per-filter chunks no larger than capMs.
func splitDelay(total, capMs int64) []int64 {
	if total <= 0 || capMs <= 0 {
		return nil
	}
	var out []int64
	for total > 0 {
		d := total
		if d > capMs {
			d = capMs
		}
		out = append(out, d)
		total -= d
	}
	return out
}

// obsAuth computes the obs-websocket v5 authentication string.
func obsAuth(password, salt, challenge string) string {
	h1 := sha256.Sum256([]byte(password + salt))
	b1 := base64.StdEncoding.EncodeToString(h1[:])
	h2 := sha256.Sum256([]byte(b1 + challenge))
	return base64.StdEncoding.EncodeToString(h2[:])
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

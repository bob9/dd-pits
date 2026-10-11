package main

// Running the models on a DIFFERENT machine from the race.
//
// The race-director PC is usually a Windows laptop, and the local models are
// MLX — Apple Silicon only. So the venue has a choice between a hosted service
// (works anywhere, needs the internet, and a dropout means silence) and no
// local models at all.
//
// This is the third option: put a MacBook on the same network and let it do the
// generating. The race PC runs FPVTrackside and dd-pits; the Mac runs the
// voice and the writer and answers over the LAN. Nothing leaves the venue, and
// a LAN round-trip is under a millisecond — against the ~110ms the model itself
// takes, that is free.
//
//   on the MacBook:   dd-pits -model-host :9099 -local-voice-python … -local-script-model …
//   on the race PC:   dd-pits -serve -race-audio -model-url http://macbook.local:9099
//
// The wire format is deliberately the same shape as the sidecar's own protocol —
// render whole, render streaming, write — so the remote path reuses the local
// engine's semantics rather than inventing new ones.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bob9/dd-pits/internal/localchat"
	"github.com/bob9/dd-pits/internal/tts"
)

// modelHost serves a local voice and writer to other machines.
type modelHost struct {
	voice  *tts.LocalEngine
	writer *localchat.Client
}

// serveModelHost brings the models up and serves them until ctx ends.
//
// Blocking, because this is the whole job of the process in host mode: there is
// no timing loop to protect and nothing else to get on with.
func serveModelHost(ctx context.Context, addr string, voiceCfg tts.LocalConfig, scriptModel string) error {
	h := &modelHost{}

	if voiceCfg.Python != "" {
		if voiceCfg.Script == "" {
			voiceCfg.Script = defaultLocalVoiceScript()
		}
		log.Printf("model host: loading the voice…")
		v, err := tts.StartLocal(ctx, voiceCfg)
		if err != nil {
			return fmt.Errorf("voice: %w", err)
		}
		h.voice = v
		log.Printf("model host: voice ready")
	}

	if scriptModel != "" && voiceCfg.Python != "" {
		log.Printf("model host: loading the writer (%s) — a large model takes minutes…", scriptModel)
		w, err := localchat.Start(ctx, localchat.Config{
			Python: voiceCfg.Python,
			Script: voiceCfg.Script,
			Model:  scriptModel,
		})
		if err != nil {
			// The voice is the more important of the two: race calls are the
			// job and commentary is colour. Losing the writer must not take
			// the host down with it.
			log.Printf("model host: writer unavailable (%v) — serving the voice only", err)
		} else {
			h.writer = w
			log.Printf("model host: writer ready")
		}
	}

	if h.voice == nil && h.writer == nil {
		return fmt.Errorf("nothing to serve: give -local-voice-python and/or -local-script-model")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.health)
	mux.HandleFunc("/render", h.render)
	mux.HandleFunc("/stream", h.stream)
	mux.HandleFunc("/write", h.write)

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	for _, ip := range localAddresses() {
		log.Printf("model host: serving on http://%s%s", ip, addr)
	}
	return srv.ListenAndServe()
}

func (h *modelHost) health(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]bool{
		"voice":  h.voice != nil,
		"writer": h.writer != nil,
	})
}

type textReq struct {
	Text   string `json:"text"`
	Prompt string `json:"prompt"`
}

func decodeText(r *http.Request) (textReq, error) {
	var req textReq
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	return req, err
}

// render returns a whole utterance as base64 PCM.
func (h *modelHost) render(w http.ResponseWriter, r *http.Request) {
	if h.voice == nil {
		http.Error(w, "no voice on this host", http.StatusServiceUnavailable)
		return
	}
	req, err := decodeText(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	pcm, rate, err := h.voice.Speak(r.Context(), tts.Request{Text: req.Text})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"pcm":  base64.StdEncoding.EncodeToString(pcm),
		"rate": rate,
	})
}

// stream sends audio AS IT IS GENERATED, one base64 fragment per line.
//
// This is the endpoint that matters. Rendering whole would put the entire
// generation time in front of the first sample; streaming keeps the ~110ms
// first-audio the local path gets, with only a LAN hop added.
func (h *modelHost) stream(w http.ResponseWriter, r *http.Request) {
	if h.voice == nil {
		http.Error(w, "no voice on this host", http.StatusServiceUnavailable)
		return
	}
	req, err := decodeText(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, err := h.voice.SpeakStream(r.Context(), req.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	for pcm := range ch {
		if len(pcm) == 0 {
			continue
		}
		// Flushed per fragment: buffering here would silently undo the
		// streaming this endpoint exists for.
		fmt.Fprintf(w, "%s\n", base64.StdEncoding.EncodeToString(pcm))
		fl.Flush()
	}
}

// write generates text from a prompt.
func (h *modelHost) write(w http.ResponseWriter, r *http.Request) {
	if h.writer == nil {
		http.Error(w, "no writer on this host", http.StatusServiceUnavailable)
		return
	}
	req, err := decodeText(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	out, err := h.writer.GenerateText(r.Context(), req.Prompt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"text": out})
}

// localAddresses lists the addresses a race PC could reach this host on, so the
// operator does not have to go looking for the Mac's IP.
func localAddresses() []string {
	out := []string{"localhost"}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			out = append(out, ipnet.IP.String())
		}
	}
	return out
}

// ── the client side, on the race PC ──────────────────────────────────────────

// remoteEngine speaks by asking a model host on the network.
//
// Satisfies tts.Engine, so the announcer, the pack, the mixer and the
// priorities are all unchanged — only where the samples come from differs.
type remoteEngine struct {
	base string
	http *http.Client
}

func newRemoteEngine(base string) *remoteEngine {
	return &remoteEngine{
		base: strings.TrimRight(base, "/"),
		// No overall timeout: a streamed call is read fragment by fragment and
		// a deadline on the whole response would cut a long line off mid-word.
		// Per-request context supplies the bound instead.
		http: &http.Client{},
	}
}

func (e *remoteEngine) Provider() string                    { return "remote" }
func (e *remoteEngine) SetUsageHook(func(string, int, int)) {}

func (e *remoteEngine) Speak(ctx context.Context, req tts.Request) ([]byte, int, error) {
	body, _ := json.Marshal(textReq{Text: req.Text})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+"/render", strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, err
	}
	hr.Header.Set("Content-Type", "application/json")

	resp, err := e.http.Do(hr)
	if err != nil {
		return nil, 0, fmt.Errorf("model host unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("model host returned %s", resp.Status)
	}

	var out struct {
		PCM  string `json:"pcm"`
		Rate int    `json:"rate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, err
	}
	pcm, err := base64.StdEncoding.DecodeString(out.PCM)
	return pcm, out.Rate, err
}

// SpeakStream mirrors the local engine's streaming, over the network.
func (e *remoteEngine) SpeakStream(ctx context.Context, text string) (<-chan []byte, error) {
	body, _ := json.Marshal(textReq{Text: text})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+"/stream", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")

	resp, err := e.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("model host unreachable: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("model host returned %s", resp.Status)
	}

	out := make(chan []byte, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
		for sc.Scan() {
			pcm, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sc.Text()))
			if err != nil || len(pcm) == 0 {
				continue
			}
			select {
			case out <- pcm:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// remoteWriter generates commentary on a model host.
type remoteWriter struct {
	base string
	http *http.Client
}

func newRemoteWriter(base string) *remoteWriter {
	return &remoteWriter{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 120 * time.Second},
	}
}

func (w *remoteWriter) Model() string { return "remote" }

func (w *remoteWriter) GenerateText(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(textReq{Prompt: prompt})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, w.base+"/write", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	hr.Header.Set("Content-Type", "application/json")

	resp, err := w.http.Do(hr)
	if err != nil {
		return "", fmt.Errorf("model host unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("model host returned %s", resp.Status)
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Text, nil
}

// probeModelHost reports what a host can do, so a misconfiguration is a clear
// message at startup rather than silence during a heat.
func probeModelHost(base string) (voice, writer bool, err error) {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(strings.TrimRight(base, "/") + "/healthz")
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()

	var out struct{ Voice, Writer bool }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, false, err
	}
	return out.Voice, out.Writer, nil
}

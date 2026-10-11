package tts

// A LOCAL speech engine: a model on the venue laptop, no network.
//
// The cloud engines are fine for the website, where a few seconds of latency is
// invisible and a meeting's worth of audio is generated in advance. A race day
// is neither. Calls happen continuously, must land within a lap of the moment
// they describe, and a venue's internet is frequently the worst part of the
// setup — so the audio that matters most is the audio that must not depend on
// it.
//
// The model runs under MLX, which is Python-only, so this drives a small
// sidecar over a line protocol. The sidecar RENDERS and returns samples rather
// than playing them: the caller owns its own audio bus, and having two programs
// on one output is precisely the contention this exists to remove.

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// localLog surfaces the sidecar's own progress and errors. Package-level so a
// caller can redirect it; silent by default would hide a model that will not
// load, which is the failure most worth seeing.
var localLog = func(msg string) { log.Printf("local speech: %s", msg) }

// LocalEngine speaks using a model on this machine.
type LocalEngine struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	replies chan string

	ready         bool
	renderTimeout time.Duration
	usage         func(kind string, inputTokens, outputTokens int)
}

// LocalConfig describes how to start the sidecar.
type LocalConfig struct {
	// Python with the model's dependencies installed.
	Python string

	// The sidecar script.
	Script string

	// Model to render with.
	Model string

	// ReferenceAudio and ReferenceText clone a voice. Both or neither: the
	// model aligns the words to the audio, and a wrong transcript gives a worse
	// clone rather than an error.
	ReferenceAudio string
	ReferenceText  string

	// StartTimeout bounds waiting for the model to load. A large model takes
	// minutes; a crashed one never answers at all, and the caller needs to know
	// the difference quickly enough to fall back.
	StartTimeout time.Duration

	// RenderTimeout bounds one line.
	RenderTimeout time.Duration
}

func (c LocalConfig) withDefaults() LocalConfig {
	if c.StartTimeout == 0 {
		c.StartTimeout = 90 * time.Second
	}
	if c.RenderTimeout == 0 {
		c.RenderTimeout = 45 * time.Second
	}
	if c.Model == "" {
		c.Model = "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-bf16"
	}
	return c
}

// StartLocal launches the sidecar and waits for it to be ready.
//
// An error here is not fatal to a race day: the caller keeps whatever engine it
// had, or goes quiet. Losing the local voice costs quality, never the meeting.
func StartLocal(ctx context.Context, cfg LocalConfig) (*LocalEngine, error) {
	cfg = cfg.withDefaults()

	if cfg.Python == "" || cfg.Script == "" {
		return nil, fmt.Errorf("local speech needs a python and a script path")
	}

	// Always render-only. This engine returns samples and never plays them —
	// the caller owns its own audio bus, and two programs on one output device
	// is the contention that bus exists to remove.
	args := []string{cfg.Script, "--model", cfg.Model, "--render-only"}
	if cfg.ReferenceAudio != "" && cfg.ReferenceText != "" {
		args = append(args, "--ref-audio", cfg.ReferenceAudio, "--ref-text", cfg.ReferenceText)
	}

	cmd := exec.Command(cfg.Python, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("local speech stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("local speech stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("local speech stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start local speech: %w", err)
	}

	e := &LocalEngine{
		cmd:   cmd,
		stdin: stdin,
		// Deep enough for a WHOLE streamed call.
		//
		// 32 was sized for request/reply, where one line comes back per
		// request. A streamed call is ~60 CHUNK lines (50ms of audio each), so
		// the buffer filled and the reader below discarded the overflow — which
		// is the END of the line, heard as a call cut off hard on its last
		// word. Sizing this for the longest call means the drop path is
		// unreachable in normal use.
		replies: make(chan string, 4096),
	}

	// One reader, draining into a channel. Reading on demand would leave a
	// timed-out read still blocked and swallowing the NEXT reply, which
	// desynchronises the pipe for good.
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // rendered audio arrives base64 on one line
		for sc.Scan() {
			select {
			case e.replies <- sc.Text():
			default:
				// Only reachable if nothing is draining — a caller that timed
				// out and walked away. Never silent: dropping a CHUNK truncates
				// a call mid-word, and that failure is invisible everywhere
				// else (the logs show a clean render, the audio is short).
				localLog("dropped a reply line — the buffer is full and nothing is reading; " +
					"a streamed call may be cut short")
			}
		}
		close(e.replies)
	}()

	// The sidecar reports progress and failures on stderr. Surfacing it is the
	// only way to diagnose a model that will not load.
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "#"))
			if line != "" {
				localLog(line)
			}
		}
	}()

	if !e.waitFor("READY", cfg.StartTimeout) {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("local speech never became ready within %s", cfg.StartTimeout)
	}

	e.ready = true
	e.renderTimeout = cfg.RenderTimeout
	return e, nil
}

// Speak renders a line and returns 16-bit mono PCM with its sample rate.
func (e *LocalEngine) Speak(ctx context.Context, req Request) ([]byte, int, error) {
	if e == nil || !e.ready {
		return nil, 0, fmt.Errorf("local speech not running")
	}

	// The sidecar takes the words only. Delivery direction is baked into the
	// cloned voice, and a preamble would be READ ALOUD rather than followed.
	line := closeSentence(strings.Join(strings.Fields(req.Text), " "))
	if line == "" {
		return nil, 0, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := fmt.Fprintf(e.stdin, "RENDER %s\n", line); err != nil {
		e.ready = false
		return nil, 0, fmt.Errorf("local speech write: %w", err)
	}

	reply, ok, aborted := e.waitForPrefixCtx(ctx, "AUDIO ", e.renderTimeout)
	if aborted {
		// The caller cancelled: the render is dead weight on a shared GPU —
		// racing has started and the live calls need the engine. STOP makes
		// the sidecar's generation loop actually break (cancelling only the
		// wait left it rendering to completion regardless), and the partial
		// AUDIO reply is then consumed so the NEXT request cannot mistake it
		// for its own answer — the line protocol has no request ids.
		_, _ = fmt.Fprintln(e.stdin, "STOP")
		_, _ = e.waitForPrefix("AUDIO ", 10*time.Second)
		return nil, 0, ctx.Err()
	}
	if !ok {
		return nil, 0, fmt.Errorf("local speech timed out rendering %q", line)
	}

	pcm, err := base64.StdEncoding.DecodeString(strings.TrimSpace(reply))
	if err != nil {
		return nil, 0, fmt.Errorf("local speech audio: %w", err)
	}
	return pcm, LocalSampleRate, nil
}

// closeSentence gives a line a terminal full stop when it carries none.
//
// The cloned voice ends utterances the way its reference recording taught it
// to, and a short reference that stops hard teaches it to stop hard. On an
// unpunctuated line the model has no marker for where the utterance closes,
// and it was heard clipping the final word — "in first" arriving as "in firs".
// Race-call templates end in bare figures and ordinals ("Willman lap 3 in
// 10.93 in 1st"), so nearly every call was unpunctuated.
//
// A full stop costs nothing to speech — it is not read aloud — and gives the
// model an explicit end to generate towards rather than a guess.
func closeSentence(line string) string {
	switch {
	case line == "":
		return line
	case strings.HasSuffix(line, "."), strings.HasSuffix(line, "!"),
		strings.HasSuffix(line, "?"):
		return line
	}
	return line + "."
}

// SpeakStream renders a line and delivers the audio AS IT IS GENERATED.
//
// The reason this exists rather than Speak: generation runs at roughly real
// time, so waiting for a finished utterance puts a two-second call two seconds
// behind the thing it describes. A race call has to land ON the lap — a lap
// time that arrives after the pilot has crossed again is a correction, not a
// call.
//
// The returned channel yields 16-bit mono PCM fragments at LocalSampleRate and
// is closed when the line is complete. It is ALWAYS closed, including on error,
// so a caller holding a cue open for it is never left waiting.
func (e *LocalEngine) SpeakStream(ctx context.Context, text string) (<-chan []byte, error) {
	if e == nil || !e.ready {
		return nil, fmt.Errorf("local speech not running")
	}

	line := closeSentence(strings.Join(strings.Fields(text), " "))
	if line == "" {
		return nil, nil
	}

	out := make(chan []byte, 16)

	// The pipe is held for the whole stream. Interleaving a second request
	// would mix two lines' fragments into one cue, so streams are serialised
	// exactly as whole renders are.
	e.mu.Lock()
	if _, err := fmt.Fprintf(e.stdin, "STREAM %s\n", line); err != nil {
		e.mu.Unlock()
		e.ready = false
		return nil, fmt.Errorf("local speech write: %w", err)
	}

	go func() {
		defer close(out)
		defer e.mu.Unlock()

		// An abandoned stream must abort the sidecar's GENERATION, not just
		// stop reading. Every drop path (a perished call, a heat going green
		// mid-render) used to leave the model generating to completion — dead
		// weight on the GPU the live calls need, and a flood of unconsumed
		// CHUNK lines ("dropped a reply line — the buffer is full"). STOP
		// breaks the loop, then the tail is drained to STREAMEND so the
		// protocol stays in sync for the next request.
		abort := func() {
			_, _ = fmt.Fprintln(e.stdin, "STOP")
			_, _ = e.waitForPrefix("STREAMEND", 10*time.Second)
		}

		deadline := time.After(e.renderTimeout)
		for {
			select {
			case reply, open := <-e.replies:
				if !open {
					return // the sidecar exited
				}
				switch {
				case strings.HasPrefix(reply, "CHUNK "):
					pcm, err := base64.StdEncoding.DecodeString(strings.TrimSpace(reply[6:]))
					if err != nil || len(pcm) == 0 {
						continue
					}
					select {
					case out <- pcm:
					case <-ctx.Done():
						abort()
						return
					}
				case strings.HasPrefix(reply, "STREAMEND"):
					return
				}
			case <-deadline:
				abort()
				return
			case <-ctx.Done():
				abort()
				return
			}
		}
	}()

	return out, nil
}

// LocalSampleRate is what the model produces.
const LocalSampleRate = 24000

// Provider identifies this engine. It is neither cloud provider — a caller
// choosing voices must not treat it as one and pass a voice name it cannot use.
func (e *LocalEngine) Provider() string { return ProviderLocal }

// ProviderLocal marks speech generated on this machine.
const ProviderLocal = "local"

// SetUsageHook is accepted and ignored: local speech costs nothing to meter.
func (e *LocalEngine) SetUsageHook(f func(kind string, inputTokens, outputTokens int)) {
	e.usage = f
}

// Close stops the sidecar.
func (e *LocalEngine) Close() error {
	if e == nil || e.cmd == nil {
		return nil
	}
	e.ready = false

	e.mu.Lock()
	_, _ = fmt.Fprintln(e.stdin, "QUIT")
	_ = e.stdin.Close()
	e.mu.Unlock()

	done := make(chan struct{})
	go func() { _ = e.cmd.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = e.cmd.Process.Kill()
	}
	return nil
}

func (e *LocalEngine) waitFor(token string, timeout time.Duration) bool {
	_, ok := e.waitForPrefix(token, timeout)
	return ok
}

func (e *LocalEngine) waitForPrefix(prefix string, timeout time.Duration) (string, bool) {
	reply, ok, _ := e.waitForPrefixCtx(context.Background(), prefix, timeout)
	return reply, ok
}

// waitForPrefixCtx is waitForPrefix that can be abandoned mid-wait. The third
// result distinguishes "the caller cancelled" from "the sidecar never
// answered" — the two demand different handling (a cancel must STOP the
// sidecar and re-sync the protocol; a timeout is the sidecar's failure).
func (e *LocalEngine) waitForPrefixCtx(ctx context.Context, prefix string, timeout time.Duration) (string, bool, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case line, open := <-e.replies:
			if !open {
				return "", false, false // the sidecar exited
			}
			if strings.HasPrefix(line, strings.TrimSpace(prefix)) {
				return strings.TrimPrefix(line, prefix), true, false
			}
		case <-deadline:
			return "", false, false
		case <-ctx.Done():
			return "", false, true
		}
	}
}

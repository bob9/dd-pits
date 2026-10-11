package main

// Playing the mixed audio on THIS machine's speakers.
//
// /booth.wav is the right transport for OBS on another box: it behaves like a
// radio station and ffmpeg treats it as one. It is the wrong transport for
// listening here, because ffmpeg fills a network buffer before it plays a
// sample and OBS defaults that to 2 MB. The stream is 48 kB/s, so that buffer
// is roughly forty-five seconds — the calls arrive correct and three-quarters
// of a minute behind the race, which is what "the audio is delayed in OBS"
// actually is. Nothing in dd-pits causes it and nothing in dd-pits can fix it
// while the audio goes over HTTP.
//
// So this bypasses HTTP entirely. The mixer already fans its paced frames out
// to a set of subscribers; this registers as one more and writes the bytes
// straight to a device. Latency is one frame plus the device's own.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// audioSink is something that plays the bus on this machine.
//
// Two implementations, chosen at runtime rather than compiled apart, because the
// choice is not purely about the platform: dd-pits owns the device when the
// DEFAULT output is wanted (exact cancellation, no sidecar), and the Python
// sidecar takes over when a specific device is named — oto cannot enumerate or
// select outputs, and that dropdown exists because a tester's audio was going
// somewhere they could not see or change.
type audioSink interface {
	// Flush drops everything already committed, so a cut is heard now.
	Flush()
	Close() error
}

// localPlayer plays the mixer's output on a local device.
type localPlayer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	frames chan []byte
}

// Flush discards whatever the audio device has buffered.
//
// Called when the bus is cleared, so a stop is heard immediately rather than
// after the device has played out what it already holds.
func (p *localPlayer) Flush() {
	if p == nil {
		return
	}
	// Drop what WE are still holding, before telling the device to drop what it
	// holds.
	//
	// Done BEFORE the sidecar check, not after: the frames are ours either way,
	// and a cut that silently kept them because there was no process to signal
	// would leave stale audio to play the moment one appeared.
	//
	// A cut has to reach four places: the mixer queue, this channel, the OS pipe,
	// and the device buffer. Only the first and last were being cleared, so the
	// frames sitting here were written to the sidecar immediately AFTER the
	// abort — stale audio arriving just in time to be played. Heard as
	// commentary continuing past the Start button.
	//
	// These are the ones we can genuinely take back: the pipe and the device are
	// another process's memory, and a byte stream carries no notion of which cue
	// a sample belonged to. Draining here is what makes the abort mean something.
	drained := 0
	for {
		select {
		case <-p.frames:
			drained++
			continue
		default:
		}
		break
	}
	if p.cmd != nil {
		flushDevice(p.cmd.Process)
	}
	if drained > 0 {
		log.Printf("audio out: dropped %d frame(s) still in hand (%dms) on the cut",
			drained, drained*boothFrameMs)
	}
}

// startLocalPlayer subscribes to the mixer and plays what it produces.
//
// Best-effort: a venue with no working output device still gets everything over
// /booth.wav, and losing local monitoring must never cost the calls themselves.
func startLocalPlayer(ctx context.Context, m *boothMixer, python, script, device, latency string) (*localPlayer, error) {
	if m == nil {
		return nil, fmt.Errorf("no audio bus to play")
	}
	if python == "" || script == "" {
		return nil, fmt.Errorf("local playback needs a python and a player script")
	}

	args := []string{script, "--rate", fmt.Sprintf("%d", boothSampleRate)}
	if latency != "" {
		args = append(args, "--latency", latency)
	}
	if device != "" {
		args = append(args, "--device", device)
	}

	cmd := exec.Command(python, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("player stdin: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("player stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start player: %w", err)
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "#")); line != "" {
				log.Printf("audio out: %s", line)
			}
		}
	}()

	p := &localPlayer{cmd: cmd, stdin: stdin, frames: make(chan []byte, 2)}

	// The bus can now throw away what the device has buffered when it clears.
	m.setFlush(p.Flush)

	m.mu.Lock()
	m.clients[p.frames] = struct{}{}
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.clients, p.frames)
			m.mu.Unlock()
			_ = stdin.Close()
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-p.frames:
				if _, err := stdin.Write(frame); err != nil {
					// The device went away — a headset unpaired, say. Stop
					// playing rather than spin: /booth.wav is unaffected, so
					// the meeting keeps its audio path.
					log.Printf("audio out: stopped (%v)", err)
					return
				}
			}
		}
	}()

	return p, nil
}

// Close stops playback.
func (p *localPlayer) Close() error {
	if p == nil || p.cmd == nil {
		return nil
	}
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return nil
}

// defaultLocalPlayerScript looks for the player beside the agent binary, so a
// normal install needs no configuration.
func defaultLocalPlayerScript() string {
	return sidecarPath("local_audio_out.py")
}

// speakerOutputReady reports whether playing through this machine's speakers
// can actually work, and if not, what is missing.
//
// This exists because the failure was invisible. Playing locally runs a small
// Python sidecar (PortAudio via sounddevice); someone who chose CLOUD text-to-
// speech has no reason to have installed any of that, so ticking "play through
// this laptop's speakers" wrote the setting, logged one line nobody reads, and
// produced silence. The only symptom was "commentary has no audio" — while the
// voice test, which plays in the BROWSER, worked perfectly and pointed the
// blame in entirely the wrong direction.
//
// Answered on the settings page instead, next to the tickbox it governs.
func speakerOutputReady(python string) (bool, string) {
	if strings.TrimSpace(python) == "" {
		python = lookupPython()
	}
	if python == "" {
		return false, "Python 3 was not found on this machine."
	}
	// Presence of the interpreter is not enough — sounddevice is a separate
	// install and is the part that is usually missing.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, python, "-c", "import sounddevice").Run(); err != nil {
		return false, "Python 3 is installed, but the sounddevice package it needs is not " +
			"(pip3 install sounddevice)."
	}
	return true, ""
}

// lookupPython finds a Python 3 without one having been configured.
//
// The speaker path is unrelated to which VOICE engine is in use, so requiring
// -local-voice-python to hear audio locally conflated two separate things.
func lookupPython() string {
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// ── choosing an output, and changing it without a restart ────────────────────

// audioDevice is one output the machine can play through.
type audioDevice struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
}

// listAudioOutputs asks the sidecar what outputs exist.
//
// A dropdown of the real devices, rather than a box to type a name fragment
// into: from memory that is a guessing game, and a typo looks exactly like a
// device that is not working.
func listAudioOutputs(python string) ([]audioDevice, string, error) {
	if strings.TrimSpace(python) == "" {
		python = lookupPython()
	}
	script := defaultLocalPlayerScript()
	if python == "" || script == "" {
		return nil, "", fmt.Errorf("no Python 3 for the audio sidecar")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, python, script, "--list-devices").Output()
	if err != nil {
		return nil, "", err
	}
	var res struct {
		Devices []audioDevice `json:"devices"`
		Default string        `json:"default"`
		Error   string        `json:"error"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, "", fmt.Errorf("could not read the device list: %w", err)
	}
	if res.Error != "" {
		return nil, "", fmt.Errorf("%s", res.Error)
	}
	return res.Devices, res.Default, nil
}

// audioOutController owns local speaker playback so it can be turned on, off,
// or pointed at a different device WHILE dd-pits is running.
//
// It used to be decided once at startup, which made the settings page lie: the
// tickbox and the device box saved happily and changed nothing until a restart,
// with nothing on screen saying so. On a race day nobody wants to restart the
// timing agent to try a different speaker.
type audioOutController struct {
	mu     sync.Mutex
	ctx    context.Context
	mixer  *boothMixer
	python string

	cur     audioSink
	device  string
	latency string
}

func newAudioOutController(ctx context.Context, m *boothMixer, python string) *audioOutController {
	return &audioOutController{ctx: ctx, mixer: m, python: python}
}

// Apply makes local playback match what the settings now say.
//
// Idempotent: called with the settings already in force it does nothing, so a
// save that did not touch the audio section never interrupts a call mid-word.
func (c *audioOutController) Apply(enabled bool, device, latency string) error {
	if c == nil {
		return fmt.Errorf("local playback is not available in this build")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case !enabled:
		c.stopLocked()
		return nil
	case c.cur != nil && c.device == device && c.latency == latency:
		return nil // already exactly this
	}

	// A device change means tearing the old one down first: two streams on one
	// output is at best doubled audio.
	c.stopLocked()

	if c.mixer == nil {
		return fmt.Errorf("no audio bus — commentary or race calls have to be on first")
	}
	// dd-pits owns the device when the DEFAULT output is wanted.
	//
	// This is the cancellation fix: in-process, a cut clears the device buffer
	// directly instead of signalling another process and hoping. It is skipped
	// the moment a specific device is named, because oto cannot select one — and
	// losing that dropdown would cost more than exact cancellation gains, since
	// audio going to the wrong output is invisible without it.
	if device == "" && goPlayerAvailable() {
		if p, err := startGoPlayer(c.ctx, c.mixer); err == nil {
			c.cur, c.device, c.latency = p, device, latency
			return nil
		} else {
			// Falls through to the sidecar rather than failing: a machine that
			// cannot open its default output in-process may still have a working
			// PortAudio, and silence is the worst outcome here.
			log.Printf("audio out: in-process playback unavailable (%v) — using the sidecar", err)
		}
	}

	python := c.python
	if python == "" {
		python = lookupPython()
	}
	script := defaultLocalPlayerScript()
	if python == "" || script == "" {
		return fmt.Errorf("no Python 3 found for the audio sidecar — /booth.wav still works")
	}
	// Checked BEFORE claiming success, because starting the sidecar is not the
	// same as it working: the process launches fine and then dies on its own
	// import, asynchronously, so startLocalPlayer returns nil and the settings
	// page cheerfully reported "playing through the speakers now" while the log
	// recorded ModuleNotFoundError. That false success is the whole reason this
	// was hard to diagnose in the first place, so it must not be reintroduced
	// by the fix for it.
	if ok, why := speakerOutputReady(python); !ok {
		return fmt.Errorf("%s /booth.wav still works", why)
	}
	p, err := startLocalPlayer(c.ctx, c.mixer, python, script, device, latency)
	if err != nil {
		return err
	}
	c.cur, c.device, c.latency = p, device, latency
	return nil
}

// Playing reports whether audio is going to this machine's speakers now.
func (c *audioOutController) Playing() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur != nil
}

func (c *audioOutController) stopLocked() {
	if c.cur != nil {
		_ = c.cur.Close()
		c.cur = nil
	}
}

// logAudioOutputs prints the machine's output devices at startup.
//
// Always, whether or not local playback is switched on. Two reasons: it proves
// the audio stack works at all before anyone tries to use it, and it means a log
// sent in from a venue already answers "what can that machine play through?"
// without a round trip asking.
//
// On its own goroutine because it shells out to the sidecar, and startup must
// not wait on it.
func logAudioOutputs(python string) {
	devs, def, err := listAudioOutputs(python)
	if err != nil {
		log.Printf("audio devices: cannot list them (%v) — the 'play through this laptop's "+
			"speakers' option is unavailable on this machine; /booth.wav still works", err)
		return
	}
	if len(devs) == 0 {
		log.Printf("audio devices: none found — /booth.wav still works")
		return
	}
	log.Printf("audio devices: %d output(s) available", len(devs))
	for _, d := range devs {
		mark := ""
		if d.Name == def {
			mark = "  <- system default"
		}
		log.Printf("audio devices:   [%d] %s%s", d.Index, d.Name, mark)
	}
}

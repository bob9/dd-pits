// dd-pits is a venue-side race-day companion for FPVTrackside. It watches a
// local FPVTrackside timing computer and serves a live dashboard, race calls,
// AI commentary, ELRS backpack OSD and broadcast overlays — all from the pit
// laptop, with no internet connection or cloud account required.
//
// Usage:
//
//	dd-pits                                   # serve on :8088, set up in a browser
//	dd-pits -fpvtrackside http://localhost:8080 -serve-addr :8088
//
// Most flags can also be supplied via env vars (DD_PITS_FPVTRACKSIDE,
// DD_PITS_SERVE_ADDR, …) so the agent can be wrapped in a systemd / launchd
// service. Run with -h for the full list.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/elrs"
	"github.com/bob9/dd-pits/internal/gemini"
	"github.com/bob9/dd-pits/internal/localchat"
	"github.com/bob9/dd-pits/internal/qwenchat"
	"github.com/bob9/dd-pits/internal/qwentts"
	"github.com/bob9/dd-pits/internal/tts"
)

// version and commit are stamped at build time via
// -ldflags "-X main.version=... -X main.commit=...". The CI/CD pipeline
// generates a date+run-number version and embeds the git short SHA; local
// builds fall back to the git short sha / "unknown" (see the Makefile). Both
// are printed on startup and via -version.
var (
	version = "dev"
	commit  = "unknown"
)

// supportURL is the developer's Buy Me a Coffee page, linked from the
// dashboard, the setup pages and the startup log.
const supportURL = "https://buymeacoffee.com/bobnine"

// timingSystem names the timer dd-pits reads, for logs and the pages that tell
// a race director what to switch on: "FPVTrackside" unless -rotorhazard is set.
var timingSystem = "FPVTrackside"

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// agent holds per-process state across ticks: the last snapshot we built (so
// FetchSnapshotIncremental can reuse ended races) and the last hash we
// posted (so we can skip duplicate POSTs).
type agent struct {
	// lastSelectedRace tracks FPVTrackside's selected race between ticks — a
	// CHANGE is the operator advancing, which retires the previous heat's
	// post-race audio (postskip.go).
	lastSelectedRace string

	ft *dronedash.Client
	// rh is set when the timing comes from RotorHazard (-rotorhazard) instead
	// of FPVTrackside; rhVersion is the plugin's change token last fetched, so
	// an unchanged timer costs one tiny request per tick.
	rh         *dronedash.RHClient
	rhVersion  string
	httpClient *http.Client

	idleInterval   time.Duration // tick cadence when no race is active
	activeInterval time.Duration // tick cadence when a race is in-flight

	// elrsMu guards a.elrs, which can now be created at RUNTIME when the setup
	// page enables the backpack — the poll loop reads it concurrently.
	elrsMu sync.Mutex

	// commentary holds the generated per-heat commentary for this meeting.
	commentary *commentaryStore

	// commentaryGen serialises generation: one heat at a time, so a day's
	// heats are worked through in order rather than all at once — which would
	// exceed every rate limit and starve the live booth of the GPU.
	commentaryGen sync.Mutex

	// localCfg is the venue's own settings file (no-account mode). Always
	// non-nil once -serve has run, so callers need not guard it.
	localCfg *localConfig

	// announcer is assigned from a goroutine (the model takes minutes to load)
	// and read by the poll loop, so it is guarded rather than a plain field.
	announcerMu sync.Mutex
	announcer   *raceAnnouncer

	prev            dronedash.Snapshot
	lastReconcileAt time.Time // last time we forced a full (non-incremental) re-read
	lastSelected    string    // operator's selected race (/current) last seen; a change forces a fresh re-read
	cuedRace        string    // last UN-RUN cued heat; persisted across an "earlier review" (see persistCue)

	// fetchFailures counts consecutive FPVTrackside read failures, so a flaky
	// link reads as self-healing in the logs rather than a silent stall.
	fetchFailures int

	local *liveServer // local dashboard server (nil = -serve off)

	// Optional ELRS backpack integration (-elrs): OSD messages to pilots'
	// goggles driven from snapshot diffs and the setup pages. nil when disabled — behaviour is then unchanged.
	elrs *elrsController

	// Optional live commentary booth (-booth): Gemini-written two-person race
	// calls streamed as audio for OBS. nil when disabled.
	booth *booth

	// mixer is the audio bus everything speaks through.
	//
	// Held on the agent rather than on the booth because it is not a commentary
	// feature: the race calls need it too, and a venue that wants FPVTrackside's
	// calls in one voice should not be made to configure an AI commentator (and
	// find an API key for it) to get them.
	mixer *boothMixer
}

// nextInterval picks the tick cadence for the upcoming wait. We look at the
// most recently fetched snapshot: if any race has Start but no End,
// FPVTrackside reckons it in-flight, so we tick fast to surface laps within
// activeInterval. Otherwise we coast at idleInterval to keep CPU and FT load
// minimal.
func (a *agent) nextInterval() time.Duration {
	// Fast cadence while a race runs — and while one is cued and unstarted,
	// so the pending→running edge (and the GO! message riding on it) is seen
	// within one fast tick instead of one idle tick.
	if a.prev.HasRunningRace() || a.prev.HasCuedPendingRace() {
		return a.activeInterval
	}
	return a.idleInterval
}

func main() {
	// Capture the log in memory as well as printing it, so the debug page can
	// show it. Tee'd rather than replaced: the Terminal window keeps working
	// exactly as before. Installed first so the startup lines — the ones most
	// worth reading and the first lost to scrollback — are in the buffer too.
	logRingBuf := newLogRing()
	log.SetOutput(io.MultiWriter(os.Stderr, logRingBuf))

	ftURL := flag.String("fpvtrackside", envOr("DD_PITS_FPVTRACKSIDE", "http://localhost:8080"),
		"base URL of the local FPVTrackside HTTP server")
	rhURL := flag.String("rotorhazard", envOr("DD_PITS_ROTORHAZARD", ""),
		"read timing from a RotorHazard timer instead of FPVTrackside (e.g. http://localhost:5000); "+
			"needs the dd-pits RotorHazard plugin, which normally starts dd-pits with this set")
	intervalStr := flag.String("interval", envOr("DD_PITS_INTERVAL", "250ms"),
		"poll cadence when no race is in-flight (Go duration) — bounds how fast a heat start / holeshot is picked up")
	activeIntervalStr := flag.String("active-interval", envOr("DD_PITS_ACTIVE_INTERVAL", "100ms"),
		"poll cadence while a race is in-flight — bounds lap-time freshness on the board")
	once := flag.Bool("once", false, "read a single snapshot and exit (for testing)")
	serve := flag.Bool("serve", envOr("DD_PITS_SERVE", "1") != "0" && envOr("DD_PITS_SERVE", "1") != "false",
		"run the local web server on this machine (default on; -serve=false for a headless poller)")
	serveAddr := flag.String("serve-addr", envOr("DD_PITS_SERVE_ADDR", ":8088"),
		"address for the local DroneDash web server when -serve is set")
	uiTickStr := flag.String("ui-tick", envOr("DD_PITS_UI_TICK", "0"),
		"minimum gap between live-update pushes to connected browsers (0 = push every change, the default). "+
			"Raise it only if the machine SERVING the board is struggling — a single slow display should use ?lite=1 instead")
	elrsEnabled := flag.Bool("elrs", envOr("DD_PITS_ELRS", "") == "1" || envOr("DD_PITS_ELRS", "") == "true",
		"enable ELRS backpack integration: OSD messages to pilots' goggles")
	elrsSerial := flag.String("elrs-serial", envOr("DD_PITS_ELRS_SERIAL", ""),
		"ELRS backpack serial port (460800 8N1); \"auto\" scans all ports; empty = use -elrs-netpack")
	elrsNetpack := flag.String("elrs-netpack", envOr("DD_PITS_ELRS_NETPACK", "elrs-netpack.local"),
		"ELRS netpack hostname (TCP :8080)")
	localConfigPath := flag.String("local-config", envOr("DD_PITS_LOCAL_CONFIG", ""),
		"settings file (default: dd-pits-local.json beside the agent)")
	setupPINFlag := flag.String("setup-pin", envOr("DD_PITS_SETUP_PIN", ""),
		"allow changing settings from another machine on the venue network with ?pin=... (default: this machine only)")
	boothEnabled := flag.Bool("booth", envOr("DD_PITS_BOOTH", "") == "1" || envOr("DD_PITS_BOOTH", "") == "true",
		"live AI commentary booth: generates two-person race commentary from the timing data and serves it as a continuous audio stream at /booth.wav for OBS (requires -serve and Gemini credentials)")
	geminiKey := flag.String("gemini-key", envOr("DD_PITS_GEMINI_KEY", ""),
		"Google AI Studio API key for the commentary booth (or use -gemini-project for Vertex AI)")
	geminiProject := flag.String("gemini-project", envOr("DD_PITS_GEMINI_PROJECT", ""),
		"GCP project id for Vertex AI Gemini calls (booth); uses -gemini-credentials or Application Default Credentials")
	geminiLocation := flag.String("gemini-location", envOr("DD_PITS_GEMINI_LOCATION", "global"),
		"Vertex AI location for booth Gemini calls")
	geminiCreds := flag.String("gemini-credentials", envOr("DD_PITS_GEMINI_CREDENTIALS", ""),
		"path to a GCP service-account JSON for Vertex AI booth calls (empty = Application Default Credentials)")
	boothModel := flag.String("booth-model", envOr("DD_PITS_BOOTH_MODEL", "gemini-2.5-flash"),
		"Gemini model used to write the booth's dialogue lines")
	obsURL := flag.String("obs-url", envOr("DD_PITS_OBS_URL", ""),
		"obs-websocket URL (e.g. ws://localhost:4455) — lets the booth AUTO-delay the OBS video so commentary stays in sync; needs -obs-source")
	obsPassword := flag.String("obs-password", envOr("DD_PITS_OBS_PASSWORD", ""),
		"obs-websocket password (OBS → Tools → WebSocket Server Settings)")
	obsSource := flag.String("obs-source", envOr("DD_PITS_OBS_SOURCE", ""),
		"OBS video source(s)/scene(s) to delay, comma-separated; empty = EVERY scene (recommended)")
	obsInjectAudio := flag.Bool("obs-inject-audio", envOr("DD_PITS_OBS_INJECT_AUDIO", "1") != "0",
		"add the booth /booth.wav audio to every OBS scene automatically, so commentary is heard on any scene")
	raceAudio := flag.Bool("race-audio", envOr("DD_PITS_RACE_AUDIO", "") == "1",
		"speak FPVTrackside's race calls from this agent (mute FPVTrackside's own audio). "+
			"Reads its Sounds.xml so the sounds stay configured where they always were.")
	raceAudioProfile := flag.String("race-audio-profile", envOr("DD_PITS_RACE_AUDIO_PROFILE", "Profile 1"),
		"FPVTrackside profile whose Sounds.xml to read")
	localVoicePython := flag.String("local-voice-python", envOr("DD_PITS_LOCAL_VOICE_PYTHON", ""),
		"python with mlx-audio installed, for speaking locally with no network")
	localVoiceScript := flag.String("local-voice-script", envOr("DD_PITS_LOCAL_VOICE_SCRIPT", ""),
		"the local speech sidecar script (defaults to local_tts_server.py beside the agent)")
	localVoiceModel := flag.String("local-voice-model", envOr("DD_PITS_LOCAL_VOICE_MODEL", ""),
		"local TTS model")
	localVoiceRef := flag.String("local-voice-ref", envOr("DD_PITS_LOCAL_VOICE_REF", ""),
		"reference wav to clone the voice from")
	localVoiceRefText := flag.String("local-voice-ref-text", envOr("DD_PITS_LOCAL_VOICE_REF_TEXT", ""),
		"exactly what is said in the reference wav (required with it)")
	qwenKey := flag.String("qwen-key", envOr("QWEN_API_KEY", ""),
		"render race calls with REMOTE Qwen3-TTS instead of a local model. Frees the GPU entirely "+
			"(the commentary model then has the machine to itself) at the cost of needing the network: "+
			"measured ~320ms to first audio warm, against ~110ms local.")
	qwenVoice := flag.String("qwen-voice", envOr("DD_PITS_QWEN_VOICE", ""),
		"voice for -qwen-key: a prebuilt name (Ethan, Cherry, …) or a cloned voice id (qwen-tts-vc-…)")
	qwenBase := flag.String("qwen-base-url", envOr("QWEN_TTS_BASE_URL", ""),
		"Qwen region override; default is the international endpoint (measured 110ms RTT vs 316ms for Beijing)")
	modelHost := flag.String("model-host", envOr("DD_PITS_MODEL_HOST", ""),
		"run as a MODEL SERVER for other machines (e.g. \":9099\"): loads the local voice and "+
			"writer and answers over the LAN. For a Windows race PC, which cannot run MLX — put a "+
			"Mac on the same network and point -model-url at it.")
	modelURL := flag.String("model-url", envOr("DD_PITS_MODEL_URL", ""),
		"use a model host on another machine for the voice and commentary "+
			"(e.g. http://macbook.local:9099). A LAN hop is under a millisecond, so this costs "+
			"almost nothing next to the model's own time.")
	qwenScriptModel := flag.String("qwen-script-model", envOr("DD_PITS_QWEN_SCRIPT_MODEL", ""),
		"write commentary with a REMOTE Qwen chat model (needs -qwen-key). Use instead of "+
			"-local-script-model to leave the GPU free. \"default\" picks "+qwenchat.DefaultModel+
			", chosen for latency: it does no thinking, which is where the seconds go.")
	audioOut := flag.Bool("audio-out", envOr("DD_PITS_AUDIO_OUT", "") == "1",
		"play the mixed audio on THIS machine's speakers as well as serving /booth.wav. "+
			"Use it for local monitoring: OBS buffers a network stream by megabytes, which is "+
			"~45s at this bitrate, so /booth.wav is for a remote OBS and this is for the room.")
	audioOutLatency := flag.String("audio-out-latency", envOr("DD_PITS_AUDIO_OUT_LATENCY", ""),
		"output buffer: seconds (e.g. 0.15) or 'low'. Bluetooth needs the larger buffer to avoid "+
			"crackle; a wired or built-in output can use 'low' and save ~750ms before the first sample.")
	audioOutDevice := flag.String("audio-out-device", envOr("DD_PITS_AUDIO_OUT_DEVICE", ""),
		"output device name or index for -audio-out (default: the system default)")
	boothYield := flag.Bool("booth-yield-while-racing", envOr("DD_PITS_BOOTH_YIELD", "1") != "0",
		"with -local-script-model: hold commentary while a heat is RUNNING so the race calls "+
			"get the GPU (they share one). Colour still runs between heats. Off = commentary "+
			"mid-heat, at the cost of late lap calls.")
	localScriptModel := flag.String("local-script-model", envOr("DD_PITS_LOCAL_SCRIPT_MODEL", ""),
		"write the booth's commentary with a LOCAL chat model instead of Gemini, so the whole "+
			"booth runs with no network (e.g. "+localchat.DefaultModel+"). Needs -local-voice-python.")

	boothVideoDelay := flag.Duration("booth-video-delay", 0,
		"fixed broadcast buffer to hold booth audio to when you set the OBS video delay BY HAND (ignored when -obs-url is set)")
	boothMinDelay := flag.Duration("booth-min-delay", 0,
		"auto mode (-obs-url) only: minimum broadcast buffer to start from and never dip below (e.g. 40s) — generous headroom so early calls are never late; the buffer only grows from here")
	demo := flag.Bool("demo", envOr("DD_PITS_DEMO", "") == "1",
		"read-only public demo: every page renders, but nothing can be changed, saved, installed or restarted")
	showVersion := flag.Bool("version", false, "print the agent version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("dd-pits %s (%s)\n", version, commit)
		return
	}
	log.Printf("dd-pits %s (%s) starting", version, commit)
	demoMode = *demo
	if demoMode {
		log.Printf("dd-pits: DEMO mode — read-only, nothing can be changed")
	}
	log.Printf("dd-pits is free and open source — if it helps your race days, support it at %s", supportURL)

	// Model-server mode: this process lends its GPU to another machine and runs
	// no agent at all, so it must be handled before anything else starts.
	if *modelHost != "" {
		hctx, hcancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer hcancel()
		if err := serveModelHost(hctx, *modelHost, tts.LocalConfig{
			Python:         *localVoicePython,
			Script:         *localVoiceScript,
			Model:          *localVoiceModel,
			ReferenceAudio: *localVoiceRef,
			ReferenceText:  *localVoiceRefText,
		}, *localScriptModel); err != nil && err != http.ErrServerClosed {
			log.Fatalf("dd-pits: model host: %v", err)
		}
		return
	}

	interval, err := time.ParseDuration(*intervalStr)
	if err != nil || interval < 100*time.Millisecond {
		log.Fatalf("dd-pits: invalid -interval %q (min 100ms): %v", *intervalStr, err)
	}
	activeInterval, err := time.ParseDuration(*activeIntervalStr)
	if err != nil || activeInterval < 100*time.Millisecond {
		log.Fatalf("dd-pits: invalid -active-interval %q (min 100ms): %v", *activeIntervalStr, err)
	}
	if activeInterval > interval {
		// Allow this but warn — it's almost always a configuration mistake.
		log.Printf("dd-pits: warning: -active-interval (%s) > -interval (%s); active polls will be slower than idle", activeInterval, interval)
	}

	ftClient, err := dronedash.NewClient(*ftURL)
	if err != nil {
		log.Fatalf("dd-pits: bad -fpvtrackside URL: %v", err)
	}

	var rhClient *dronedash.RHClient
	if *rhURL != "" {
		if rhClient, err = dronedash.NewRHClient(*rhURL); err != nil {
			log.Fatalf("dd-pits: bad -rotorhazard URL: %v", err)
		}
		timingSystem = "RotorHazard"
		log.Printf("dd-pits: reading timing from RotorHazard at %s", *rhURL)
	}

	a := &agent{
		ft:             ftClient,
		rh:             rhClient,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		idleInterval:   interval,
		activeInterval: activeInterval,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	defer cancel()

	// applyLocalBooth carries the settings file's booth config forward to be
	// applied once the booth is constructed, further down. Declared out here
	// because the two live in sibling blocks — which is precisely how the old
	// `if a.booth != nil` guard came to sit where the booth did not yet exist.
	var applyLocalBooth func()

	// The local dashboard: serves the live surfaces from local FPVTrackside
	// data, fully offline.
	if *serve {
		a.local = newLiveServer(defaultBrand())
		if d, err := time.ParseDuration(*uiTickStr); err != nil {
			log.Fatalf("dd-pits: invalid -ui-tick %q: %v", *uiTickStr, err)
		} else if d < 0 {
			log.Fatalf("dd-pits: invalid -ui-tick %q: must not be negative", *uiTickStr)
		} else {
			a.local.uiTick = d
		}
		// The controls page needs the announcer, which comes up later on its
		// own goroutine — hence a lookup rather than a value.
		a.local.announcerFn = a.raceAnnouncer

		// The venue's own settings, for a club with no platform account.
		//
		// Loaded unconditionally: the setup pages then SHOW the
		// configuration read-only rather than 404ing, which is friendlier than
		// pretending the feature does not exist. A malformed file is reported
		// and treated as empty — a bad JSON edit must not stop a race day.
		setupPIN = *setupPINFlag
		setupConfigPath = *localConfigPath // so local models install beside it
		// The voice sidecar is bundled, so nobody should have to name it.
		//
		// Defaulted once, here, because it is passed to several places and only
		// one of them was filling it in — so a venue that chose the local voice
		// through the settings page got a model installed and no interpreter
		// script to run it with.
		if *localVoiceScript == "" {
			*localVoiceScript = defaultLocalVoiceScript()
		}
		lc, lerr := loadLocalConfig(*localConfigPath)
		if lerr != nil {
			log.Printf("local setup: %v — starting with no local settings", lerr)
		}

		// The settings file supplies what the website would, but ONLY when
		// there is no website and no flag already said otherwise. A flag is an
		// explicit instruction from whoever started the process, so it wins;
		// the file is the fallback for a venue that never types flags.
		// Pronunciations apply in EVERY mode — they rewrite text at the TTS
		// boundary.
		setPronunciations(lc.BoothConfig().Pronounce)

		bc := lc.BoothConfig()

		// The WRITER and the local voice paths, which are flag-only
		// otherwise. A club configuring through the pages must not have to
		// discover flag names to finish the job.
		if bc.ScriptModel != "" && *localScriptModel == "" {
			*localScriptModel = bc.ScriptModel
		}
		if bc.GeminiKey != "" && *geminiKey == "" {
			*geminiKey = bc.GeminiKey
		}
		if bc.VoicePython != "" && *localVoicePython == "" {
			*localVoicePython = bc.VoicePython
		}
		if bc.VoiceRef != "" && *localVoiceRef == "" {
			*localVoiceRef = bc.VoiceRef
		}
		if bc.VoiceRefText != "" && *localVoiceRefText == "" {
			*localVoiceRefText = bc.VoiceRefText
		}
		// A configured voice AND writer means the booth is wanted; the flag
		// is then just a way of saying the same thing.
		if bc.Engine != "" && (*localScriptModel != "" || *geminiKey != "" || *qwenScriptModel != "") {
			*boothEnabled = true
		}

		// Race calls are wanted unless the venue said otherwise. Without
		// this, a club that configured a voice through the pages would
		// still get silence until they found the -race-audio flag.
		if !*raceAudio && !strings.EqualFold(lc.CallsConfig().Source, "trackside") {
			*raceAudio = true
		}

		// Speakers.
		au := lc.AudioSettings()
		if au.Enabled && !*audioOut {
			*audioOut = true
		}
		if au.Device != "" && *audioOutDevice == "" {
			*audioOutDevice = au.Device
		}
		if au.Latency != "" && *audioOutLatency == "" {
			*audioOutLatency = au.Latency
		}

		// The venue's commentary STYLE. Local-only has no website to fetch
		// it from, so without this the personality, humour and language
		// pickers save to a file nothing ever reads.
		// Remembered, and applied once the booth EXISTS.
		//
		// This used to run here as `if a.booth != nil { … }` — and the booth
		// is not built until much further down, so the condition was never
		// true and the entire block was dead. Every setting on the
		// Commentary page that is not a flag went into a file nothing read:
		// language, accent, theme, humour, energy, voice characteristics,
		// solo/duo, both voice names, and the phase and per-beat toggles.
		//
		// Reported as "even after selecting Spanish I still hear English".
		// The voice test spoke Spanish because it builds its own Style
		// straight from the config, which is what made this look like a
		// language problem rather than an everything problem.
		if bc.Engine != "" {
			applyLocalBooth = func() {
				// Re-read, do not reuse the startup value: this same
				// closure is called again whenever the settings page saves,
				// and applying the config as it was at launch would make a
				// save appear to do nothing — the fault this whole area
				// already had once.
				bc := lc.BoothConfig()
				a.booth.mu.Lock()
				a.booth.style = boothpkg.Style{
					Theme:    bc.Theme,
					Humour:   bc.Humour,
					Language: bc.Language,
					Accent:   boothpkg.SanitiseAccent(bc.Accent),
					Energy:   bc.Energy,
				}
				a.booth.voiceStyle = bc.Style

				// The phase and per-beat toggles. Absent means ON, so a venue
				// that never opened the page keeps the whole broadcast.
				a.booth.phasePre = phaseOn(bc.PhasePre)
				a.booth.phaseIn = phaseOn(bc.PhaseIn)
				a.booth.phasePost = phaseOn(bc.PhasePost)
				// A venue that has never chosen gets the ECONOMICAL set —
				// build-up and result only. Everything-on was costing real
				// money for chatter between heats that nobody asked for.
				if bc.Beats != nil {
					a.booth.beatCfg = bc.Beats
				} else {
					a.booth.beatCfg = boothEconomyBeats()
				}
				if bc.Mode != "" {
					a.booth.voiceMode = bc.Mode
				}
				if bc.VoicePBP != "" {
					a.booth.voicePBP = bc.VoicePBP
				}
				if bc.VoiceColor != "" {
					a.booth.voiceColor = bc.VoiceColor
				}
				a.booth.mu.Unlock()
			}
		}

		switch bc.Engine {
		case "qwen":
			if *qwenKey == "" {
				*qwenKey = bc.QwenKey
			}

			// One key is the whole configuration.
			//
			// Qwen serves both halves of the booth: the voice (qwentts) and
			// the WRITER (qwenchat, an OpenAI-shaped chat endpoint on the
			// same account). Requiring a second model or a Gemini key on top
			// would be asking for something the operator already has, and
			// leaving the writer unset is exactly what makes the booth
			// refuse to start with no obvious reason.
			if *qwenKey != "" {
				if *qwenScriptModel == "" && *localScriptModel == "" && *geminiKey == "" {
					*qwenScriptModel = "default"
				}
				if *qwenVoice == "" {
					*qwenVoice = qwentts.DefaultVoicePBP
				}
				*boothEnabled = true
			}
			if *qwenBase == "" && bc.QwenBase != "" {
				*qwenBase = bc.QwenBase
			}
			if bc.VoicePBP != "" && *qwenVoice == "" {
				*qwenVoice = bc.VoicePBP
			}
			log.Printf("local setup: commentary voice = Qwen3-TTS (over the internet)")
		case "local":
			log.Printf("local setup: commentary voice = local model on this machine")
		default:
			log.Printf("local setup: commentary is off (set a voice at /setup/booth)")
		}

		// Whether anything can speak — computed AFTER the engine switch,
		// which is where the Qwen key is resolved from the settings file.
		//
		// Two ordering mistakes made this worth spelling out. It was first
		// computed inside startRaceAudio, which only runs when the calls
		// belong to dd-pits, so choosing dd-pits reported "no voice" and
		// the page reset the choice back to FPVTrackside. Moving it here
		// but ABOVE this switch had the same effect for a different reason:
		// the key had not been read yet. A setting that silently un-chooses
		// itself is the worst kind, so it is worth being precise about when
		// this is known.
		if a.local != nil {
			a.local.hasVoice = *localVoicePython != "" || *qwenKey != "" || *modelURL != ""
		}
		a.localCfg = lc
		a.local.cfg = lc
		a.local.logs = logRingBuf
		// Keeps FPVTrackside's speech volume matching the who-speaks switch,
		// writing whenever FPVTrackside is closed. Makes "restart FPVTrackside"
		// the complete instruction in every ordering — see ftVolumeReconcileLoop.
		go ftVolumeReconcileLoop(ctx, lc, a.local.hasVoice)
		a.local.tracksideURL = *ftURL
		a.commentary = newCommentaryStore(commentaryDir(lc.Path()))
		a.local.commentary = a.commentary
		a.local.youtubeKey = envOr("YOUTUBE_API_KEY", "")

		// One heat at a time: a day's heats are worked through in order rather
		// than all at once, which would exceed every rate limit and starve the
		// live booth of the GPU.
		a.local.generateCommentary = func(raceID string) {
			go func() {
				a.commentaryGen.Lock()
				defer a.commentaryGen.Unlock()
				a.generateRaceCommentary(raceID)
			}()
		}
		setSetupBase(*serveAddr)
		logSetupReady()
		offerSetupPages(lc.Path(), setupBaseURL)
	}

	if *raceAudio && a.local == nil {
		log.Printf("dd-pits: -race-audio needs -serve (the audio bus lives on the local server) — not speaking race calls")
	}

	// Live commentary booth. Switched on by -booth, or by configuring a voice
	// and writer at /setup/booth.
	if a.local != nil && *boothEnabled {
		var gem *gemini.Client
		if *geminiKey != "" || *geminiProject != "" {
			g, gerr := gemini.New(gemini.Config{
				APIKey:          *geminiKey,
				ProjectID:       *geminiProject,
				Location:        *geminiLocation,
				CredentialsFile: *geminiCreds,
				Model:           *boothModel,
			})
			if gerr != nil {
				log.Fatalf("dd-pits: booth gemini client: %v", gerr)
			}
			gem = g
		} else if *localScriptModel == "" && *qwenScriptModel == "" {
			// Qwen counts as a writer as well as a voice — one key covers both,
			// which is the whole point of choosing it. Leaving it out of this
			// check refused a venue that had already configured everything it
			// needed.
			log.Fatalf("dd-pits: -booth requires -gemini-key, " +
				"-gemini-project, or -local-script-model to write locally")
		}
		a.booth = newBooth(gem)
		if applyLocalBooth != nil {
			applyLocalBooth() // the settings file's booth config, now that there is a booth
			a.booth.mu.Lock()
			var on []string
			for _, k := range boothAllBeatKinds() {
				if v, ok := a.booth.beatCfg[k]; ok && v {
					on = append(on, k)
				}
			}
			pre, in, post := a.booth.phasePre, a.booth.phaseIn, a.booth.phasePost
			a.booth.mu.Unlock()
			log.Printf("booth: phases pre=%v during=%v post=%v; talking about: %s",
				pre, in, post, strings.Join(on, ", "))
			if a.local != nil {
				a.local.applyBoothConfig = applyLocalBooth
			}
		}
		a.mixer = a.booth.mixer

		// A REMOTE writer for the booth's scripts.
		//
		// The alternative to -local-script-model, and the reason to pick it is
		// the same as for remote speech: it uses no GPU. A local 30B and the
		// race calls contend for one Metal device, which is what made calls
		// late enough to need the mid-race yield in the first place.
		//
		// No background load and no readiness wait — there is nothing to load.
		// A writer on another machine. Checked before the remote-API and local
		// options, because naming a host is the most explicit choice of the
		// three and should win.
		if *modelURL != "" {
			if _, hasWriter, perr := probeModelHost(*modelURL); perr != nil {
				log.Printf("booth: model host %s unreachable (%v) — falling back", *modelURL, perr)
			} else if hasWriter {
				a.booth.mu.Lock()
				a.booth.mainScript = newRemoteWriter(*modelURL)
				a.booth.mainScriptName = "model host " + *modelURL
				a.booth.enabled = true
				a.booth.voiceMode = "solo"
				a.booth.mu.Unlock()
				log.Printf("booth: writing commentary on the model host at %s — no GPU used here", *modelURL)
			} else {
				log.Printf("booth: model host %s serves no writer (start it with -local-script-model)", *modelURL)
			}
		}

		if *qwenScriptModel != "" && *qwenKey != "" {
			model := *qwenScriptModel
			if model == "default" {
				model = qwenchat.DefaultModel
			}
			qc, qerr := qwenchat.New(qwenchat.Config{APIKey: *qwenKey, Model: model})
			if qerr != nil {
				log.Printf("booth: remote script model unavailable (%v) — falling back", qerr)
			} else {
				// Metered, so the spend page is not silently zero.
				//
				// The usage hooks were only ever attached on the CLOUD config
				// path, so a local-only venue running Qwen saw "~$0.00 · 0
				// calls" no matter how long it had been talking — the one number
				// someone worried about cost goes looking for, reading as free.
				qc.OnUsage = a.booth.usage.add
				a.booth.mu.Lock()
				a.booth.mainScript = qc
				a.booth.mainScriptName = qc.Model()
				a.booth.enabled = true
				a.booth.voiceMode = "solo"
				a.booth.mu.Unlock()
				log.Printf("booth: writing commentary REMOTELY with %s — no GPU used", qc.Model())
			}
		} else if *qwenScriptModel != "" {
			log.Printf("booth: -qwen-script-model needs -qwen-key — not writing remotely")
		}

		// A local writer for the booth's scripts. Loaded in the background: a
		// 30B model takes minutes to come up, and the timing loop must never
		// wait on it — the booth simply starts writing locally once it is ready,
		// and until then behaves exactly as it would without the flag.
		if *localScriptModel != "" {
			python, script := *localVoicePython, *localVoiceScript
			if script == "" {
				script = defaultLocalVoiceScript()
			}

			// Local dev with no cloud and no Gemini: nobody is going to send a
			// booth config, so switch it on here. Without this the booth loads a
			// model and then never speaks.
			a.booth.mu.Lock()
			a.booth.enabled = true

			// SOLO, because there is only one voice to speak with: the
			// voice here is a single cloned commentator, so "duo" renders
			// both speakers identically and what comes out is one person
			// holding a conversation with themselves.
			a.booth.voiceMode = "solo"
			a.booth.mu.Unlock()

			log.Printf("booth: one cloned voice, so commentary is SOLO")

			// A local writer shares this machine's GPU with the race calls, so
			// it stands aside while a heat is running. Measured: commentary
			// generation takes 17-31s and pushed lap calls from ~2s to ~12s.
			a.booth.mu.Lock()
			a.booth.yieldWhileRacing = *boothYield
			a.booth.mu.Unlock()

			go func() {
				lc, lerr := localchat.Start(context.Background(), localchat.Config{
					Python: python,
					Script: script,
					Model:  *localScriptModel,
				})
				if lerr != nil {
					log.Printf("booth: local script model unavailable (%v) — scripts stay with Gemini", lerr)
					return
				}
				a.booth.mu.Lock()
				a.booth.mainScript = lc
				a.booth.mainScriptName = lc.Model()
				a.booth.mainScriptLocal = true
				a.booth.mu.Unlock()
				log.Printf("booth: writing commentary locally with %s", lc.Model())
			}()

			// The booth needs a local VOICE as well as a local writer, or it
			// falls back to Gemini TTS and the "runs with no network" claim is
			// only half true — which is exactly what the 429s on startup were.
			//
			// Its OWN engine, deliberately, rather than sharing the race
			// announcer's. A commentary segment is twenty seconds of audio and a
			// lap call is one; on a shared sidecar the call waits behind the
			// colour, which is the single problem this whole audio design exists
			// to remove. The TTS model is small enough that a second copy is a
			// far better trade than reintroducing that queue.
			go func() {
				engine, eerr := tts.StartLocal(context.Background(), tts.LocalConfig{
					Python:         python,
					Script:         script,
					Model:          *localVoiceModel,
					ReferenceAudio: *localVoiceRef,
					ReferenceText:  *localVoiceRefText,
				})
				if eerr != nil {
					log.Printf("booth: local voice unavailable (%v) — commentary needs Gemini TTS", eerr)
					return
				}
				a.booth.mu.Lock()
				a.booth.speech = engine
				a.booth.mu.Unlock()
				// The audition and voice test reuse this engine rather than
				// starting their own — see sharedengine.go for the 16GB story.
				registerLongformEngine(engine, tts.LocalConfig{
					Python:         python,
					Script:         script,
					Model:          *localVoiceModel,
					ReferenceAudio: *localVoiceRef,
					ReferenceText:  *localVoiceRefText,
				})
				log.Printf("booth: voicing commentary locally")
			}()
		}

		// The booth's VOICE, when it is a remote one.
		//
		// This was only ever assigned inside the local-script-model branch, so
		// a venue running entirely on a Qwen key got a writer and no voice —
		// "no voice: a local script model needs a local voice too", which names
		// a flag that has nothing to do with the setup they chose. One key is
		// supposed to be the whole configuration, and this is the other half of
		// making that true.
		if *qwenKey != "" {
			qc, qerr := qwentts.New(qwentts.Config{APIKey: *qwenKey, BaseURL: *qwenBase})
			if qerr != nil {
				log.Printf("booth: qwen voice unavailable (%v)", qerr)
			} else {
				// Qwen speech bills per CHARACTER, not per token, and reports
				// {"characters":N} with no token fields — so it needs its own
				// bucket rather than being folded into the token counters.
				qc.OnUsage = a.booth.usage.add
				a.booth.mu.Lock()
				if a.booth.speech == nil {
					a.booth.speech = tts.FromQwen(qc)
					log.Printf("booth: voicing commentary with Qwen3-TTS — no GPU used")
				}
				a.booth.mu.Unlock()
			}
		}

		// The race calls share the booth's bus, so a call outranks colour by
		// priority rather than by fighting it for the speaker.
		if *raceAudio {
			a.startRaceAudio(*raceAudioProfile, tts.LocalConfig{
				Python:         *localVoicePython,
				Script:         *localVoiceScript,
				Model:          *localVoiceModel,
				ReferenceAudio: *localVoiceRef,
				ReferenceText:  *localVoiceRefText,
			}, remoteVoice{APIKey: *qwenKey, Voice: *qwenVoice, BaseURL: *qwenBase, HostURL: *modelURL})
		}
		// The /booth.wav source the website-driven OBS path injects into scenes.
		a.booth.audioURL = "http://127.0.0.1" + *serveAddr + "/booth.wav"
		if *boothMinDelay > 0 {
			a.booth.sync.setFloor(boothMinDelay.Milliseconds())
			log.Printf("booth: broadcast-sync floor set to %s (auto buffer starts here and only grows)", *boothMinDelay)
		}
		// Broadcast sync: keep the generated audio lined up with the video by
		// delaying the video to match. Preferred path drives OBS automatically;
		// the manual path holds audio to a fixed buffer the operator dials into
		// OBS by hand.
		if *obsURL != "" {
			var sources []string
			for _, s := range strings.Split(*obsSource, ",") {
				if s = strings.TrimSpace(s); s != "" {
					sources = append(sources, s)
				}
			}
			audioURL := "http://127.0.0.1" + *serveAddr + "/booth.wav"
			a.booth.obs = newBoothOBS(*obsURL, *obsPassword, sources, *obsInjectAudio, audioURL)
			scope := "every scene"
			if len(sources) > 0 {
				scope = strings.Join(sources, ", ")
			}
			log.Printf("booth: broadcast sync ON — auto-delaying OBS video (%s) to match the generated commentary", scope)
		} else if *boothVideoDelay > 0 {
			a.booth.manualDelayMs = boothVideoDelay.Milliseconds()
			log.Printf("booth: broadcast sync ON (manual) — holding audio to %s; set your OBS video render delay to match", *boothVideoDelay)
		}
		a.local.booth = a.booth
		a.local.mixer = a.mixer
		if a.booth != nil {
			a.local.tape = a.booth.tape
		}
		go a.booth.run(ctx)
	} else if *raceAudio && a.local != nil {
		// Race calls WITHOUT the commentary booth.
		//
		// The two are separate features that happen to share a speaker, and a
		// venue that just wants FPVTrackside's calls in one voice should not
		// have to stand up an AI commentator — and find an API key for it — to
		// get them. So the bus is built here on its own.
		a.mixer = newBoothMixer()
		a.local.mixer = a.mixer
		go a.mixer.run(ctx)

		a.startRaceAudio(*raceAudioProfile, tts.LocalConfig{
			Python:         *localVoicePython,
			Script:         *localVoiceScript,
			Model:          *localVoiceModel,
			ReferenceAudio: *localVoiceRef,
			ReferenceText:  *localVoiceRefText,
		}, remoteVoice{APIKey: *qwenKey, Voice: *qwenVoice, BaseURL: *qwenBase})
		log.Printf("booth: commentary bus ready — OBS media source: http://localhost%s/booth.wav (control: /setup/booth)", *serveAddr)
	} else if *boothEnabled {
		log.Fatalf("dd-pits: -booth requires -serve (the audio stream is served by the local server)")
	}

	// Local monitoring. Placed here because both paths above build the bus, and
	// it needs whichever one ran.
	//
	// This is the answer to "the audio is delayed in OBS": /booth.wav is a
	// network stream, and ffmpeg fills a buffer measured in MEGABYTES before it
	// plays anything — ~45s at this bitrate. Nothing on this side causes that
	// and nothing on this side can fix it. Playing locally skips HTTP entirely.
	// Local playback is owned by a controller so the settings page can turn it
	// on, off, or move it to another device while a meeting is running.
	if a.mixer != nil {
		ctrl := newAudioOutController(ctx, a.mixer, *localVoicePython)
		// Listed on every run, so the log a venue sends already says what that
		// machine can play through.
		go logAudioOutputs(*localVoicePython)
		if a.local != nil {
			a.local.applyAudioOut = ctrl.Apply
			a.local.audioPlaying = ctrl.Playing
			a.local.audioOutputs = func() ([]audioDevice, string, error) {
				return listAudioOutputs(*localVoicePython)
			}
		}
		if *audioOut {
			if err := ctrl.Apply(true, *audioOutDevice, *audioOutLatency); err != nil {
				// Never fatal: /booth.wav still carries everything, so a missing
				// output device costs monitoring, not the meeting.
				log.Printf("audio out: %v — local playback off, /booth.wav unaffected", err)
			} else {
				log.Printf("audio out: playing through this machine's speakers")
			}
		}
	}
	if *audioOut && a.mixer == nil {
		log.Printf("audio out: no audio bus (needs -serve with -race-audio or -booth) — not playing locally")
	}

	if a.local != nil {
		go a.local.start(ctx, *serveAddr)
	}

	// Optional ELRS backpack: OSD to pilots' goggles + race-director commands.
	// The settings file can enable the backpack and name it, for a venue that
	// configures through the pages rather than flags. A FLAG STILL WINS: it is
	// an explicit instruction from whoever started the process.
	if a.localCfg != nil {
		le := a.localCfg.ELRSSettings()
		if le.Enabled {
			*elrsEnabled = true
		}
		// Resolved through the stored MODE: "both" has to force serial to
		// "auto", because the library only falls back from netpack to USB for
		// that exact value.
		np, ser := le.transport()
		if ser != "" && *elrsSerial == "" {
			*elrsSerial = ser
		}
		if *elrsNetpack == "elrs-netpack.local" {
			*elrsNetpack = np // "" for USB-only, which disables the netpack
		}
	}

	if *elrsEnabled {
		a.startELRS(ctx, *elrsSerial, *elrsNetpack)
	}

	// Enabling the backpack on the setup page starts it there and then.
	if a.local != nil {
		a.local.enableBackpack = func() error {
			le := a.localCfg.ELRSSettings()
			if !le.Enabled {
				return fmt.Errorf("the backpack is switched off on this page")
			}
			np, ser := le.transport()
			a.startELRS(ctx, ser, np)
			return nil
		}
	}

	timerURL := *ftURL
	if *rhURL != "" {
		timerURL = *rhURL
	}
	log.Printf("dd-pits: polling %s at %s (idle=%s active=%s)",
		timingSystem, timerURL, interval, activeInterval)

	if *once {
		if err := a.safeTick(ctx); err != nil {
			log.Fatalf("dd-pits: %v", err)
		}
		return
	}

	// Self-resetting timer so we can pick a different interval after each
	// tick depending on whether a race is currently in-flight.
	a.runTick(ctx)
	timer := time.NewTimer(a.nextInterval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("dd-pits: shutdown signal, exiting")
			return
		case <-timer.C:
			a.runTick(ctx)
			timer.Reset(a.nextInterval())
		}
	}
}

// safeTick runs one tick with a panic guard so a single bad snapshot/parse can
// never crash the whole agent — the loop logs the error and tries again next
// tick. This is the difference between "recovers on its own" and "stops sending
// data until someone restarts it".
func (a *agent) safeTick(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered panic in tick: %v", r)
		}
	}()
	return a.tick(ctx)
}

// runTick runs a panic-safe tick and tracks connection state, so a flaky link
// produces clear self-healing logs instead of looking like a silent stall. The
// agent NEVER stops retrying — it just paces itself at the normal tick cadence.
func (a *agent) runTick(ctx context.Context) {
	if err := a.safeTick(ctx); err != nil {
		// A tick only fetches FPVTrackside + updates the local board now (the cloud
		// POST runs on the pushLoop goroutine, which tracks its own connection
		// state), so a tick failure means FPVTrackside is unreachable / unparseable.
		a.fetchFailures++
		// First few failures verbosely, then sparsely, so a long outage doesn't
		// flood the console while still showing it's alive and retrying.
		if a.fetchFailures <= 3 || a.fetchFailures%20 == 0 {
			log.Printf("dd-pits: %s read failed (#%d): %v — retrying every %s",
				timingSystem, a.fetchFailures, err, a.nextInterval())
		}
		return
	}
	if a.fetchFailures > 0 {
		log.Printf("dd-pits: %s read recovered after %d failed read(s)", timingSystem, a.fetchFailures)
		a.fetchFailures = 0

		// Re-baseline the race calls.
		//
		// An outage usually means FPVTrackside was restarted, and what comes
		// back is not a continuation of what we were watching — it may be a
		// different event entirely, with race ids we have never seen. Every one
		// of those races then looks like news, and the whole day's history is
		// announced at once. Measured here: a hundred-odd calls in a single
		// snapshot, which filled the bus and pushed real racing off it.
		//
		// Treated exactly like startup: the first snapshot after an outage is
		// the state of the world, not a list of things that just happened.
		if ann := a.raceAnnouncer(); ann != nil {
			ann.Reprime(timingSystem + " came back")
		}
	}
}

// tick fetches the current event id, pulls an incremental snapshot, and POSTs
// it — but only when the snapshot hash differs from the last one sent OR
// the heartbeat deadline has passed.
//
// FPVTrackside being offline is non-fatal — we'll try again next tick.
func (a *agent) tick(ctx context.Context) error {
	tctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	snap, err := a.fetchSnapshot(tctx)
	if err != nil {
		return err
	}
	return a.handleSnapshot(snap)
}

// fetchSnapshot reads the meeting from whichever timer this venue uses.
func (a *agent) fetchSnapshot(tctx context.Context) (dronedash.Snapshot, error) {
	if a.rh != nil {
		return a.fetchRotorHazard(tctx)
	}
	return a.fetchFPVTrackside(tctx)
}

// fetchRotorHazard asks the plugin whether anything changed and only pulls the
// meeting when it has. The plugin bumps its version on every lap, heat change
// and edit, so an unchanged version means the previous snapshot is still exact.
func (a *agent) fetchRotorHazard(tctx context.Context) (dronedash.Snapshot, error) {
	v, err := a.rh.FetchVersion(tctx)
	if err != nil {
		return dronedash.Snapshot{}, fmt.Errorf("RotorHazard not reachable (is the dd-pits plugin installed?): %w", err)
	}
	if v != "" && v == a.rhVersion && a.prev.Event.ID != "" {
		return a.prev, nil
	}
	snap, err := a.rh.FetchSnapshot(tctx)
	if err != nil {
		return snap, fmt.Errorf("read RotorHazard meeting: %w", err)
	}
	a.rhVersion = v
	return snap, nil
}

// fetchFPVTrackside reads FPVTrackside's HTTP API, reusing finished heats from
// the previous snapshot between periodic full re-reads.
func (a *agent) fetchFPVTrackside(tctx context.Context) (dronedash.Snapshot, error) {
	eventID, err := a.ft.FetchCurrentEventID(tctx)
	if err != nil {
		return dronedash.Snapshot{}, fmt.Errorf("FPVTrackside not reachable: %w", err)
	}
	// Periodically force a FULL re-read (empty prev) instead of the incremental
	// fetch. The incremental path treats a finished heat as immutable and reuses
	// it from the cache forever — so a re-run (FPVTrackside clears the heat's End
	// and runs it again) or a hand-edit of a finished race would never be seen.
	// A full reconcile every heartbeat re-reads every heat and picks those up.
	// Reconcile every few seconds (not every tick) so re-runs surface quickly
	// while still skipping the per-heat re-fetch on most ticks — during active
	// racing (sub-second ticks) only ~1 in N ticks pays for a full re-read.
	const reconcileInterval = 5 * time.Second
	prevForFetch := a.prev
	dueForReconcile := time.Since(a.lastReconcileAt) >= reconcileInterval
	if dueForReconcile {
		prevForFetch = dronedash.Snapshot{}
	}
	snap, err := a.ft.FetchSnapshotIncremental(tctx, eventID, prevForFetch)
	if err != nil {
		return snap, fmt.Errorf("fetch snapshot: %w", err)
	}
	// A change of the operator's selected race (FPVTrackside /current) means a heat
	// was just cued — and it may be a RE-RUN (a finished heat the operator reset).
	// The incremental fetch reuses finished heats from cache, so it would still show
	// the stale "ended" version until the periodic reconcile. Force a fresh whole-
	// meeting re-read THIS tick so the reset (cleared start/end) — and the new
	// selection — surface immediately (sub-second), not after the 5s reconcile.
	if !dueForReconcile && snap.SelectedRaceID != a.lastSelected {
		if fresh, ferr := a.ft.FetchSnapshotIncremental(tctx, eventID, dronedash.Snapshot{}); ferr == nil {
			snap = fresh
			dueForReconcile = true
		}
	}
	a.lastSelected = snap.SelectedRaceID
	if dueForReconcile {
		a.lastReconcileAt = time.Now()
	}
	return snap, nil
}

// handleSnapshot feeds one fresh snapshot to everything that consumes it: the
// dashboard, the goggles, the booth and the race calls.
func (a *agent) handleSnapshot(snap dronedash.Snapshot) error {
	a.prev = snap // keep for next tick's incremental fetch

	// Persist a cued UN-RUN heat across an "earlier review". FPVTrackside's /current
	// goes null when the operator clicks an already-run heat to review it — but a
	// future heat they cued to run out of sequence should STAY on the dashboard
	// (rather than the board flipping to the next-up heat). Remember the last un-run
	// cued heat and keep reporting it as the selection through the review; it clears
	// once that heat runs, or the operator cues a different un-run heat. Reported via
	// SelectedRaceID so the local board and the cloud stay in lock-step.
	a.cuedRace, snap.SelectedRaceID = persistCue(a.cuedRace, snap.SelectedRaceID, func(id string) bool {
		if id == "" {
			return false
		}
		for _, rc := range snap.Races {
			if rc.ID == id {
				return parseTime(rc.Start).IsZero() && parseTime(rc.End).IsZero()
			}
		}
		return false // not in the meeting (yet) → not a live cue
	})

	// Stamp our wall clock so the cloud can measure the venue↔server offset and
	// correct the live race timer (start_at is on this laptop's clock, which may
	// be unsynced), and so both the cloud and the local board have a "now" to
	// retire a heat that's been left started-but-unended and quiet for too long.
	// Only while a race is running: that's when it matters, and running snapshots
	// change every push anyway, so this doesn't defeat the idle-snapshot dedup
	// hash (which keeps byte-identical pushes cheap). Set before the local handoff
	// so the offline board gets the same reference.
	if snap.HasRunningRace() {
		snap.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}

	// Feed the local dashboard (if running) before the cloud push, so it stays
	// live even when the cloud is unreachable.
	if a.local != nil {
		a.local.setSnapshot(snap)
	}

	// Feed the ELRS OSD engine (if enabled). Coalescing hand-off — a slow
	// backpack never stalls this loop.
	if a.elrs != nil {
		a.elrs.Observe(snap)
	}

	// Feed the commentary booth (if enabled) — beat detection is cheap and
	// generation runs on the booth's own goroutine.
	if a.booth != nil {
		a.booth.observe(snap)
	}

	// Speak the race calls (if enabled). Shares the booth's mixer, so a call
	// outranks colour by priority rather than by fighting for the speaker.
	//
	// Two sources, one voice: Observe derives the calls worth improving on from
	// the timing data, and Relay picks up everything else FPVTrackside said from
	// its log. speechbridge.go is what stops them overlapping.
	if ann := a.raceAnnouncer(); ann != nil {
		// Wired here rather than at construction because the announcer comes up
		// on its own goroutine minutes after the booth (the voice model takes
		// that long to load), so at booth-creation time there is nothing to
		// wire to.
		if a.booth != nil && a.booth.onAnnounceRecords == nil {
			a.booth.onAnnounceRecords = ann.caller.SetAnnounceRecords
		}
		// The pushed-event path is wired here for the same reason the booth hook
		// above is: the announcer comes up on its own goroutine well after the
		// server, so there is nothing to attach to at construction time. Attached
		// once, then left alone.
		if a.local != nil && a.local.ftEvents != nil && a.local.ftEvents.onEvent == nil {
			a.local.ftEvents.onEvent = ann.onFTLap
			log.Printf("ft events: lap calls will come from the push when it beats the poll")
		}

		// The operator advancing to another race retires the last one's
		// post-race audio — see postskip.go. Detected here because the
		// snapshot is in hand; first observation only primes.
		if sel := snap.SelectedRaceID; sel != "" && a.lastSelectedRace != "" &&
			sel != a.lastSelectedRace {
			a.skipPostRace()
		}
		if snap.SelectedRaceID != "" {
			a.lastSelectedRace = snap.SelectedRaceID
		}

		ann.Observe(*buildMeeting(snap))
		ann.Relay()
	}

	return nil
}

// persistCue decides which heat the dashboard treats as cued/current, keeping a
// future heat the operator cued (to run out of sequence) on the dashboard even
// while they click back to an already-run heat to review it.
//
//   - raw   = FPVTrackside's current selection (empty when reviewing an already-run
//     heat, since /current returns null for an ended race).
//   - cued  = the heat we're currently persisting.
//   - pending(id) reports whether a heat is still un-run.
//
// Rules: a fresh un-run selection becomes the cue; otherwise the existing cue is
// kept as long as it's still un-run (this is the "persist through a review" case);
// once the cue has run (or there never was one) it's dropped. The returned cue is
// also the selection to report, so local + cloud render the same heat.
func persistCue(cued, raw string, pending func(string) bool) (newCued, sel string) {
	switch {
	case pending(raw):
		newCued = raw // a fresh (or re-confirmed) un-run cue
	case pending(cued):
		newCued = cued // keep the still-un-run cue across an earlier-heat review
	default:
		newCued = "" // the cue has run / vanished, or there never was one
	}
	return newCued, newCued
}

// startELRS brings the goggle link up.
//
// Callable at RUNTIME as well as at startup, because a club that ticks "use an
// ELRS backpack" on the setup page should get goggles, not an instruction to
// restart with a flag. Guarded so a second call is a no-op: the page can be
// saved repeatedly and the poll loop reads a.elrs concurrently.
func (a *agent) startELRS(ctx context.Context, serial, netpack string) {

	a.elrsMu.Lock()
	if a.elrs != nil {
		a.elrsMu.Unlock()
		return // already running
	}
	a.elrsMu.Unlock()

	// Over TCP the netpack has a deep ring buffer and TCP flow control;
	// the conservative default frame pacing only matters for raw serial.
	// 1ms saves ~30-50ms of pure sleep per OSD session.
	frameDelay := time.Duration(0) // 0 = library default (serial-safe)
	if serial == "" && netpack != "" {
		frameDelay = time.Millisecond
	}
	bp := elrs.NewBackpack(elrs.BackpackConfig{
		Serial:     serial,
		Netpack:    netpack,
		FrameDelay: frameDelay,
		Logf:       log.Printf,
		// Inbound frames (netpack delivery reports, TX-backpack DVR
		// traffic) route to the controller; a.elrs is assigned before
		// bp.Run starts the rx loop, so the read below is safe.
		OnPacket: func(p elrs.Packet) {
			if ec := a.elrs; ec != nil {
				ec.handleInbound(p)
			}
		},
	})
	a.elrs = newELRSController(bp)
	a.elrs.localCfg = a.localCfg

	// The setup pages drive the goggles directly: a pilot added between heats works on the
	// next one, and the race director can change a channel from the desk.
	if a.local != nil {
		a.local.reloadELRS = func() {
			n := a.elrs.applyLocal()
			log.Printf("elrs: reloaded local pilot config (%d pilots)", n)
		}
		a.local.setChannel = a.elrs.setChannelByCallsign
		a.local.backpackStatus = bp.Connected
		a.local.sendAnnounce = a.elrs.announceToPilots
	}
	go bp.Run(ctx)
	go a.elrs.engineLoop(ctx)
	a.elrs.loadConfig()
	go a.elrs.clockLoop(ctx)
	go a.elrs.deliveryLoop(ctx)
	log.Printf("dd-pits: ELRS backpack integration enabled (serial=%q netpack=%q)", serial, netpack)
}

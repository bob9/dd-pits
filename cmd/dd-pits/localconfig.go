package main

// Running a whole race day with no platform account.
//
// dd-pits already reads FPVTrackside and serves the dashboard, the overlays,
// the race calls and the commentary booth without touching the internet. What
// it could not do offline is the things whose CONFIGURATION lives on the
// website: which goggles belong to which pilot, what their OSD looks like, and
// how the booth is set up. Not because that data is large — a pilot's whole
// ELRS config is a bind phrase — but because there was nowhere local to put it.
//
// This is that place: one JSON file beside the agent, edited through pages the
// agent serves itself. A club with no account gets the full race day; a club
// with an account is unaffected, because the file is only consulted when there
// is no cloud to consult instead.
//
// THE RULE THAT KEEPS THIS HONEST: the cloud wins whenever it is present. A
// connected agent shows these pages read-only and points at the website. Two
// editable copies of the same pilot is how you end up owning two-way sync
// without ever deciding to, and that is a product decision rather than a
// storage one.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bob9/dd-pits/internal/elrs"
)

// localConfigVersion is stamped into the file so a future format change can be
// migrated rather than guessed at. A venue's file outlives any one release.
const localConfigVersion = 1

// localPilot is one pilot's goggle setup.
//
// BindPhrase is what a pilot actually knows — the words they typed into their
// goggles — and the UID is derived from it exactly as the ExpressLRS firmware
// does (elrs.UIDFromPhrase). Storing the phrase rather than the derived UID
// means an operator can read the file and see something meaningful, and a
// mistyped phrase can be corrected without anyone computing a hash.
//
// UID is still accepted for a pilot who knows theirs and not their phrase, and
// wins when both are set.
type localPilot struct {
	Callsign   string       `json:"callsign"`
	BindPhrase string       `json:"bind_phrase,omitempty"`
	UID        string       `json:"uid,omitempty"`
	OSDEnabled bool         `json:"osd_enabled"`
	Layout     *elrs.Layout `json:"layout,omitempty"` // nil = the shared default
}

// resolvedUID returns the goggle UID for this pilot, or "" when unset.
func (p localPilot) resolvedUID() string {
	if u := strings.TrimSpace(p.UID); u != "" {
		return strings.ToLower(u)
	}
	if ph := strings.TrimSpace(p.BindPhrase); ph != "" {
		return elrs.UIDFromPhrase(ph).String()
	}
	return ""
}

// localBooth is the commentary configuration a website would otherwise hold.
//
// Engine picks where the VOICE comes from. Both are "local" in the sense that
// matters — neither needs this platform — but they fail differently, and a
// venue should choose deliberately:
//
//	"local"  MLX on this machine. No internet, no per-word cost, and it
//	         competes with everything else for the GPU. Apple Silicon only.
//	"qwen"   Alibaba Model Studio over the internet. Runs on any hardware and
//	         leaves the GPU free, but a dropout means silence.
//	""       no booth.
type localBooth struct {
	// EngineWas remembers where the voice ran, so switching commentary OFF and
	// back ON returns to the same setup instead of silently reverting to the
	// cloud. engine=="" is the off state, which means the choice would otherwise
	// be destroyed by the act of pausing.
	EngineWas string `json:"engine_was,omitempty"`

	Engine     string `json:"engine"` // local | qwen | ""
	QwenKey    string `json:"qwen_key,omitempty"`
	QwenBase   string `json:"qwen_base,omitempty"` // region override
	VoicePBP   string `json:"voice_pbp,omitempty"`
	VoiceColor string `json:"voice_color,omitempty"`
	Mode       string `json:"mode,omitempty"` // solo | duo
	Style      string `json:"style,omitempty"`
	Theme      string `json:"theme,omitempty"`
	Humour     string `json:"humour,omitempty"`

	// Language is what the commentary is SPOKEN IN. Blank = English.
	//
	// Accent is a regional variety of it ("Brabants", "Québécois", "Bavarian")
	// and is ignored without a language — it is a delivery instruction, not a
	// language of its own. It reaches the voice as well as the script, which is
	// the bug it exists to prevent: a club whose voice characteristics say
	// "Australian, ESPN polish" was having its Dutch commentary read with an
	// Australian direction.
	Language string `json:"language,omitempty"`
	Accent   string `json:"accent,omitempty"`

	// Energy is how hard the delivery pushes — separate from humour (density of
	// wit) and personality (character). A straight booth can still be loud.
	Energy string `json:"energy,omitempty"`

	// Beats a venue can silence individually, mirroring the website's per-beat
	// toggles. Absent = the shared defaults.
	Beats map[string]bool `json:"beats,omitempty"`

	// Phase toggles: the race director's coarse control.
	//
	// PRE is the build-up (welcome, the lineup, between-heats chatter), IN is
	// everything while a heat is running, POST is the result wrap. A beat fires
	// only when its phase AND its own toggle are on.
	//
	// *bool because ABSENT MUST MEAN ON: a settings file written before these
	// existed, or by a club that never opened the page, must not silence a
	// whole phase of the broadcast.
	PhasePre  *bool `json:"phase_pre,omitempty"`
	PhaseIn   *bool `json:"phase_in,omitempty"`
	PhasePost *bool `json:"phase_post,omitempty"`

	// AnnounceRecords turns on the personal-best race calls.
	AnnounceRecords bool `json:"announce_records"`

	// The WRITER, which is separate from the voice and easy to forget.
	//
	// Something has to write the words before anything can say them. Setting a
	// voice and no writer starts an agent that refuses the booth outright —
	// which is a confusing way to learn that commentary has two halves.
	//
	//	ScriptModel  a model on this machine (MLX, Apple Silicon)
	//	GeminiKey    Google AI Studio, over the internet
	ScriptModel string `json:"script_model,omitempty"`
	GeminiKey   string `json:"gemini_key,omitempty"`

	// The local voice needs a Python and a reference clip to clone. Flags for
	// these exist, but a club configuring through the pages should not have to
	// discover them.
	VoicePython string `json:"voice_python,omitempty"`
	// VoiceRefs is the venue's library of recorded voices.
	//
	// More than one, because a club has more than one commentator and a voice
	// worth recording is worth keeping — swapping between them should not mean
	// re-uploading a file you already had. VoiceRef/VoiceRefText below stay as
	// the ACTIVE pair so the agent and every existing caller are unchanged.
	// Pronounce is the venue's pronunciation rules, one per line:
	// "Bola = Boh-lah". Applied only where text meets a TTS engine (sayable),
	// so caches, dedup and transcripts keep the real names. Local-only setups
	// have no reach into the website's per-pilot pronunciation cards; this is
	// their equivalent.
	Pronounce string `json:"pronounce,omitempty"`

	VoiceRefs []localVoiceRef `json:"voice_refs,omitempty"`

	VoiceRef     string `json:"voice_ref,omitempty"`
	VoiceRefText string `json:"voice_ref_text,omitempty"`
}

// phaseOn resolves a phase toggle, where absent means on.
func phaseOn(v *bool) bool { return v == nil || *v }

// beatOn resolves a per-beat toggle. Absent means on, for the same reason the
// phases do: a file that predates the setting must not silence anything.
func (b localBooth) beatOn(kind string) bool {
	if b.Beats == nil {
		return true
	}
	on, ok := b.Beats[kind]
	return !ok || on
}

// localAudio is where the sound comes out.
type localAudio struct {
	// Enabled plays through this machine's speakers. Off means the audio is
	// only available as the /booth.wav stream for OBS.
	Enabled bool   `json:"enabled"`
	Device  string `json:"device,omitempty"`  // name fragment; empty = system default
	Latency string `json:"latency,omitempty"` // seconds, or "low"
}

// localVoiceRef is one recorded voice: a name, the audio, and what it says.
type localVoiceRef struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Text string `json:"text"`
}

// localELRS is how this venue reaches the goggles.
//
// These are command-line flags too, and the flags still win — whoever started
// the process said something explicit. But a club setting up for the first time
// should not have to know flag names to turn goggle messaging on, and the
// netpack hostname is exactly the sort of thing that differs per venue and gets
// forgotten between meetings.
type localELRS struct {
	Enabled bool `json:"enabled"`

	// Mode is the operator's CHOICE of transport, kept separate from the
	// hostname and port it resolves to.
	//
	// The two fields alone could express every arrangement, but not
	// unambiguously: a netpack plus a named port silently means "ignore the
	// netpack", while a netpack plus "auto" means "netpack, then USB". Those
	// read identically in a form and behave completely differently, so the
	// intent is stored rather than inferred.
	//
	//	"netpack"  the network only
	//	"usb"      a USB backpack only
	//	"both"     netpack first, USB if it is missing
	Mode string `json:"mode,omitempty"`

	Netpack string `json:"netpack,omitempty"` // hostname, TCP :8080
	Serial  string `json:"serial,omitempty"`  // port, or "auto" to scan

	// RaceChannels puts every pilot's goggles on their race channel as each
	// race is cued, so nobody starts on the wrong video.
	RaceChannels bool `json:"race_channels,omitempty"`
}

// transport resolves the mode into the hostname and port the backpack library
// takes, which is where the subtle part lives: the library falls back from
// netpack to USB only when the serial field is exactly "auto".
func (e localELRS) transport() (netpack, serial string) {
	port := strings.TrimSpace(e.Serial)
	if port == "" {
		port = "auto"
	}
	switch strings.ToLower(e.Mode) {
	case "usb":
		return "", port
	case "both":
		// "auto" is required here, not merely preferred: a NAMED port is tried
		// instead of the netpack rather than after it, so honouring one would
		// quietly turn "both" into "USB only".
		return e.Netpack, "auto"
	case "netpack":
		return e.Netpack, ""
	}
	// No stored mode (a file written before this existed): behave as the
	// fields say, which is what that file already meant.
	return e.Netpack, e.Serial
}

// localCalls decides who speaks the race calls.
//
// A venue without a local voice model — no Apple Silicon, no Qwen key — must
// still get a working race day, and FPVTrackside has spoken its own calls for
// years. So this is not an all-or-nothing feature:
//
//	"ddpits"      dd-pits voices everything and FPVTrackside stays silent.
//	"trackside"   FPVTrackside speaks its own calls, as it always has, and
//	              dd-pits does not touch the audio. Everything else — the
//	              dashboard, overlays, goggles — still works.
//
// Defaulting to "trackside" would be wrong for a venue that HAS set a voice up,
// so the resolution is by capability rather than by a stored preference: see
// callsSourceFor.
type localCalls struct {
	Source string `json:"source"` // ddpits | trackside

	// Where FPVTrackside keeps its files.
	//
	// dd-pits reads two of them: Sounds.xml for the WORDING of every call (so a
	// venue's own phrasing is used rather than something invented here), and
	// the log, which is how it learns about the calls it does not derive
	// itself — "Hurry Up" is a keypress and invisible from outside.
	//
	// Both are assumed to be under ~/Documents/FPVTrackside. A portable
	// install, a second drive or a renamed profile breaks that assumption, and
	// the symptom is no race calls with nothing explaining why — so it is
	// configurable, and the setup page says whether the files were found.
	TracksideDir string `json:"trackside_dir,omitempty"`
	Profile      string `json:"profile,omitempty"` // default "Profile 1"

	// FTRestartNeeded records that who-speaks changed while FPVTrackside was
	// RUNNING — so it is still using the old audio setting in memory, whatever
	// the file now says. Set at the save, shown as a banner on the calls page,
	// and cleared by the reconciler the moment FPVTrackside is seen closed
	// (its next start reads the corrected file). Persisted, because the advice
	// used to live on a redirect page that existed for two seconds.
	FTRestartNeeded bool `json:"ft_restart_needed,omitempty"`
	// FTRestartDone is the follow-through: set by the reconciler the moment it
	// sees FPVTrackside closed, shown once on the calls page as "that worked",
	// then cleared. Without it the banner just vanished, and a vanished warning
	// is indistinguishable from a missed one.
	FTRestartDone bool `json:"ft_restart_done,omitempty"`
}

// soundsPath is the wording file, or "" to use the default location.
func (c localCalls) soundsPath() string {
	if c.TracksideDir == "" {
		return ""
	}
	profile := c.Profile
	if profile == "" {
		profile = "Profile 1"
	}
	return filepath.Join(c.TracksideDir, "data", profile, "Sounds.xml")
}

// logPath is the speech log the relay follows, or "" for the default.
func (c localCalls) logPath() string {
	if c.TracksideDir == "" {
		return ""
	}
	return filepath.Join(c.TracksideDir, "log", "Log.txt")
}

// localConfig is the whole file.
type localConfig struct {
	Version int    `json:"version"`
	Club    string `json:"club,omitempty"` // shown on the dashboard chrome

	DefaultLayout *elrs.Layout `json:"default_layout,omitempty"`
	Pilots        []localPilot `json:"pilots,omitempty"`

	Booth localBooth `json:"booth"`
	Calls localCalls `json:"calls"`
	ELRS  localELRS  `json:"elrs"`
	Audio localAudio `json:"audio"`
	Video localVideo `json:"video"`

	// RHTimer is the RotorHazard an FPVTrackside times with, for marshalling
	// from its signal traces. Empty URL: read it from FPVTrackside's own timer
	// settings. Key: the dd-pits plugin's key on that RotorHazard, if the
	// plugin is installed there (without it the FPVTrackside connector is used).
	RHTimer localRHTimer `json:"rh_timer,omitempty"`

	path string       // where it came from; not serialised
	mu   sync.RWMutex // guards everything above once loaded
}

// defaultLocalConfigPath is the file beside the agent, so a venue that unzips
// dd-pits and runs it finds its settings next to the binary rather than in a
// hidden directory it will never think to back up.
func defaultLocalConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "dd-pits-local.json"
	}
	// Inside the Mac app (dd-pits.app/Contents/MacOS) the folder beside the
	// program is part of the app and is replaced on every update, so the
	// settings, log and data live in Application Support instead.
	if dir := macAppDataDir(exe); dir != "" {
		return filepath.Join(dir, "dd-pits-local.json")
	}
	return filepath.Join(filepath.Dir(exe), "dd-pits-local.json")
}

// loadLocalConfig reads the file, returning an empty config when it does not
// exist yet.
//
// A MISSING file is not an error: a venue's first run has nothing configured,
// and failing to start would be a poor introduction. A malformed file IS an
// error, because silently ignoring it would strand an operator wondering why
// their pilots are not loading.
func loadLocalConfig(path string) (*localConfig, error) {
	if path == "" {
		path = defaultLocalConfigPath()
	}
	cfg := &localConfig{Version: localConfigVersion, path: path}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return cfg, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	cfg.path = path
	if cfg.Version == 0 {
		cfg.Version = localConfigVersion
	}
	return cfg, nil
}

// Save writes the file atomically.
//
// Atomic because the operator may well be editing pilots between heats, and a
// half-written file that fails to parse on the next start is a race day lost.
func (c *localConfig) Save() error {
	c.mu.RLock()
	snapshot := localConfig{
		Version: c.Version, Club: c.Club,
		DefaultLayout: c.DefaultLayout, Pilots: c.Pilots,
		Booth: c.Booth, Calls: c.Calls, ELRS: c.ELRS, Audio: c.Audio,
		Video: c.Video,
	}
	path := c.path
	c.mu.RUnlock()

	data, err := json.MarshalIndent(&snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ELRSConfig projects the local pilots into the shape the ELRS engine already
// consumes.
//
// Deliberately the SAME type the cloud endpoint returns, so the engine, the OSD
// diffing and every test are untouched by where the config came from. The only
// difference between a connected venue and an offline one is which function
// filled this struct in.
func (c *localConfig) ELRSConfig() elrs.ConfigResponse {
	if c == nil {
		return elrs.ConfigResponse{Pilots: map[string]elrs.ConfigPilot{}}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	out := elrs.ConfigResponse{
		Pilots: map[string]elrs.ConfigPilot{},
	}
	if c.DefaultLayout != nil {
		out.DefaultLayout = *c.DefaultLayout
	} else {
		out.DefaultLayout = elrs.DefaultLayout()
	}

	for _, p := range c.Pilots {
		call := strings.ToUpper(strings.TrimSpace(p.Callsign))
		if call == "" {
			continue
		}
		uid := p.resolvedUID()
		if uid == "" {
			continue // nothing to address: no phrase, no uid
		}
		if !p.OSDEnabled {
			// Distinguished from "never set up" so the engine skips them even
			// with callsign-derived-UID fallback on — the same meaning the
			// website's master switch has.
			out.OptOut = append(out.OptOut, call)
			continue
		}
		out.Pilots[call] = elrs.ConfigPilot{UID: uid, Layout: p.Layout}
	}
	sort.Strings(out.OptOut)
	return out
}

// SetPilot adds or replaces a pilot, keyed on callsign.
func (c *localConfig) SetPilot(p localPilot) {
	c.mu.Lock()
	defer c.mu.Unlock()

	p.Callsign = strings.TrimSpace(p.Callsign)
	for i := range c.Pilots {
		if strings.EqualFold(c.Pilots[i].Callsign, p.Callsign) {
			c.Pilots[i] = p
			return
		}
	}
	c.Pilots = append(c.Pilots, p)
}

// RemovePilot drops a pilot by callsign.
func (c *localConfig) RemovePilot(callsign string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	kept := c.Pilots[:0]
	for _, p := range c.Pilots {
		if !strings.EqualFold(p.Callsign, callsign) {
			kept = append(kept, p)
		}
	}
	c.Pilots = kept
}

// PilotList returns a copy, callsign-sorted for a stable page.
func (c *localConfig) PilotList() []localPilot {
	if c == nil {
		return nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	out := append([]localPilot(nil), c.Pilots...)
	sort.Slice(out, func(i, j int) bool {
		return strings.ToUpper(out[i].Callsign) < strings.ToUpper(out[j].Callsign)
	})
	return out
}

// BoothConfig / SetBooth / CallsConfig / SetCalls are the guarded accessors the
// pages use.
func (c *localConfig) BoothConfig() localBooth {
	if c == nil {
		return localBooth{}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Booth
}

func (c *localConfig) SetBooth(b localBooth) {
	c.mu.Lock()
	c.Booth = b
	c.mu.Unlock()
}

func (c *localConfig) AudioSettings() localAudio {
	if c == nil {
		return localAudio{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Audio
}

func (c *localConfig) SetAudio(v localAudio) {
	c.mu.Lock()
	c.Audio = v
	c.mu.Unlock()
}

func (c *localConfig) ELRSSettings() localELRS {
	if c == nil {
		return localELRS{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ELRS
}

func (c *localConfig) SetELRS(v localELRS) {
	c.mu.Lock()
	c.ELRS = v
	c.mu.Unlock()
}

func (c *localConfig) CallsConfig() localCalls {
	if c == nil {
		return localCalls{}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Calls
}

func (c *localConfig) SetCalls(v localCalls) {
	c.mu.Lock()
	c.Calls = v
	c.mu.Unlock()
}

// localRHTimer is the RotorHazard behind FPVTrackside (see RHTimer).
type localRHTimer struct {
	URL string `json:"url,omitempty"`
	Key string `json:"key,omitempty"`
}

func (c *localConfig) RHTimerConfig() localRHTimer {
	if c == nil {
		return localRHTimer{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.RHTimer
}

func (c *localConfig) SetRHTimer(v localRHTimer) {
	c.mu.Lock()
	c.RHTimer = v
	c.mu.Unlock()
}

func (c *localConfig) Path() string {
	if c == nil {
		return ""
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.path
}

// callsSource decides who voices the race calls.
//
// Resolved from CAPABILITY, not just preference: a venue that has configured no
// voice at all must not end up with a silent race day because a stored setting
// said dd-pits was in charge. So "ddpits" requires something that can actually
// speak, and anything else falls back to letting FPVTrackside do what it has
// always done.
//
// hasVoice is whether a TTS engine came up (local model or Qwen key).
func callsSource(stored localCalls, hasVoice bool) string {
	if !hasVoice {
		return "trackside"
	}
	if strings.EqualFold(stored.Source, "trackside") {
		return "trackside"
	}
	return "ddpits"
}

// describeCallsSource is the operator-facing sentence for the setup page and
// the startup log, so the choice is never a mystery.
func describeCallsSource(source string, hasVoice bool) string {
	if source == "trackside" {
		if !hasVoice {
			return timingSystem + " speaks its own race calls — no voice is configured here, " +
				"so dd-pits is leaving the audio alone. The dashboard, overlays and goggles all still work."
		}
		return timingSystem + " speaks its own race calls, by choice. dd-pits is leaving the audio alone."
	}
	if timingSystem == "RotorHazard" {
		return "dd-pits speaks the race calls in its own voice — turn off RotorHazard's voice callouts."
	}
	return "dd-pits speaks the race calls in its own voice — mute FPVTrackside's audio output."
}

// logLocalMode reports what a venue is actually running, once, at startup.
func (c *localConfig) logLocalMode(hasVoice bool) {
	pilots := 0
	for _, p := range c.PilotList() {
		if p.resolvedUID() != "" {
			pilots++
		}
	}
	log.Printf("local setup: OFFLINE mode — %d pilot(s) configured in %s", pilots, c.Path())
	log.Printf("local setup: %s", describeCallsSource(callsSource(c.CallsConfig(), hasVoice), hasVoice))
	if pilots == 0 {
		log.Printf("local setup: no goggle pilots yet — add bind phrases at %s/setup "+
			"to turn on the lap OSD, DVR naming and clock sync", setupBaseURL)
	}
}

// boothEconomyBeats is what a venue gets when it has not chosen for itself:
// the build-up before each heat and the wrap after it, and nothing else.
//
// The old default was EVERYTHING, which on a quiet meeting meant a line every
// ~60 seconds forever plus a mid-heat call every few seconds — each carrying an
// ~800-1250 token prompt. That is a real bill for chatter nobody asked for, and
// the racing calls themselves cost nothing (they are templated from the timing
// data, no model involved), so the expensive part was purely the commentary
// talking to fill space.
//
// Pre/post is the setting a club actually wants when it is paying per token: it
// introduces the heat, it calls the result, and it is silent in between. Any of
// the rest can be switched back on individually — this is the DEFAULT, not a
// restriction.
func boothEconomyBeats() map[string]bool {
	on := map[string]bool{"meeting_open": true, "staged": true, "finish": true}
	all := map[string]bool{}
	for _, k := range boothAllBeatKinds() {
		all[k] = on[k]
	}
	return all
}

// boothAllBeatKinds is every beat the booth can produce.
func boothAllBeatKinds() []string {
	return []string{
		"meeting_open", "staged", "filler",
		"launch", "holeshot", "progress", "meeting_talk",
		"pilot_out", "pilot_back", "qual_move",
		"finish",
	}
}

// macAppDataDir is ~/Library/Application Support/dd-pits when dd-pits runs as
// the Mac app, created if need be; "" otherwise.
func macAppDataDir(exe string) string {
	if !strings.Contains(filepath.ToSlash(exe), ".app/Contents/MacOS/") {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, "Library", "Application Support", "dd-pits")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

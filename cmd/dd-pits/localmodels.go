package main

// The AI models page — what runs where, and one click to set it up.
//
// The commentary has two halves that cost money separately: something WRITES
// the words, and something SAYS them. Each can run in the cloud or on this
// machine, and until now that choice was spread across a free-text model name,
// a "voice engine" dropdown, and two command-line flags — so nobody could tell
// what they had, let alone what it was costing.
//
// The split matters because the two are not remotely equal. Measured against
// the real rates: a session billing 1.7 cents was 1.66c of SPEECH and 0.08c of
// script. The voice is ~95% of the bill. Moving just that onto the venue's own
// Mac removes almost all of the cost, and a club can be told exactly that
// rather than being asked to reason about model names.
//
// Installing is done here too, with progress, because the alternative is a
// terminal and a 4GB download the night before a race day.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/qwentts"
	"github.com/bob9/dd-pits/internal/tts"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// modelSpec is one downloadable model.
type modelSpec struct {
	ID    string // huggingface id
	Label string
	Size  string
	// MinRAMGB is what the machine needs to run it WITHOUT swapping.
	//
	// Enforced rather than advised: swapping does not degrade gently on a race
	// day — model contention has been measured pushing a 2-second lap call out
	// to 12 seconds. Offering a model that cannot fit would be offering someone
	// a broken meeting.
	MinRAMGB int
	Note     string
}

// voiceModel is the local speech model. Small, and the one that matters —
// it removes ~95% of the cost on its own.
var voiceModel = modelSpec{
	ID:       "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-bf16",
	Label:    "Qwen3-TTS (small)",
	Size:     "1.2 GB",
	MinRAMGB: 8,
	Note:     "This is the one worth doing — the voice is about 95% of the bill.",
}

// scriptModels are the local writers, largest first.
var scriptModels = []modelSpec{
	{ID: "mlx-community/Qwen3-30B-A3B-4bit", Label: "Qwen3 30B", Size: "17 GB", MinRAMGB: 32,
		Note: "Best writing. Needs a 32GB machine to stay resident."},
	{ID: "mlx-community/Qwen3-14B-4bit", Label: "Qwen3 14B", Size: "8 GB", MinRAMGB: 24,
		Note: "Good middle ground."},
	{ID: "mlx-community/Qwen3-8B-4bit", Label: "Qwen3 8B", Size: "4.5 GB", MinRAMGB: 16,
		Note: "UNTESTED for race commentary. Small models have a habit of " +
			"attributing a lap time to the wrong pilot, which sounds completely " +
			"convincing and is completely wrong. Check it before trusting it."},

	// Gemma, as an alternative family.
	//
	// Worth offering because the small Gemma is the only writer that sits
	// COMFORTABLY on a 16GB Mac alongside the 1.2GB voice — 2.5GB against the
	// 4.5GB of the smallest Qwen. On a machine also running timing and goggles
	// that headroom is the difference between working and swapping.
	{ID: "mlx-community/gemma-3-27b-it-4bit", Label: "Gemma 3 27B", Size: "16 GB", MinRAMGB: 32,
		Note: "Google's model, similar scale to Qwen3 30B."},
	{ID: "mlx-community/gemma-3-12b-it-4bit", Label: "Gemma 3 12B", Size: "7 GB", MinRAMGB: 24,
		Note: "Google's mid-size model."},
	{ID: "mlx-community/gemma-3-4b-it-4bit", Label: "Gemma 3 4B", Size: "3.4 GB", MinRAMGB: 16,
		Note: "The lightest option, and the most comfortable on a 16GB Mac. " +
			"UNTESTED for race commentary though — check that it attributes lap " +
			"times to the right pilot before trusting it on a race day."},
}

// ── status ──────────────────────────────────────────────────────────────────

type modelState struct {
	Spec       modelSpec
	Downloaded bool
	FitsRAM    bool
}

type machineState struct {
	AppleSilicon bool
	RAMGB        int
	Python       string
	MLXAudio     bool // the voice runtime
	MLXLM        bool // the script runtime
	Voice        modelState
	Scripts      []modelState
}

// venvPython is the interpreter this page installs into.
//
// A dedicated environment beside the agent, never the system Python: a broken
// system pip is a support call nobody wants, and this way removing one folder
// undoes everything.
func venvPython() string {
	return localStateDir() + "/voice-env/bin/python"
}

func localStateDir() string {
	if p := setupConfigPath; p != "" {
		if i := strings.LastIndex(p, "/"); i > 0 {
			return p[:i]
		}
	}
	return "."
}

// setupConfigPath is where the settings file lives, so the environment lands
// beside it rather than in whatever directory the agent happened to start in.
var setupConfigPath string

func probeMachine() machineState {
	m := machineState{
		AppleSilicon: runtimeIsAppleSilicon(),
		RAMGB:        machineRAMGB(),
	}
	m.Python = venvPython()
	if !fileExists(m.Python) {
		m.Python = lookupPython()
	}
	if m.Python != "" {
		m.MLXAudio = pyHasModule(m.Python, "mlx_audio")
		m.MLXLM = pyHasModule(m.Python, "mlx_lm")
	}
	m.Voice = modelState{
		Spec:       voiceModel,
		Downloaded: m.MLXAudio && modelCached(m.Python, voiceModel.ID),
		FitsRAM:    m.RAMGB >= voiceModel.MinRAMGB,
	}
	for _, s := range scriptModels {
		m.Scripts = append(m.Scripts, modelState{
			Spec:       s,
			Downloaded: m.MLXLM && modelCached(m.Python, s.ID),
			FitsRAM:    m.RAMGB >= s.MinRAMGB,
		})
	}
	return m
}

func pyHasModule(py, mod string) bool {
	ctx, cancel := timeoutCtx(6 * time.Second)
	defer cancel()
	return exec.CommandContext(ctx, py, "-c", "import "+mod).Run() == nil
}

// modelCached asks whether the weights are already on disk, so the page can say
// "ready" rather than "probably fine" — and so a race morning never discovers a
// download it needed the night before.
func modelCached(py, id string) bool {
	// Look on disk FIRST, because the Python route needs huggingface_hub and
	// therefore an environment that may not exist.
	//
	// Reported as a bug and it was a real one: 23GB of Qwen3-30B was sitting in
	// the cache while the page offered to download it, because the venv holding
	// huggingface_hub had been cleared and a failed CHECK was being reported as
	// "not downloaded". Those are different facts and only one of them is about
	// the model.
	if cached, known := modelCachedOnDisk(id); known {
		return cached
	}
	return modelCachedViaPython(py, id)
}

// modelCachedOnDisk reads the huggingface cache layout directly.
//
// Returns (result, true) when it can tell, and (_, false) when it cannot — so a
// caller never mistakes "I could not look" for "it is not there".
func modelCachedOnDisk(id string) (cached bool, known bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, false
	}
	root := filepath.Join(home, ".cache", "huggingface", "hub")
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return false, false // no cache at all: cannot conclude anything
	}
	// models--<org>--<name>, the documented layout.
	dir := filepath.Join(root, "models--"+strings.ReplaceAll(id, "/", "--"))
	snaps, err := os.ReadDir(filepath.Join(dir, "snapshots"))
	if err != nil {
		return false, true // the cache exists and this model is not in it
	}
	// A snapshot directory with actual files in it. An interrupted download
	// leaves the folder without the weights, and calling that "downloaded" is
	// how a race morning discovers a model that will not load.
	for _, sn := range snaps {
		if !sn.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, "snapshots", sn.Name()))
		if err == nil && len(files) > 0 {
			return true, true
		}
	}
	return false, true
}

func modelCachedViaPython(py, id string) bool {
	ctx, cancel := timeoutCtx(8 * time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, py, "-c",
		`import sys
from huggingface_hub import snapshot_download
try:
    snapshot_download(sys.argv[1], local_files_only=True)
    print("yes")
except Exception:
    print("no")`, id).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "yes")
}

// ── installing ──────────────────────────────────────────────────────────────

// installJob is one running install, streamed to the page.
type installJob struct {
	mu     sync.Mutex
	lines  []string
	done   bool
	failed bool

	// Progress a human can read, kept apart from the raw output.
	//
	// Streaming pip and tqdm straight onto the page technically showed that
	// something was happening, but it reads as noise — you cannot tell a 4GB
	// download from a stalled one, or how much is left. Step/Total plus a
	// percentage answers "what is it doing and how long" without anyone having
	// to interpret build output.
	step    int
	total   int
	label   string
	percent int    // -1 when not knowable
	modelID string // set on success, so the page can offer to use it
	wantScr bool
}

var (
	installMu   sync.Mutex
	currentJob  *installJob
	installBusy bool
)

func (j *installJob) log(s string) {
	j.mu.Lock()
	j.lines = append(j.lines, s)
	if len(j.lines) > 400 {
		j.lines = j.lines[len(j.lines)-400:]
	}
	j.mu.Unlock()
}

func (j *installJob) setStep(n, total int, label string) {
	j.mu.Lock()
	j.step, j.total, j.label, j.percent = n, total, label, -1
	j.mu.Unlock()
	j.log("▸ " + label)
}

// notePercent pulls a percentage out of a progress line when there is one.
//
// huggingface and pip both emit their own bars; rather than showing those, the
// number is lifted out and the bar is drawn properly on the page.
func (j *installJob) notePercent(line string) {
	m := pctRE.FindStringSubmatch(line)
	if m == nil {
		return
	}
	var v int
	if _, err := fmt.Sscan(m[1], &v); err != nil || v < 0 || v > 100 {
		return
	}
	j.mu.Lock()
	j.percent = v
	j.mu.Unlock()
}

var pctRE = regexp.MustCompile(`(\d{1,3})%\|`)

func (j *installJob) snapshot() ([]string, bool, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := append([]string(nil), j.lines...)
	return out, j.done, j.failed
}

// progress is what the page renders.
func (j *installJob) progress() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	tail := ""
	if n := len(j.lines); n > 0 {
		tail = j.lines[n-1]
	}
	return map[string]any{
		"step": j.step, "total": j.total, "label": j.label,
		"percent": j.percent, "tail": tail,
		"running": !j.done, "failed": j.failed,
		"model": j.modelID, "script": j.wantScr,
	}
}

// runInstall sets up the environment and fetches one model.
//
// Serialised: two concurrent pip installs into one environment corrupt it, and
// the page must never be able to start a second by double-clicking.
func runInstall(modelID string, wantScript bool) (*installJob, error) {
	installMu.Lock()
	defer installMu.Unlock()
	if installBusy {
		return nil, fmt.Errorf("an install is already running")
	}
	py := lookupPython()
	if py == "" {
		return nil, fmt.Errorf("no Python 3 on this Mac — install it with: xcode-select --install")
	}
	installBusy = true
	job := &installJob{}
	currentJob = job

	go func() {
		defer func() {
			installMu.Lock()
			installBusy = false
			installMu.Unlock()
			job.mu.Lock()
			job.done = true
			job.mu.Unlock()
		}()

		venv := localStateDir() + "/voice-env"
		vpy := venv + "/bin/python"

		nstep := 0
		const totalSteps = 4
		step := func(desc string, args ...string) bool {
			nstep++
			job.setStep(nstep, totalSteps, desc)
			ctx, cancel := timeoutCtx(45 * time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			out, _ := cmd.StdoutPipe()
			cmd.Stderr = cmd.Stdout
			if err := cmd.Start(); err != nil {
				job.log("  failed to start: " + err.Error())
				job.failed = true
				return false
			}
			sc := bufio.NewScanner(out)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" {
					continue
				}
				job.notePercent(line)
				job.log("  " + line)
			}
			if err := cmd.Wait(); err != nil {
				job.log("  FAILED: " + err.Error())
				job.failed = true
				return false
			}
			return true
		}

		if !fileExists(vpy) {
			if !step("Creating a self-contained Python environment", py, "-m", "venv", venv) {
				return
			}
		}
		rt := "mlx-audio"
		if wantScript {
			rt = "mlx-lm"
		}
		if !step("Installing the audio software", vpy, "-m", "pip", "install",
			"--upgrade", "pip", "sounddevice", rt) {
			return
		}
		if !step("Downloading the model — the slow part", vpy, "-c",
			`import sys
from huggingface_hub import snapshot_download
snapshot_download(sys.argv[1])
print("downloaded")`, modelID) {
			return
		}
		job.setStep(totalSteps, totalSteps, "Ready")
		job.mu.Lock()
		job.modelID, job.wantScr = modelID, wantScript
		job.percent = 100
		job.mu.Unlock()
		job.log("✓ Installed and ready.")
	}()

	return job, nil
}

// ── the page ────────────────────────────────────────────────────────────────

func (s *liveServer) setupModels(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		switch r.FormValue("action") {
		case "delete":
			// Freeing disk is a legitimate thing to want: these are gigabytes,
			// and a venue that tried a 17GB writer and went back to the cloud
			// should not have to hunt through a hidden cache directory to
			// reclaim it.
			if err := deleteModel(r.FormValue("model")); err != nil {
				http.Redirect(w, r, "/setup/models?err="+urlEscape(err.Error()), http.StatusSeeOther)
				return
			}
			// A model that was IN USE cannot stay selected once its files are
			// gone, or the booth would start up pointing at nothing.
			if s.cfg != nil {
				bc := s.cfg.BoothConfig()
				if bc.ScriptModel == r.FormValue("model") {
					bc.ScriptModel = ""
					s.cfg.SetBooth(bc)
					_ = s.cfg.Save()
				}
			}
			http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
			return
		case "use":
			// Pick which downloaded model writes the words.
			if s.cfg != nil {
				bc := s.cfg.BoothConfig()
				id := r.FormValue("model")
				if id == "cloud" {
					bc.ScriptModel = ""
				} else {
					bc.ScriptModel = id
					bc.VoicePython = venvPython()
				}
				s.cfg.SetBooth(bc)
				_ = s.cfg.Save()
				if s.applyBoothConfig != nil {
					s.applyBoothConfig()
				}
			}
			http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
			return
		case "install":
			id := r.FormValue("model")
			want := r.FormValue("kind") == "script"
			if _, err := runInstall(id, want); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case "usevoice":
			// Switch which recording is the live one. The files stay put — a club
			// with two commentators swaps between them, it does not re-upload.
			if s.cfg != nil {
				bc := s.cfg.BoothConfig()
				want := r.FormValue("path")
				for _, v := range bc.VoiceRefs {
					if v.Path == want {
						bc.VoiceRef, bc.VoiceRefText = v.Path, v.Text
					}
				}
				if want == "" { // back to the model's own voice
					bc.VoiceRef, bc.VoiceRefText = "", ""
				}
				s.cfg.SetBooth(bc)
				_ = s.cfg.Save()
				if s.applyBoothConfig != nil {
					s.applyBoothConfig()
				}
			}
			http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
			return
		case "voices":
			// Voices live with the models now, not with what the booth talks
			// about. Splitting them across two pages meant the thing you were
			// choosing (a voice) and the thing that produces it (a model) were
			// configured in different places.
			if s.cfg != nil {
				bc := s.cfg.BoothConfig()
				// The VOICE IDENTITY is baked into engines at startup — the
				// announcer's local engine holds its reference recording, the
				// cloud path its captured voice — so changing it and hot-applying
				// reaches the booth but NOT the calls, and the operator hears
				// the old voice with the page showing the new one. A change to
				// any of these restarts the agent (the spawn restart, proven by
				// the who-speaks switch) so every consumer rebuilds from the
				// saved config and there is exactly one voice in the building.
				wasVoice := bc.Engine + "|" + bc.VoiceRef + "|" + bc.VoiceRefText + "|" + bc.VoicePBP
				bc.Mode = r.FormValue("mode")
				// A "ref:" selection is one of the venue's own recordings, which is
				// the LOCAL voice — not a cloud voice name. Listing both in one
				// picker is the only arrangement that is not confusing; routing
				// them to the same field would not work, since they are different
				// mechanisms entirely.
				pbp := strings.TrimSpace(r.FormValue("voice_pbp"))
				if strings.HasPrefix(pbp, "ref:") {
					want := strings.TrimPrefix(pbp, "ref:")
					for _, v := range bc.VoiceRefs {
						if v.Path == want {
							bc.VoiceRef, bc.VoiceRefText = v.Path, v.Text
							bc.Engine = "local" // a recording only means anything locally
						}
					}
				} else if pbp != "" {
					// The symmetric half, and it was missing: choosing a CLOUD
					// voice must flip the engine to the cloud, exactly as
					// choosing a recording flips it local above. Without this
					// the name was stored, the engine stayed local, and the
					// page — correctly showing that a local engine means the
					// recording is the active voice — appeared to reset the
					// choice on every save.
					bc.VoicePBP = pbp
					bc.Engine = "qwen"
				}
				bc.VoiceColor = strings.TrimSpace(r.FormValue("voice_color"))
				bc.Language = strings.TrimSpace(r.FormValue("language"))
				bc.Accent = boothpkg.SanitiseAccent(r.FormValue("accent"))
				bc.Energy = strings.TrimSpace(r.FormValue("energy"))
				bc.Theme = strings.TrimSpace(r.FormValue("theme"))
				bc.Humour = strings.TrimSpace(r.FormValue("humour"))
				bc.Style = strings.TrimSpace(r.FormValue("style"))
				s.cfg.SetBooth(bc)
				_ = s.cfg.Save()
				if s.applyBoothConfig != nil {
					s.applyBoothConfig()
				}
				if now := bc.Engine + "|" + bc.VoiceRef + "|" + bc.VoiceRefText + "|" + bc.VoicePBP; now != wasVoice {
					log.Printf("voices: the voice changed — restarting so calls and commentary both use it")
					s.restartToApply(w, "Saved — restarting so the new voice is used everywhere",
						"The race calls build their voice engine at startup, so a voice change needs "+
							"the restart to reach them as well as the commentary. This page will come back by itself.")
					return
				}
			}
			http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
			return
		case "voice_design":
			// DESIGN, from one of the venue's OWN voices — never a built-in.
			//
			// There is no separate design API: an instructed render IS the
			// design step. The base must be a voice that takes instructions,
			// which means a CLOUD clone (the VC model accepts them; the local
			// model takes words only) — so a raw recording is cloned first,
			// automatically, and the character is then rendered on top of it.
			// The result auditioned here is byte-for-byte what "keep" clones.
			if s.cfg == nil {
				jsonError(w, http.StatusServiceUnavailable, "no settings loaded")
				return
			}
			bc := s.cfg.BoothConfig()
			if strings.TrimSpace(bc.QwenKey) == "" {
				jsonError(w, http.StatusBadRequest, "voice design runs on the cloud — save an API key first")
				return
			}
			base := strings.TrimSpace(r.FormValue("design_base"))
			desc := strings.TrimSpace(r.FormValue("design_desc"))
			name := strings.TrimSpace(r.FormValue("design_name"))
			if base == "" || desc == "" || name == "" {
				jsonError(w, http.StatusBadRequest, "pick a base voice, describe the character, and name it")
				return
			}
			qc, err := qwentts.New(qwentts.Config{APIKey: bc.QwenKey, BaseURL: bc.QwenBase})
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			ctx, cancel := timeoutCtx(4 * time.Minute)
			defer cancel()
			if strings.HasPrefix(base, "ref:") {
				// A recording: clone it raw so the design has an instructable base.
				wav, rerr := os.ReadFile(strings.TrimPrefix(base, "ref:"))
				if rerr != nil {
					jsonError(w, http.StatusBadRequest, "that recording could not be read: "+rerr.Error())
					return
				}
				id, eerr := qc.EnrollVoice(ctx, name+"base", wav)
				if eerr != nil {
					jsonError(w, http.StatusInternalServerError, "cloning the recording failed: "+eerr.Error())
					return
				}
				log.Printf("voice design: recording cloned as %s to serve as the design base", id)
				base = id
			}
			eng := tts.FromQwen(qc)
			pcm, rate, err := eng.Speak(ctx, tts.Request{
				Text:  designParagraph,
				Voice: base,
				Instruction: "Perform this read fully in character: " + desc + ". " +
					"Keep the same underlying voice — this is that person performing the character. " +
					"Read the text exactly as written.",
			})
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if rate <= 0 {
				rate = boothSampleRate
			}
			wavOut := wavFromPCM(pcm, rate)
			s.designMu.Lock()
			s.designWav, s.designName = wavOut, name
			s.designMu.Unlock()
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(wavOut)
			return

		case "voice_design_keep":
			// CLONE the sample that was just auditioned — that exact audio.
			s.designMu.Lock()
			wav, name := s.designWav, s.designName
			s.designMu.Unlock()
			if len(wav) == 0 {
				jsonError(w, http.StatusBadRequest, "generate a sample first — keep clones what you heard")
				return
			}
			bc2 := s.cfg.BoothConfig()
			qc2, err := qwentts.New(qwentts.Config{APIKey: bc2.QwenKey, BaseURL: bc2.QwenBase})
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			ctx2, cancel2 := timeoutCtx(2 * time.Minute)
			defer cancel2()
			id, err := qc2.EnrollVoice(ctx2, name, wav)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			log.Printf("voice design: %q kept as %s — pick it as a voice and save to use it", name, id)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "voice": id})
			return

		case "clone":
			// The cloned-voice fields lost their home when the two pages were
			// separated: the agent still read them and the config still held
			// them, but nothing could set them. They belong here, beside the
			// local voice model they configure.
			if s.cfg != nil {
				bc := s.cfg.BoothConfig()
				bc.VoiceRef = strings.TrimSpace(r.FormValue("voice_ref"))
				bc.VoiceRefText = strings.TrimSpace(r.FormValue("voice_ref_text"))
				s.cfg.SetBooth(bc)
				_ = s.cfg.Save()
				if s.applyBoothConfig != nil {
					s.applyBoothConfig()
				}
			}
			http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
			return
		case "pronounce":
			// A pronunciation edit reaches the very next call: the rules live
			// in a package-level atomic that sayable() reads at the TTS
			// boundary, so no engine restart is involved.
			if s.cfg != nil {
				bc := s.cfg.BoothConfig()
				bc.Pronounce = strings.TrimSpace(r.FormValue("pronounce"))
				s.cfg.SetBooth(bc)
				_ = s.cfg.Save()
				setPronunciations(bc.Pronounce)
			}
			http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
			return
		case "key":
			// The API key belongs here too.
			//
			// It lives on the Commentary page, but a setup of "voice on this Mac,
			// words from the cloud" still needs one for the words — and sending
			// someone to a different page to finish the thing they just chose
			// here is how a half-configured booth happens.
			if s.cfg != nil {
				bc := s.cfg.BoothConfig()
				if k := strings.TrimSpace(r.FormValue("qwen_key")); k != "" {
					bc.QwenKey = k
				}
				if r.FormValue("qwen_key_clear") == "1" {
					bc.QwenKey = ""
				}
				s.cfg.SetBooth(bc)
				_ = s.cfg.Save()
				if s.applyBoothConfig != nil {
					s.applyBoothConfig()
				}
			}
			http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
			return
		case "preset":
			s.applyPreset(w, r)
			return
		case "choose":
			s.saveModelChoice(r)
			http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
			return
		}
	}
	m := probeMachine()
	writeSetup(w, "AI models", modelsPageHTML(s, m, r.URL.Query().Get("saved") != ""))
}

// saveModelChoice writes the cloud/local decision for each half.
func (s *liveServer) saveModelChoice(r *http.Request) {
	if s.cfg == nil {
		return
	}
	bc := s.cfg.BoothConfig()
	// A setting is only written when the posting form actually CARRIED it.
	//
	// The else-branches used to write the cloud default, which meant any save
	// that omitted voice_where — a different section of the page, a script-only
	// form, a hand-rolled POST — silently flipped the voice to the cloud. Lived
	// experience of that: "it keeps changing the audio from the cloud". The
	// operator's choice was being overwritten by forms that never asked about
	// the voice at all.
	//
	// Same bug, same fix as processPilotProfileUpdate presence-guarding
	// discord_user_id: absent-from-form means "no opinion", never "the default".
	if v := r.FormValue("voice_where"); v != "" {
		// The VOICE. "local" means MLX on this machine; "qwen" means the cloud.
		if v == "local" {
			bc.Engine = "local"
		} else {
			bc.Engine = "qwen"
		}
	}
	if sw := r.FormValue("script_where"); sw != "" {
		// The WORDS. A local script model set means local; empty means cloud.
		if sw == "local" {
			bc.ScriptModel = strings.TrimSpace(r.FormValue("script_model"))
		} else {
			bc.ScriptModel = ""
		}
	}
	if r.FormValue("voice_where") == "local" || r.FormValue("script_where") == "local" {
		bc.VoicePython = venvPython() // where this page installs to
	}
	s.cfg.SetBooth(bc)
	_ = s.cfg.Save()
	if s.applyBoothConfig != nil {
		s.applyBoothConfig()
	}
}

// installStatus streams the running install to the page.
func (s *liveServer) installStatus(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	installMu.Lock()
	job := currentJob
	installMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if job == nil {
		_, _ = w.Write([]byte(`{"running":false,"lines":[]}`))
		return
	}
	lines, _, _ := job.snapshot()
	out := job.progress()
	out["lines"] = lines
	_ = json.NewEncoder(w).Encode(out)
}

func timeoutCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// runtimeIsAppleSilicon gates every local-model option: MLX exists only here.
func runtimeIsAppleSilicon() bool {
	return runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
}

// machineRAMGB is what decides which models are offered at all — a model that
// does not fit is not a slower option, it is a broken race day.
func machineRAMGB() int {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	var b int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &b); err != nil {
		return 0
	}
	return int(b / (1 << 30))
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// modelsPageHTML renders the page.
//
// Built around the one thing a club actually needs to decide — where each half
// runs — with the cost consequence stated next to it, because "Qwen3-30B-A3B"
// tells a race director nothing and "saves about 95%" tells them everything.
func modelsPageHTML(s *liveServer, m machineState, saved bool) string {
	bc := localBooth{}
	if s != nil && s.cfg != nil {
		bc = s.cfg.BoothConfig()
	}

	var b strings.Builder
	if saved {
		b.WriteString(`<div class="card" style="border-left:3px solid var(--ok,#7c5)">
 <strong>Saved.</strong> <span class="mut">In effect now — no restart needed.</span></div>`)
	}

	b.WriteString(presetCardsHTML(m, bc))
	b.WriteString(s.runningPanelHTML())
	b.WriteString(s.writerLoadHTML())
	b.WriteString(cloudKeyPanelHTML(bc))
	b.WriteString(voicesPanelHTML(bc))
	b.WriteString(actionsPanelHTML(m, bc))
	b.WriteString(scriptTestPanelHTML(m, bc))

	// What this machine can do. Below the presets, because most people never
	// need to read it — the presets already account for it.
	b.WriteString(`<div class="card"><h2>This Mac</h2><table style="font-size:.9rem">`)
	row := func(ok bool, label, detail string) {
		mark, col := "✗", "var(--warn,#c88)"
		if ok {
			mark, col = "✓", "var(--ok,#7c5)"
		}
		fmt.Fprintf(&b, `<tr><td style="color:%s;font-weight:700;width:1.5rem">%s</td>`+
			`<td style="white-space:nowrap"><strong>%s</strong></td>`+
			`<td class="mut" style="padding-left:.6rem">%s</td></tr>`, col, mark, label, detail)
	}
	if m.AppleSilicon {
		row(true, "Apple Silicon", "Can run models locally.")
	} else {
		row(false, "Not Apple Silicon",
			"Local models need an M1 or later. Use the cloud for both — and save money "+
				"instead by turning off between-heats chatter on the Commentary page.")
	}
	row(m.RAMGB >= 16, fmt.Sprintf("%d GB memory", m.RAMGB), ramAdvice(m.RAMGB))
	row(m.Python != "", "Python", pythonAdvice(m.Python))
	b.WriteString(`</table>`)

	// What is on disk, at TOP LEVEL.
	//
	// This used to live only inside the "choose each half myself" section, which
	// is collapsed — so answering "are my models downloaded?" meant opening an
	// advanced panel you had no reason to open. It is the first thing someone
	// wants to know, and the one thing that decides whether a race morning is
	// the first time they discover a missing 4GB download.
	b.WriteString(`<h3 style="margin-top:1rem">Models on this Mac</h3>
 <p class="mut" style="font-size:.83rem;margin:.1rem 0 .5rem"><strong>Qwen3-TTS</strong> is the
 text-to-speech — the voice that reads the commentary aloud. The <strong>Qwen3</strong> models
 below it are language models that write what to say.</p>
 <table style="font-size:.88rem">`)
	inUse := strings.TrimSpace(bc.ScriptModel)
	modelLine := func(st modelState, isVoice bool) {
		switch {
		case !st.FitsRAM:
			fmt.Fprintf(&b, `<tr><td style="width:1.5rem;color:var(--muted)">–</td>`+
				`<td style="white-space:nowrap">%s</td>`+
				`<td class="mut" style="padding-left:.7rem">too large for this Mac — needs %d GB</td>`+
				`<td></td></tr>`,
				template.HTMLEscapeString(st.Spec.Label), st.Spec.MinRAMGB)
		case st.Downloaded:
			fmt.Fprintf(&b, `<tr><td style="color:var(--ok,#7c5);font-weight:700">✓</td>`+
				`<td style="white-space:nowrap"><strong>%s</strong></td>`+
				`<td style="padding-left:.7rem">downloaded (%s)</td>`+
				`<td style="padding-left:.7rem;white-space:nowrap">%s</td></tr>`,
				template.HTMLEscapeString(st.Spec.Label), st.Spec.Size,
				modelActionsHTML(st, st.Spec.ID == inUse, isVoice))
		default:
			fmt.Fprintf(&b, `<tr><td style="color:var(--muted)">○</td>`+
				`<td style="white-space:nowrap">%s</td>`+
				`<td class="mut" style="padding-left:.7rem">not downloaded (%s)</td>`+
				`<td style="padding-left:.7rem;white-space:nowrap">%s</td></tr>`,
				template.HTMLEscapeString(st.Spec.Label), st.Spec.Size,
				modelActionsHTML(st, false, isVoice))
		}
	}
	modelLine(m.Voice, true)
	for _, st := range m.Scripts {
		modelLine(st, false)
	}
	// Where the VOICE runs, as one sentence with one button — not a radio grid.
	voiceOn, wantVoice, voiceBtn := "in the cloud", "local", "Move it to this Mac"
	if bc.Engine == "local" {
		voiceOn, wantVoice, voiceBtn = "on this Mac", "cloud", "Move it to the cloud"
	}
	scriptWhere := "cloud"
	if inUse != "" {
		scriptWhere = "local"
	}
	fmt.Fprintf(&b, `<tr><td></td><td colspan="3" style="padding-top:.6rem">
 The voice runs <strong>%s</strong> &mdash; that is about 95%% of the cost.
 <form method="post" style="display:inline;margin:0">
 <input type="hidden" name="action" value="choose">
 <input type="hidden" name="voice_where" value="%s">
 <input type="hidden" name="script_where" value="%s">
 <input type="hidden" name="script_model" value="%s">
 <button type="submit" style="padding:.15rem .5rem;border:1px solid var(--line);
   border-radius:6px;font-size:.74rem;background:transparent;color:var(--muted);
   cursor:pointer">%s</button></form></td></tr>`,
		voiceOn, wantVoice, scriptWhere, template.HTMLEscapeString(inUse), voiceBtn)

	if inUse == "" {
		b.WriteString(`<tr><td></td><td colspan="3" class="mut">
 The words are written <strong>in the cloud</strong> &mdash; about 5%. Download a
 writer above to move them here.</td></tr>`)
	} else {
		b.WriteString(`<tr><td></td><td colspan="3" class="mut" style="padding-top:.4rem">
 The words are written on this Mac. <form method="post" style="display:inline;margin:0">
 <input type="hidden" name="action" value="use"><input type="hidden" name="model" value="cloud">
 <button type="submit" style="padding:.15rem .5rem;border:1px solid var(--line);border-radius:6px;
   font-size:.74rem;background:transparent;color:var(--muted);cursor:pointer">
   Go back to the cloud</button></form></td></tr>`)
	}
	b.WriteString(cloneVoiceHTML(bc))
	b.WriteString(`<p class="mut" style="font-size:.8rem">Checked against the disk, not remembered — so it stays
 right if you move your settings to another machine. Nothing is downloaded until you ask.</p>
</div>`)

	// The per-half controls, for anyone who wants to differ from the presets.
	// The install console.
	//
	// A bar and a sentence, with the build output folded away behind a toggle.
	// Streaming raw pip onto the page did prove something was happening, but it
	// reads as noise: you cannot tell a large download from a stalled one, and
	// it looks alarming to someone who just wanted commentary.
	b.WriteString(`<div class="card" id="ic" style="display:none">
 <h2 id="ih">Install progress</h2>
 <div style="background:var(--bg2,#111);border-radius:99px;height:12px;overflow:hidden;margin:.6rem 0">
   <div id="ib" style="height:100%;width:0;background:var(--accent,#b6e925);
        transition:width .4s ease"></div></div>
 <p id="istep" style="margin:.3rem 0;font-weight:600"></p>
 <p class="mut" id="itail" style="margin:.2rem 0;font-size:.82rem;
    white-space:nowrap;overflow:hidden;text-overflow:ellipsis"></p>
 <p class="mut" style="font-size:.82rem">Safe to leave this page open. A large model can
  take a while on a slow connection — it is a one-off, and it is cached afterwards.</p>
 <details style="margin-top:.5rem"><summary class="mut" style="cursor:pointer;font-size:.85rem">
   Show the technical detail</summary>
  <pre id="il" style="max-height:18rem;overflow:auto;background:var(--bg2,#111);
    padding:.7rem;border-radius:8px;font-size:.74rem;white-space:pre-wrap"></pre></details>
 <div id="idone" style="display:none;margin-top:.8rem;padding-top:.8rem;
      border-top:1px solid var(--line)">
  <p style="color:var(--ok,#7c5);font-weight:600;margin:0 0 .5rem">✓ Installed.</p>
  <form method="post" style="margin:0"><input type="hidden" name="action" value="choose">
   <input type="hidden" name="voice_where" id="dv" value="local">
   <input type="hidden" name="script_where" id="ds" value="cloud">
   <input type="hidden" name="script_model" id="dm" value="">
   <button type="submit" style="padding:.5rem 1rem;border:0;border-radius:8px;font-weight:600;
     background:var(--accent,#b6e925);color:#111;cursor:pointer">Use it &mdash; and save</button>
   <span class="mut" style="margin-left:.6rem">Then quit dd-pits and start it again.</span>
  </form></div></div>`)

	b.WriteString(modelsPageScript())
	b.WriteString(actionsScript())
	b.WriteString(writerLoadScript())
	return b.String()
}

func ramAdvice(gb int) string {
	switch {
	case gb >= 32:
		return "Enough for the voice and any writer, locally."
	case gb >= 24:
		return "Enough for the voice and a mid-size writer."
	case gb >= 16:
		return "Enough for the voice. Keep the writing on the cloud — it is the cheap part."
	default:
		return "Tight. Use the cloud for both."
	}
}

func pythonAdvice(py string) string {
	if py == "" {
		return `Needed for local models. Install with <code>xcode-select --install</code>.`
	}
	return "Found at <code>" + template.HTMLEscapeString(py) + "</code>"
}

func voiceAvailability(m machineState) string {
	switch {
	case !m.AppleSilicon:
		return "Needs Apple Silicon."
	case m.Voice.Downloaded:
		return "Ready. Costs nothing to run."
	default:
		return "Needs a 1.2 GB download — one click below."
	}
}

func scriptAvailability(m machineState) string {
	if !m.AppleSilicon {
		return "Needs Apple Silicon."
	}
	if !anyScriptFits(m) {
		return fmt.Sprintf("This Mac has %d GB — not enough to hold a writer without swapping.", m.RAMGB)
	}
	return "Pick a model below."
}

func anyScriptFits(m machineState) bool {
	for _, s := range m.Scripts {
		if s.FitsRAM {
			return true
		}
	}
	return false
}

// choiceRow is one radio option with its consequence spelled out.
func choiceRow(name, val string, checked bool, label, detail string, enabled bool) string {
	dis, style := "", ""
	if !enabled {
		dis, style = " disabled", "opacity:.5"
	}
	ck := ""
	if checked {
		ck = " checked"
	}
	return fmt.Sprintf(`<label style="display:block;margin:.45rem 0;%s">
 <input type="radio" name="%s" value="%s"%s%s style="width:auto;margin-right:.5rem">
 <strong>%s</strong> <span class="mut">— %s</span></label>`,
		style, name, val, ck, dis, label, detail)
}

// modelRow shows a model's state and, when it is missing, the button that fixes it.
func modelRow(st modelState, kind string) string {
	if !st.FitsRAM {
		return fmt.Sprintf(`<p class="mut" style="margin-left:1.6rem">%s (%s) needs %d GB.</p>`,
			st.Spec.Label, st.Spec.Size, st.Spec.MinRAMGB)
	}
	if st.Downloaded {
		return fmt.Sprintf(`<p style="margin-left:1.6rem;color:var(--ok,#7c5)">✓ %s installed (%s)</p>`,
			st.Spec.Label, st.Spec.Size)
	}
	return fmt.Sprintf(`<p style="margin-left:1.6rem">
 <button type="button" class="dl" data-model="%s" data-kind="%s"
   style="padding:.4rem .8rem;border:0;border-radius:7px;font-weight:600;
          background:var(--accent,#b6e925);color:#111;cursor:pointer">⬇ Install %s (%s)</button>
 <span class="mut" style="margin-left:.5rem">%s</span></p>`,
		template.HTMLEscapeString(st.Spec.ID), kind,
		template.HTMLEscapeString(st.Spec.Label), st.Spec.Size,
		template.HTMLEscapeString(st.Spec.Note))
}

func scriptOption(st modelState, chosen string) string {
	if !st.FitsRAM {
		return fmt.Sprintf(`<p class="mut">%s (%s) — needs a %d GB Mac.</p>`,
			st.Spec.Label, st.Spec.Size, st.Spec.MinRAMGB)
	}
	ck := ""
	if chosen == st.Spec.ID {
		ck = " checked"
	}
	status := modelRow(st, "script")
	return fmt.Sprintf(`<label style="display:block;margin:.4rem 0">
 <input type="radio" name="script_model" value="%s"%s style="width:auto;margin-right:.5rem">
 <strong>%s</strong> <span class="mut">%s — %s</span></label>%s`,
		template.HTMLEscapeString(st.Spec.ID), ck,
		template.HTMLEscapeString(st.Spec.Label), st.Spec.Size,
		template.HTMLEscapeString(st.Spec.Note), status)
}

func modelsPageScript() string {
	return `<script>
(function(){
 var box=document.getElementById('ic'), log=document.getElementById('il'), timer=null;
 // sawRunning is what stops an infinite reload loop.
 //
 // The status endpoint reports the LAST job forever, so a fresh page load saw a
 // finished install, reloaded to pick up the result, saw the same finished
 // install again, and reloaded — endlessly. The reload is only correct on a
 // transition this page actually WATCHED: running, then not.
 var sawRunning=false;
 function poll(){
   fetch('/setup/models/status').then(function(r){return r.json();}).then(function(j){
     if(j.lines && j.lines.length){
       box.style.display='';
       var atEnd = log.scrollTop + log.clientHeight >= log.scrollHeight - 30;
       log.textContent = j.lines.join('\n');
       if(atEnd) log.scrollTop = log.scrollHeight;
     }
     var h=document.getElementById('ih');
     if(h) h.textContent = j.running ? 'Installing\u2026'
            : (j.failed ? 'Install failed' : 'Finished');
     if(j.running===true){ sawRunning=true; }
     if(j.running===false && sawRunning){
       sawRunning=false;
       if(timer){ clearInterval(timer); timer=null; }
       // Reloaded ONCE, so the page re-checks what is on disk rather than
       // claiming success on its own say-so.
       setTimeout(function(){ location.reload(); }, 1200);
     } else if(j.running===false && timer && (!j.lines || !j.lines.length)){
       clearInterval(timer); timer=null;   // nothing running, nothing to show
     }
   }).catch(function(){});
 }
 document.querySelectorAll('.dl').forEach(function(btn){
   btn.addEventListener('click', function(){
     btn.disabled=true; btn.textContent='Installing…';
     var body='action=install&model='+encodeURIComponent(btn.dataset.model)+
              '&kind='+encodeURIComponent(btn.dataset.kind);
     fetch('/setup/models',{method:'POST',
       headers:{'Content-Type':'application/x-www-form-urlencoded'},body:body})
       .then(function(){ box.style.display=''; if(!timer) timer=setInterval(poll,1000); poll(); })
       .catch(function(){ btn.disabled=false; btn.textContent='Try again'; });
   });
 });
 // An install already running when the page opens keeps reporting.
 poll(); timer=setInterval(poll,1500);
})();
</script>`
}

// ── one-click setups ────────────────────────────────────────────────────────
//
// Three named outcomes instead of a grid of model names.
//
// The per-half controls are still there for anyone who wants them, but nobody
// should have to hold "Qwen3-30B-A3B-4bit" in their head to answer the only
// question they actually have, which is "how do I stop this costing money".
// Each of these installs whatever it needs AND writes the settings, in one
// press.

type presetSpec struct {
	ID, Title, Cost, Detail string
	VoiceLocal, ScriptLocal bool
}

func presetsFor(m machineState) []presetSpec {
	out := []presetSpec{
		{ID: "cheap", Title: "Voice on this Mac, words from the cloud",
			Cost:       "Cheapest sensible option — removes about 95% of the cost",
			Detail:     "A 1.2 GB one-off download. Recommended for almost everyone.",
			VoiceLocal: true},
		{ID: "cloud", Title: "Everything from the cloud",
			Cost:   "Costs the most, needs nothing installed",
			Detail: "Works on any Mac, and needs a working internet connection at the track.",
		},
	}
	// Only offered when the machine can genuinely hold both, because a model
	// that swaps turns a 2-second lap call into a 12-second one.
	if m.AppleSilicon && anyScriptFits(m) {
		out = append(out, presetSpec{ID: "alllocal",
			Title:      "Everything on this Mac",
			Cost:       "Free to run, and works with no internet at all",
			Detail:     "A larger download. Uses this Mac's GPU, shared with the timing.",
			VoiceLocal: true, ScriptLocal: true})
	}
	return out
}

// applyPreset installs anything missing, then writes the settings.
func (s *liveServer) applyPreset(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("preset")
	m := probeMachine()
	var want presetSpec
	for _, p := range presetsFor(m) {
		if p.ID == id {
			want = p
		}
	}
	if want.ID == "" {
		http.Error(w, "unknown setup", http.StatusBadRequest)
		return
	}

	// Pick the biggest local writer this machine can actually hold.
	scriptID := ""
	if want.ScriptLocal {
		for _, st := range m.Scripts {
			if st.FitsRAM {
				scriptID = st.Spec.ID
				break
			}
		}
		// Nothing fits — but the operator asked for all-local, so fall back to
		// the SMALLEST writer rather than silently saving a cloud setup.
		//
		// This is what "it keeps losing its config" was: FitsRAM is judged
		// against free memory, and on a machine with other things open nothing
		// qualified. scriptID stayed empty, empty means cloud, and the page then
		// honestly reported the cloud setup it had been given. The choice was
		// never stored, so re-picking it changed nothing and it looked like the
		// setting would not stick.
		//
		// A tight fit is the operator's call to make: they can see the RAM
		// warning on the page. Quietly overriding them is not.
		if scriptID == "" && len(m.Scripts) > 0 {
			smallest := m.Scripts[len(m.Scripts)-1]
			for _, st := range m.Scripts {
				if st.Spec.MinRAMGB > 0 && st.Spec.MinRAMGB < smallest.Spec.MinRAMGB {
					smallest = st
				}
			}
			scriptID = smallest.Spec.ID
			log.Printf("models: no writer fits comfortably in free memory — using the "+
				"smallest (%s) because all-local was asked for; close some apps if it swaps",
				smallest.Spec.ID)
		}
	}

	// Write the settings FIRST, so the choice is recorded even if a download
	// later fails — a half-finished install should leave the venue with a
	// working cloud setup, not an unusable one.
	if s.cfg != nil {
		bc := s.cfg.BoothConfig()
		bc.Engine = "qwen"
		if want.VoiceLocal {
			bc.Engine = "local"
		}
		bc.ScriptModel = scriptID
		// Pointed at the environment the install CREATES, not at one that
		// already exists.
		//
		// The settings are written before the download on purpose, so a failed
		// fetch leaves a working cloud setup — which meant this test was always
		// false on a first run and the venue ended up with a model installed and
		// nothing configured to run it.
		if want.VoiceLocal {
			bc.VoicePython = venvPython()
		}
		s.cfg.SetBooth(bc)
		_ = s.cfg.Save()
		if s.applyBoothConfig != nil {
			s.applyBoothConfig()
		}
	}

	// Then fetch whatever is missing. Voice first: it is the one that matters.
	switch {
	case want.VoiceLocal && !m.Voice.Downloaded:
		if _, err := runInstall(voiceModel.ID, false); err != nil {
			http.Redirect(w, r, "/setup/models?err="+urlEscape(err.Error()), http.StatusSeeOther)
			return
		}
	case scriptID != "":
		if _, err := runInstall(scriptID, true); err != nil {
			http.Redirect(w, r, "/setup/models?err="+urlEscape(err.Error()), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/setup/models?saved=1", http.StatusSeeOther)
}

func urlEscape(s string) string {
	return strings.NewReplacer(" ", "%20", "\n", " ", "&", "%26").Replace(s)
}

// presetCardsHTML is the top of the page: read three, press one.
func presetCardsHTML(m machineState, bc localBooth) string {
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Set it up in one click</h2>
 <p class="mut">Commentary has two halves. <strong>Qwen3-TTS</strong> is the text-to-speech
 that SAYS the words — that is about <strong>95% of the cost</strong>, and it can run on this
 Mac for nothing. A <strong>Qwen3</strong> language model WRITES them, which is the cheap
 part. Pick a setup and everything is installed and configured for you.</p>`)

	// Say what is active, ALWAYS — matching a preset or not.
	//
	// The badge alone was not enough: voice-from-the-cloud with words-on-this-Mac
	// is a perfectly valid setup that matches none of the three, so the page
	// highlighted nothing and looked as though none was in use. A combination the
	// presets do not happen to name is not the same as no setup at all.
	var cur string
	switch {
	case bc.Engine == "":
		cur = "Commentary is <strong>switched off</strong>."
	default:
		voice := "from the cloud"
		if bc.Engine == "local" {
			voice = "on this Mac"
		}
		words := "from the cloud"
		if m := strings.TrimSpace(bc.ScriptModel); m != "" {
			words = "on this Mac"
			for _, sp := range scriptModels {
				if sp.ID == m {
					words = "on this Mac (" + sp.Label + ")"
				}
			}
		}
		cur = fmt.Sprintf("Voice <strong>%s</strong>, words <strong>%s</strong>.", voice, words)
	}
	named := false
	for _, p := range presetsFor(m) {
		if p.VoiceLocal == (bc.Engine == "local") &&
			p.ScriptLocal == (strings.TrimSpace(bc.ScriptModel) != "") && bc.Engine != "" {
			named = true
		}
	}
	tail := ""
	if !named && bc.Engine != "" {
		tail = ` <span class="mut">This is a custom mix — none of the three below matches it,
 which is fine. Press one to change to it.</span>`
	}
	fmt.Fprintf(&b, `<div style="padding:.6rem .8rem;border-left:3px solid var(--accent,#b6e925);
 background:rgba(255,255,255,.03);border-radius:0 8px 8px 0;margin:.2rem 0 .9rem">
 <div style="font-size:.78rem;letter-spacing:.05em;color:var(--muted)">RUNNING NOW</div>
 <div>%s</div>%s</div>`, cur, tail)

	for _, p := range presetsFor(m) {
		voiceLocal, scriptLocal := bc.Engine == "local", strings.TrimSpace(bc.ScriptModel) != ""
		active := p.VoiceLocal == voiceLocal && p.ScriptLocal == scriptLocal && bc.Engine != ""
		border, badge := "1px solid var(--line)", ""
		if active {
			// A tinted panel as well as a badge: a 2px border alone is easy to
			// miss on a dark page, and "which one am I on" is the first question
			// anyone opening this page has.
			border = "2px solid var(--ok,#7c5);background:rgba(124,197,85,.07)"
			badge = ` <span style="font-size:.72rem;background:var(--ok,#7c5);color:#111;
                 padding:.1rem .4rem;border-radius:99px;vertical-align:middle">&#10003; IN USE NOW</span>`
		}
		rec := ""
		if p.ID == "cheap" {
			rec = ` <span style="font-size:.72rem;background:var(--accent,#b6e925);color:#111;
                padding:.1rem .4rem;border-radius:99px;vertical-align:middle">RECOMMENDED</span>`
		}
		btn := "Use this setup"
		if active {
			btn = "Re-apply"
		}
		fmt.Fprintf(&b, `<form method="post" style="margin:.6rem 0;padding:.8rem;
   border:%s;border-radius:10px">
 <input type="hidden" name="action" value="preset">
 <input type="hidden" name="preset" value="%s">
 <div style="font-weight:600">%s%s%s</div>
 <div style="font-size:.86rem;margin:.25rem 0">%s</div>
 <div class="mut" style="font-size:.82rem;margin-bottom:.5rem">%s</div>
 <button type="submit" style="padding:.45rem .9rem;border:0;border-radius:7px;font-weight:600;
   background:%s;color:#111;cursor:pointer">%s</button></form>`,
			border, p.ID,
			template.HTMLEscapeString(p.Title), rec, badge,
			template.HTMLEscapeString(p.Cost),
			template.HTMLEscapeString(p.Detail),
			map[bool]string{true: "var(--line)", false: "var(--accent,#b6e925)"}[active],
			btn)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// ── what is actually loaded right now ───────────────────────────────────────

// liveEngines reports what the booth is USING at this moment, read from the
// running booth rather than from the settings file.
//
// The distinction is the whole point. A setting saying "local" and a booth
// actually speaking with it are different facts, and every confusing hour spent
// on this feature has come from reading the first and assuming the second.
func (s *liveServer) liveEngines() (voice, script string, up bool) {
	if s == nil || s.booth == nil {
		return "", "", false
	}
	b := s.booth
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.speech != nil {
		switch b.speech.Provider() {
		case tts.ProviderLocal:
			voice = "on this Mac"
		case tts.ProviderQwen:
			voice = "Qwen, over the internet"
		default:
			voice = b.speech.Provider()
		}
	} else if b.gem != nil {
		voice = "Gemini, over the internet"
	}
	switch {
	case b.mainScriptName != "" && b.mainScriptLocal:
		script = b.mainScriptName + " (on this Mac)"
	case b.mainScriptName != "":
		script = b.mainScriptName + " (over the internet)"
	case b.script != nil:
		script = "Qwen, over the internet"
	case b.gem != nil:
		script = "Gemini, over the internet"
	}
	return voice, script, b.enabled
}

// runningPanelHTML is the "right now" card, plus how to start and stop it.
func (s *liveServer) runningPanelHTML() string {
	voice, script, up := s.liveEngines()
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Running right now</h2>`)

	if voice == "" && script == "" {
		// Not running is a symptom; it needs a cause.
		//
		// Found by this very panel: a setup that named a local writer which had
		// never been downloaded left the booth silently dead, with the settings
		// page happily showing the choice. Saying only "not running" would have
		// repeated the fault it caught.
		b.WriteString(`<p><strong>The commentary booth is not running.</strong></p>`)
		if why := s.boothNotRunningReason(); why != "" {
			fmt.Fprintf(&b, `<p style="color:var(--warn,#c88)">%s</p>`,
				template.HTMLEscapeString(why))
		} else {
			b.WriteString(`<p class="mut">It starts with dd-pits once commentary is
 switched on and either a key or a local model is set up.</p>`)
		}
	} else {
		row := func(label, val string) {
			if val == "" {
				val = "—"
			}
			fmt.Fprintf(&b, `<tr><td style="white-space:nowrap"><strong>%s</strong></td>`+
				`<td style="padding-left:.8rem">%s</td></tr>`,
				label, template.HTMLEscapeString(val))
		}
		b.WriteString(`<table style="font-size:.9rem">`)
		row("Voice", voice)
		row("Words", script)
		row("Commentary", map[bool]string{true: "on", false: "switched off"}[up])
		b.WriteString(`</table>`)
		b.WriteString(`<p class="mut" style="font-size:.82rem;margin-top:.5rem">Read from the
 booth itself, not from the settings — so this is what is genuinely in use.</p>`)
	}

	// Start and stop, stated plainly.
	//
	// There is no separate server to run: a local model is loaded by dd-pits and
	// released when it exits. Saying so is better than implying a control panel
	// that does not exist — and it is genuinely the whole answer.
	b.WriteString(`<h3>Starting and stopping</h3>
 <p style="font-size:.9rem">A local model is loaded by dd-pits itself. There is no
 separate server to start.</p>
 <table style="font-size:.88rem"><tr>
  <td style="vertical-align:top;white-space:nowrap"><strong>To start it</strong></td>
  <td style="padding-left:.8rem">Choose a setup above, then quit dd-pits and start it
   again. The first line of commentary takes longer than the rest — that is the model
   loading. After that it stays in memory.</td></tr>
 <tr><td style="vertical-align:top;white-space:nowrap"><strong>To stop it</strong></td>
  <td style="padding-left:.8rem">Quit dd-pits (Ctrl-C in its window, or close it). The
   model is released with it and the memory comes back.</td></tr>
 <tr><td style="vertical-align:top;white-space:nowrap"><strong>To go back to the cloud</strong></td>
  <td style="padding-left:.8rem">Pick &ldquo;Everything from the cloud&rdquo; above and restart.
   Nothing is deleted — switching back to local later needs no download.</td></tr>
 <tr><td style="vertical-align:top;white-space:nowrap"><strong>To remove it entirely</strong></td>
  <td style="padding-left:.8rem">Delete the <code>voice-env</code> folder beside your
   settings file. That is everything this page installed.</td></tr></table>
 <p class="mut" style="font-size:.82rem">If lap calls start arriving late during a heat,
 the model and the timing are competing for this Mac. Switch &ldquo;Mid-heat lap calls&rdquo;
 off on the Commentary page, or move the words back to the cloud.</p>`)
	b.WriteString(`</div>`)
	return b.String()
}

// boothNotRunningReason names the most likely cause, so the panel does not just
// report a symptom.
func (s *liveServer) boothNotRunningReason() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	bc := s.cfg.BoothConfig()
	m := probeMachine()

	if sm := strings.TrimSpace(bc.ScriptModel); sm != "" {
		for _, st := range m.Scripts {
			if st.Spec.ID == sm && !st.Downloaded {
				return "It is set to write the words with " + st.Spec.Label +
					" on this Mac, but that model has not been downloaded — so there is " +
					"nothing to write with. Install it above, or switch the words back " +
					"to the cloud."
			}
		}
	}
	// Everything a local setup needs is present, so it is loading rather than
	// broken. A large model takes MINUTES to come up, and reporting that as a
	// configuration fault sends someone hunting a problem they do not have.
	if bc.Engine == "local" && m.Voice.Downloaded {
		if sm := strings.TrimSpace(bc.ScriptModel); sm != "" {
			return "It is still starting up — a local model this size takes a few minutes " +
				"to load. Reload this page shortly. The first line of commentary will be " +
				"slow, then it settles."
		}
		return "It is still starting up — reload this page in a moment."
	}
	if bc.Engine == "local" && !m.Voice.Downloaded {
		return "It is set to speak with a local model that has not been downloaded yet. " +
			"Install it above, or switch the voice back to the cloud."
	}
	if bc.Engine == "qwen" && strings.TrimSpace(bc.QwenKey) == "" {
		return "It is set to use Qwen over the internet, but no API key has been entered " +
			"on the Commentary page."
	}
	return ""
}

// ── proving it works, and restarting without a terminal ─────────────────────

// testLocalVoice synthesises a sample with the LOCAL engine and returns it.
//
// The point is proof rather than reassurance. Everything else on this page
// reports configuration; this actually loads the model on this machine and
// makes a sound with it, which is the only thing that settles whether the local
// setup works before a race day depends on it.
func (s *liveServer) testLocalVoice(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	py := venvPython()
	if !fileExists(py) {
		py = lookupPython()
	}
	if py == "" {
		http.Error(w, "No Python 3 — install the voice above first.", http.StatusPreconditionRequired)
		return
	}

	// Generous: this may be the first load, which pulls the model into memory.
	ctx, cancel := timeoutCtx(4 * time.Minute)
	defer cancel()

	// The reference recording MUST be passed, or this tests the wrong thing.
	//
	// Without it the model speaks in its own default voice, and the test then
	// proves only that local speech works — not that it sounds like the person
	// you recorded. Which is the entire question being asked, and it was
	// answered wrongly: "who was the second voice?" was a stranger's, because
	// the clone was never applied.
	bc := localBooth{}
	if s.cfg != nil {
		bc = s.cfg.BoothConfig()
	}
	tcfg := tts.LocalConfig{
		Python:         py,
		Script:         defaultLocalVoiceScript(),
		Model:          voiceModel.ID,
		ReferenceAudio: bc.VoiceRef,
		ReferenceText:  bc.VoiceRefText,
	}
	// Borrow the booth's engine for the same voice instead of paying a model
	// load per button press — and per the 16GB lesson, never a fourth copy.
	eng := sharedLongformEngine(tcfg)
	owned := eng == nil
	if owned {
		var err error
		eng, err = tts.StartLocal(ctx, tcfg)
		if err != nil {
			http.Error(w, "The local voice would not start: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	defer func() {
		if owned {
			_ = eng.Close()
		}
	}()

	pcm, rate, err := eng.Speak(ctx, tts.Request{
		Text: "Local commentary is working. bob9 through in eleven point four, " +
			"the quickest of the heat so far.",
	})
	if err != nil {
		http.Error(w, "It loaded but produced no audio: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(pcm) == 0 {
		http.Error(w, "It returned silence.", http.StatusInternalServerError)
		return
	}
	if rate <= 0 {
		rate = boothSampleRate
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(wavFromPCM(pcm, rate))
}

// restartNow relaunches the agent so a settings change takes effect.
//
// The voice is chosen when the process starts, so switching it has always meant
// "now go and restart it" — which for someone who double-clicked an icon means
// finding a Terminal window and knowing what Ctrl-C does. Reuses the same exec
// the self-updater uses: same PID, same arguments, and a failed exec leaves this
// process intact and still racing.
func (s *liveServer) restartNow(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	exe, err := osExecutable()
	if err != nil {
		http.Error(w, "Could not find my own program to restart: "+err.Error(),
			http.StatusInternalServerError)
		return
	}
	// Answered BEFORE exec, because exec never returns — the page has to be told
	// to start polling for the successor while this process still exists.
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("restarting"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(600 * time.Millisecond) // let the response land
		if err := restartSelf(exe); err != nil {
			log.Printf("restart: %v — still running the old process", err)
		}
	}()
}

// designParagraph is the read every designed voice is built from: ~18 seconds,
// with calm and excitement both in it, so the clone learns the character's
// RANGE rather than one register. Fixed, so two designs differ only by their
// description — never by what happened to be typed.
const designParagraph = "Welcome back to trackside, ladies and gentlemen — what a day of " +
	"racing we have in store. Three heats down, the qualifying board is tightening, and the " +
	"big names are starting to show their hands. HOLESHOT — one point six four, absolutely " +
	"flying! And through the final gate it is close, so close. Strap in, arm your quads, " +
	"and let's go racing."

func osExecutable() (string, error) { return os.Executable() }

// restartToApply answers with a self-reloading page and restarts the agent —
// the pattern the who-speaks switch proved: settings that are baked into
// engines at startup are APPLIED by restarting, immediately and without being
// asked, rather than stored and left to disagree with what is running.
func (s *liveServer) restartToApply(w http.ResponseWriter, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8">
<title>Applying…</title><body style="font-family:system-ui;background:#111;color:#ddd;padding:2rem">
<h2>` + template.HTMLEscapeString(title) + `</h2>
<p>` + template.HTMLEscapeString(detail) + `</p>
<script>const t=setInterval(async()=>{try{const r=await fetch('/setup/models',{cache:'no-store'});
if(r.ok){clearInterval(t);location.href='/setup/models';}}catch(e){}},700);</script>`))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(600 * time.Millisecond) // let the response land
		exe, err := osExecutable()
		if err == nil {
			err = restartSelf(exe)
		}
		if err != nil {
			log.Printf("restart: %v — still running the old process; the change applies "+
				"on the next manual restart", err)
		}
	}()
}

// cloudKeyPanelHTML asks for the API key when any half still needs one.
//
// Shown here rather than only on the Commentary page: the commonest setup is
// "voice on this Mac, words from the cloud", and that still needs a key. Being
// sent elsewhere to finish what you just chose is how a booth ends up
// half-configured and silent.
func cloudKeyPanelHTML(bc localBooth) string {
	voiceLocal := bc.Engine == "local"
	scriptLocal := strings.TrimSpace(bc.ScriptModel) != ""
	needsKey := !voiceLocal || !scriptLocal
	hasKey := strings.TrimSpace(bc.QwenKey) != ""

	if !needsKey {
		return `<div class="card"><h2>🔑 API key</h2>
 <p class="mut">Not needed — nothing is using the cloud. This setup works with no
 internet at all.</p></div>`
	}

	which := "the words"
	if !voiceLocal && !scriptLocal {
		which = "both halves"
	} else if !voiceLocal {
		which = "the voice"
	}

	var b strings.Builder
	b.WriteString(`<div class="card"><h2>🔑 API key</h2>`)
	if hasKey {
		fmt.Fprintf(&b, `<p style="color:var(--ok,#7c5)">✓ A key is saved. %s %s the cloud.</p>`,
			capitalise(which), map[bool]string{true: "use", false: "uses"}[which == "both halves"])
	} else {
		fmt.Fprintf(&b, `<p style="color:var(--warn,#c88)"><strong>A key is needed.</strong>
 This setup uses the cloud for %s, and without a key that half cannot run —
 so there will be no commentary at all.</p>`, which)
	}
	b.WriteString(`<form method="post"><input type="hidden" name="action" value="key">
 <label>Qwen API key</label>
 <input type="password" name="qwen_key" placeholder="` +
		map[bool]string{true: "leave blank to keep the saved key", false: "sk-…"}[hasKey] + `">
 <p class="mut" style="font-size:.82rem">From <strong>Alibaba Model Studio</strong> →
 API keys. If a key stops working, check the free quota has not run out and that
 &ldquo;use free tier only&rdquo; is switched off in their console — that blocks it even
 after you have paid.</p>
 <button type="submit" style="padding:.45rem .9rem;border:0;border-radius:7px;font-weight:600;
   background:var(--accent,#b6e925);color:#111;cursor:pointer">Save key</button>`)
	if hasKey {
		b.WriteString(` <label style="display:inline-flex;align-items:center;gap:.3rem;margin-left:.8rem">
  <input type="checkbox" name="qwen_key_clear" value="1" style="width:auto">
  <span class="mut">remove the saved key</span></label>`)
	}
	b.WriteString(`</form></div>`)
	return b.String()
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// actionsPanelHTML is the prove-it-and-restart pair.
func actionsPanelHTML(m machineState, bc localBooth) string {
	canTest := m.AppleSilicon && m.Voice.Downloaded
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Check it works</h2>`)
	if canTest {
		b.WriteString(`<p class="mut">Loads the model on this Mac and speaks a line with it.
 This is proof rather than a status light — if you hear it, the local voice works.
 The first time can take a couple of minutes while the model loads.</p>
 <button type="button" id="tv" style="padding:.5rem 1rem;border:0;border-radius:8px;
   font-weight:600;background:var(--accent,#b6e925);color:#111;cursor:pointer">
   🔊 Test the local voice</button>
 <span class="mut" id="tvs" style="margin-left:.6rem"></span>
 <div id="tva" style="margin-top:.6rem"></div>`)
	} else {
		b.WriteString(`<p class="mut">Install the local voice above and this will let you
 test it before a race day.</p>`)
	}
	b.WriteString(`<h3 style="margin-top:1.2rem">Apply your changes</h3>
 <p class="mut">The voice is chosen when dd-pits starts, so switching it needs a restart.
 This does it for you — same window, same settings, back in a few seconds.</p>
 <button type="button" id="rs" style="padding:.5rem 1rem;border:0;border-radius:8px;
   font-weight:600;background:var(--accent,#b6e925);color:#111;cursor:pointer">
   ↻ Restart dd-pits now</button>
 <span class="mut" id="rss" style="margin-left:.6rem"></span>
 <p class="mut" style="font-size:.82rem;margin-top:.6rem">Nothing is lost — the dashboard,
 the goggles and the timing all reconnect on their own. Do not do it mid-heat.</p>
</div>`)
	return b.String()
}

// actionsScript wires the two buttons, reporting every stage.
func actionsScript() string {
	return `<script>
(function(){
 var tv=document.getElementById('tv'), tvs=document.getElementById('tvs'),
     tva=document.getElementById('tva'), rs=document.getElementById('rs'),
     rss=document.getElementById('rss');
 if(tv) tv.addEventListener('click', function(){
   tv.disabled=true;
   // Narrated, because the first load is slow enough that a silent button
   // reads as broken.
   tvs.textContent='Loading the model on this Mac… this can take a couple of minutes.';
   fetch('/setup/models/test-voice',{method:'POST'}).then(function(r){
     if(!r.ok) return r.text().then(function(t){ throw new Error(t); });
     tvs.textContent='Speaking…';
     return r.blob();
   }).then(function(bl){
     tvs.textContent='✓ The local voice works.';
     tva.innerHTML='';
     var a=document.createElement('audio'); a.controls=true; a.autoplay=true;
     a.src=URL.createObjectURL(bl); tva.appendChild(a);
     tv.disabled=false;
   }).catch(function(e){
     tvs.textContent='✗ '+(e.message||'it did not work');
     tv.disabled=false;
   });
 });
 if(rs) rs.addEventListener('click', function(){
   rs.disabled=true; rss.textContent='Restarting…';
   fetch('/setup/restart',{method:'POST'}).then(function(){
     // Polled until the successor answers, so the page says when it is BACK
     // rather than guessing at a delay.
     var tries=0;
     var t=setInterval(function(){
       tries++;
       fetch('/healthz',{cache:'no-store'}).then(function(r){
         if(r.ok){ clearInterval(t); rss.textContent='✓ Back up — reloading.';
                   setTimeout(function(){ location.reload(); }, 700); }
       }).catch(function(){
         rss.textContent='Restarting… ('+tries+'s)';
         if(tries>45){ clearInterval(t);
           rss.textContent='It has not come back — check the dd-pits window.'; }
       });
     }, 1000);
   }).catch(function(){ rss.textContent='Could not restart — quit and start it by hand.';
                        rs.disabled=false; });
 });
})();
</script>`
}

// deleteModel removes a model's weights from the cache.
//
// Done through huggingface's own cache API rather than by deleting a path we
// guessed at: these are multi-gigabyte directories with a layout we do not own,
// and "rm -rf a path I assembled from a model name" is not a thing to ship.
func deleteModel(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("no model given")
	}
	// Only ever a model this page offers. Otherwise the field is an arbitrary
	// string being handed to a delete operation.
	known := id == voiceModel.ID
	for _, m := range scriptModels {
		if m.ID == id {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("not a model this page manages")
	}
	py := venvPython()
	if !fileExists(py) {
		py = lookupPython()
	}
	if py == "" {
		return fmt.Errorf("no Python 3 to remove it with")
	}
	ctx, cancel := timeoutCtx(90 * time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, py, "-c",
		`import sys
from huggingface_hub import scan_cache_dir
want = sys.argv[1]
info = scan_cache_dir()
hits = [r for r in info.repos if r.repo_id == want]
if not hits:
    print("not cached"); sys.exit(0)
revs = [rev.commit_hash for r in hits for rev in r.revisions]
info.delete_revisions(*revs).execute()
print("deleted")`, id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not remove it: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// modelActionsHTML is the per-model row of buttons: use it, or delete it.
func modelActionsHTML(st modelState, inUse bool, isVoice bool) string {
	if !st.FitsRAM {
		return ""
	}
	var b strings.Builder
	if st.Downloaded {
		// "Use this" only for the WRITER. The voice is a single model, chosen by
		// the local/cloud toggle rather than by picking between models.
		if !isVoice {
			if inUse {
				b.WriteString(`<span style="font-size:.74rem;background:var(--ok,#7c5);color:#111;
 padding:.1rem .45rem;border-radius:99px">IN USE</span> `)
			} else {
				fmt.Fprintf(&b, `<form method="post" style="display:inline;margin:0">
 <input type="hidden" name="action" value="use">
 <input type="hidden" name="model" value="%s">
 <button type="submit" style="padding:.2rem .55rem;border:0;border-radius:6px;
   font-size:.76rem;font-weight:600;background:var(--accent,#b6e925);color:#111;
   cursor:pointer">Use this</button></form> `,
					template.HTMLEscapeString(st.Spec.ID))
			}
		}
		// Deleting frees real disk, so it asks first — several gigabytes and a
		// slow re-download is not something to trigger by a stray click.
		fmt.Fprintf(&b, `<form method="post" style="display:inline;margin:0"
 onsubmit="return confirm('Delete %s (%s)? You can download it again later.')">
 <input type="hidden" name="action" value="delete">
 <input type="hidden" name="model" value="%s">
 <button type="submit" style="padding:.2rem .55rem;border:1px solid var(--line);
   border-radius:6px;font-size:.76rem;background:transparent;color:var(--muted);
   cursor:pointer">Delete</button></form>`,
			template.HTMLEscapeString(st.Spec.Label), st.Spec.Size,
			template.HTMLEscapeString(st.Spec.ID))
		return b.String()
	}
	kind := "script"
	if isVoice {
		kind = "voice"
	}
	fmt.Fprintf(&b, `<button type="button" class="dl" data-model="%s" data-kind="%s"
 style="padding:.2rem .55rem;border:0;border-radius:6px;font-size:.76rem;font-weight:600;
 background:var(--accent,#b6e925);color:#111;cursor:pointer">&#8681; Download</button>`,
		template.HTMLEscapeString(st.Spec.ID), kind)
	return b.String()
}

// cloneVoiceHTML is the your-own-voice section for the LOCAL voice model.
//
// Restored after the page split dropped it. Only meaningful for the local
// voice: cloning happens on this machine from a reference recording, and the
// cloud voices are a fixed catalogue instead.
func cloneVoiceHTML(bc localBooth) string {
	return `</table>

<h3 style="margin-top:1.1rem">Your own voices (local only)</h3>
` + voiceLibraryHTML(bc) + `
<p style="font-size:.9rem;margin:.2rem 0">` + voiceRefStatus(bc) + `</p>
<p class="mut" style="font-size:.85rem">The local voice copies a real voice &mdash; yours, or your
 club commentator's &mdash; from one short recording. There is nothing to enrol: the recording
 <strong>is</strong> the voice, and the model re-reads it on every call.</p>

<form method="post" action="/setup/models/voice-upload" enctype="multipart/form-data">
<div class="row">
 <div><label>Name this voice</label>
  <input type="text" name="voice_name" placeholder="e.g. Dave, our club commentator"></div>
 <div><label>Recording (.wav, 10&ndash;20 seconds)</label>
  <input type="file" name="voice_wav" accept=".wav,audio/wav">
  <div style="margin-top:.35rem"><span class="mut" style="font-size:.8rem">&mdash; or record it
   right here, captured in the exact format the model wants:</span>
   <div id="vrHere" style="margin-top:.3rem"></div></div></div>
 <div><label>Exactly what the recording says</label>
  <input type="text" name="voice_ref_text" value="` + htmlEsc(bc.VoiceRefText) + `"
   placeholder="type the words spoken in the clip, word for word"></div>
</div>
<p class="mut" style="font-size:.82rem">Clear speech, no music, no background noise. The
 transcript must match the words <strong>exactly</strong> &mdash; that is what the model lines the
 voice up against, and a wrong one gives a poor clone rather than an error. It is stored beside
 your settings file, so backing up one backs up both.</p>
<button type="submit" style="padding:.4rem .8rem;border:0;border-radius:7px;font-weight:600;
 background:var(--accent,#b6e925);color:#111;cursor:pointer">Upload recording</button>
<span class="mut" style="margin-left:.5rem">Then use &ldquo;Test the local voice&rdquo; below to hear it.</span>
</form>
<script>
/* Inlined rather than served: dd-pits' pages are self-contained strings with
   no static file server. */
// In-browser voice recording for cloning, in the format the models want.
//
// Every constraint here was learned the expensive way at a bench:
//  - the cloud enrolment REJECTS anything under 16kHz (Audio.SampleRateError);
//  - a float32 wav breaks readers that assume 16-bit;
//  - a 3-second reference clones a voice that chews the ends of its words,
//    because the model ends utterances the way its reference taught it to.
// So this records straight to 24kHz mono 16-bit PCM WAV — the native rate of
// the voice model, above the enrolment floor, in the one encoding everything
// downstream reads — and the UI leans on the operator for the part no encoder
// can fix: ten to twenty seconds, ending GENTLY, because the recording IS the
// voice.
//
// Usage: VoiceRecorder.mount(el, { onWav(blob, seconds) }) renders the
// record/stop/play controls into el and hands back a finished WAV.
window.VoiceRecorder = (function () {
  const TARGET_RATE = 24000, MIN_S = 8, MAX_S = 30;

  function encodeWav(samples, rate) {
    const buf = new ArrayBuffer(44 + samples.length * 2);
    const v = new DataView(buf);
    const str = (o, s) => { for (let i = 0; i < s.length; i++) v.setUint8(o + i, s.charCodeAt(i)); };
    str(0, "RIFF"); v.setUint32(4, 36 + samples.length * 2, true); str(8, "WAVE");
    str(12, "fmt "); v.setUint32(16, 16, true); v.setUint16(20, 1, true);
    v.setUint16(22, 1, true); v.setUint32(24, rate, true);
    v.setUint32(28, rate * 2, true); v.setUint16(32, 2, true); v.setUint16(34, 16, true);
    str(36, "data"); v.setUint32(40, samples.length * 2, true);
    for (let i = 0; i < samples.length; i++) {
      const s = Math.max(-1, Math.min(1, samples[i]));
      v.setInt16(44 + i * 2, s < 0 ? s * 0x8000 : s * 0x7FFF, true);
    }
    return new Blob([buf], { type: "audio/wav" });
  }

  // Linear resample to the target rate — capture runs at whatever the device
  // gives (44.1k/48k); the models want 24k, and doing it here means the server
  // never sees a rate it has to reject.
  function resample(input, from) {
    if (from === TARGET_RATE) return input;
    const ratio = from / TARGET_RATE;
    const out = new Float32Array(Math.floor(input.length / ratio));
    for (let i = 0; i < out.length; i++) {
      const pos = i * ratio, i0 = Math.floor(pos), frac = pos - i0;
      out[i] = input[i0] * (1 - frac) + (input[Math.min(i0 + 1, input.length - 1)] || 0) * frac;
    }
    return out;
  }

  function mount(el, opts) {
    el.innerHTML = '<button type="button" class="vr-rec">&#127908; Record</button> ' +
      '<button type="button" class="vr-stop" disabled>&#9209; Stop</button> ' +
      '<span class="vr-time" style="font-variant-numeric:tabular-nums"></span> ' +
      '<span class="vr-hint" style="opacity:.7;font-size:.85em"></span>' +
      '<div class="vr-out" style="margin-top:.4rem"></div>';
    const rec = el.querySelector(".vr-rec"), stop = el.querySelector(".vr-stop"),
      time = el.querySelector(".vr-time"), hint = el.querySelector(".vr-hint"),
      out = el.querySelector(".vr-out");
    let ctx, node, src, stream, chunks, started, timer;

    hint.textContent = "10–20 seconds; finish your last sentence gently — the clone copies how you END.";

    async function start() {
      try {
        stream = await navigator.mediaDevices.getUserMedia({ audio: {
          echoCancellation: false, noiseSuppression: false, autoGainControl: false } });
      } catch (e) { hint.textContent = "microphone refused: " + e.message; return; }
      ctx = new (window.AudioContext || window.webkitAudioContext)();
      src = ctx.createMediaStreamSource(stream);
      node = ctx.createScriptProcessor(4096, 1, 1);
      chunks = [];
      node.onaudioprocess = (ev) => chunks.push(new Float32Array(ev.inputBuffer.getChannelData(0)));
      src.connect(node); node.connect(ctx.destination);
      started = Date.now();
      rec.disabled = true; stop.disabled = false; out.innerHTML = "";
      timer = setInterval(() => {
        const s = (Date.now() - started) / 1000;
        time.textContent = s.toFixed(1) + "s";
        if (s >= MAX_S) finish(); else if (s >= MIN_S) time.style.color = "#7c5";
      }, 100);
    }

    function finish() {
      clearInterval(timer);
      const seconds = (Date.now() - started) / 1000;
      const rate = ctx.sampleRate;
      node.disconnect(); src.disconnect();
      stream.getTracks().forEach(t => t.stop()); ctx.close();
      rec.disabled = false; stop.disabled = true;

      let total = 0; chunks.forEach(c => total += c.length);
      const all = new Float32Array(total); let off = 0;
      chunks.forEach(c => { all.set(c, off); off += c.length; });
      const wav = encodeWav(resample(all, rate), TARGET_RATE);

      const au = document.createElement("audio");
      au.controls = true; au.src = URL.createObjectURL(wav);
      out.appendChild(au);
      if (seconds < MIN_S) {
        hint.textContent = "that was " + seconds.toFixed(1) +
          "s — under " + MIN_S + "s clones poorly; consider recording again.";
      } else {
        hint.textContent = seconds.toFixed(1) + "s at 24kHz mono — ready.";
      }
      opts.onWav(wav, seconds);
    }

    rec.addEventListener("click", start);
    stop.addEventListener("click", finish);
  }
  return { mount };
})();

(function(){
 var slot=document.getElementById('vrHere'); if(!slot||!window.VoiceRecorder) return;
 VoiceRecorder.mount(slot,{onWav:function(wav,secs){
   // The recording becomes the chosen file of the EXISTING upload form, so one
   // path validates, stores, and advises — recorded or picked alike.
   var f=new File([wav],'recorded-'+Math.round(secs)+'s.wav',{type:'audio/wav'});
   var dt=new DataTransfer(); dt.items.add(f);
   document.querySelector('input[name=voice_wav]').files=dt.files;
 }});
})();
</script>

<form method="post" action="/setup/models/voice-clear" style="margin-top:.5rem">
<details><summary class="mut" style="cursor:pointer;font-size:.85rem">Or point at a file already
 on this Mac</summary>
 <div style="margin-top:.4rem">
  <input type="hidden" name="action" value="clone">
  <label>Full path to a .wav</label>
  <input type="text" name="voice_ref" form="refpath" value="` + htmlEsc(bc.VoiceRef) + `"
   placeholder="/Users/you/Desktop/commentator.wav">
  <p class="mut" style="font-size:.8rem">Keep it somewhere permanent. It is read on every call, so
   a file in Downloads or on a USB stick will silence the booth when it disappears.</p>
 </div></details>
` + clearVoiceButton(bc) + `</form>
<form method="post" action="/setup/models" id="refpath" style="display:none">
 <input type="hidden" name="action" value="clone">
 <input type="hidden" name="voice_ref_text" value="` + htmlEsc(bc.VoiceRefText) + `">
</form>

<div class="card" style="margin-top:1rem">
<h3>Pronunciations</h3>
<p class="mut" style="font-size:.85rem">Callsigns are made up, and the voice can only guess.
 When it says a name wrong, spell out how it should SOUND — one per line, applied to every
 race call and commentary line the moment you save. Only the audio changes: leaderboards,
 logs and the page keep the real name.</p>
<form method="post" action="/setup/models">
 <input type="hidden" name="action" value="pronounce">
 <textarea name="pronounce" rows="4" style="width:100%;font-family:monospace"
  placeholder="Bola = Boh-lah&#10;9dtr = niner detter">` + htmlEsc(bc.Pronounce) + `</textarea>
 <button type="submit" style="margin-top:.4rem;padding:.3rem .7rem">Save pronunciations</button>
</form>
</div>
<table style="display:none">`
}

// clearVoiceButton only appears when there is something to clear.
func clearVoiceButton(bc localBooth) string {
	if strings.TrimSpace(bc.VoiceRef) == "" {
		return ""
	}
	return `<button type="submit" style="margin-top:.5rem;padding:.3rem .7rem;
 border:1px solid var(--line);border-radius:6px;font-size:.8rem;background:transparent;
 color:var(--muted);cursor:pointer">Remove this recording, use the default voice</button>`
}

// voicesPanelHTML is the voices card: who speaks, in what language, how.
//
// Here rather than on the Commentary page because a voice is a property of the
// model producing it. Choosing "Ethan" on one page while the thing that can
// actually say it was configured on another is how somebody ends up with a
// voice name the engine rejects — which happened: a Gemini voice reached Qwen
// and failed every beat.
func voicesPanelHTML(bc localBooth) string {
	local := bc.Engine == "local"

	var b strings.Builder
	b.WriteString(`<div class="card"><h2>🗣 Voices</h2>`)

	// A contradiction the page used to accept in silence.
	//
	// Reported as "the post race was not using the selected voice", and it was
	// not: a CLOUD voice was chosen while the voice engine was set to LOCAL, and
	// the local model has no voice names — so the choice was dropped and its own
	// default spoke instead. With no recording either, that is a stranger.
	//
	// Warned about rather than silently resolved, because both readings are
	// legitimate: they may want that voice, or they may want local. Both fixes
	// are one press.
	if local && strings.TrimSpace(bc.VoiceRef) == "" && bc.VoicePBP != "" &&
		!strings.HasPrefix(bc.VoicePBP, "ref:") {
		fmt.Fprintf(&b, `<div style="padding:.6rem .8rem;border-left:3px solid var(--warn,#c88);
 background:rgba(255,255,255,.03);border-radius:0 8px 8px 0;margin-bottom:.8rem">
 <strong>&#9888; You have picked %s, but you will not hear it.</strong>
 <div class="mut" style="font-size:.85rem;margin-top:.3rem">The voice is set to run
  <strong>on this Mac</strong>, and the local model has no voice names &mdash; so it uses its own
  default instead. You have no recording uploaded either, which is the only way to change it.</div>
 <div style="margin-top:.5rem">
  <form method="post" style="display:inline;margin:0">
   <input type="hidden" name="action" value="choose">
   <input type="hidden" name="voice_where" value="cloud">
   <input type="hidden" name="script_where" value="%s">
   <input type="hidden" name="script_model" value="%s">
   <button type="submit" style="padding:.35rem .8rem;border:0;border-radius:7px;font-weight:600;
    background:var(--accent,#b6e925);color:#111;cursor:pointer">Use the cloud, so I hear %s</button>
  </form>
  <span class="mut" style="margin-left:.5rem;font-size:.83rem">or upload a recording above to
   keep it local.</span></div></div>`,
			template.HTMLEscapeString(qwentts.ClonedVoiceName(bc.VoicePBP)),
			map[bool]string{true: "local", false: "cloud"}[strings.TrimSpace(bc.ScriptModel) != ""],
			template.HTMLEscapeString(bc.ScriptModel),
			template.HTMLEscapeString(qwentts.ClonedVoiceName(bc.VoicePBP)))
	}

	// Name the local voice HERE, because this is where someone looks for it.
	//
	// A recording cannot appear in the voice DROPDOWN — that lists Qwen's cloud
	// catalogue, and the local model has no voice names at all. But uploading a
	// voice and then not finding it on the voices card reads as it having failed.
	if local {
		if v := activeVoiceName(bc); v != "" {
			fmt.Fprintf(&b, `<p style="padding:.5rem .7rem;border-left:3px solid var(--ok,#7c5);
 background:rgba(255,255,255,.03);border-radius:0 8px 8px 0;font-size:.9rem">
 The local voice is speaking as <strong>%s</strong> &mdash; your own recording.
 <span class="mut">Manage it under &ldquo;Your own voices&rdquo; above. It has no entry in the
 dropdowns below: those are Qwen&rsquo;s cloud voices, and a recording is not one of them.</span></p>`,
				template.HTMLEscapeString(v))
		}
	}
	if local {
		// Not a nag: the local model has no voice catalogue at all, so a duo
		// setting and a second voice name are silently meaningless here.
		b.WriteString(`<p style="padding:.5rem .7rem;border-left:3px solid var(--warn,#c88);
 background:rgba(255,255,255,.03);border-radius:0 8px 8px 0;font-size:.88rem">
 <strong>The local voice has no voice names.</strong> It has one built-in voice, and the only way
 to change it is your own recording above. So the pickers below apply when the voice runs in the
 cloud &mdash; and commentary runs <strong>solo</strong> while it is local, because two speakers
 would come out as the same voice talking to itself.</p>`)
	}

	b.WriteString(`<form method="post"><input type="hidden" name="action" value="voices">
<div class="row">
 <div><label>Commentators</label>
  <select name="mode">
   <option value="duo"` + selIf(bc.Mode != "solo") + `>Two — a caller and an analyst</option>
   <option value="solo"` + selIf(bc.Mode == "solo") + `>One voice</option>
  </select></div>
 <div><label>Main voice (play-by-play)</label>
  <select name="voice_pbp">` + allVoiceOptions(bc, bc.VoicePBP, qwentts.DefaultVoicePBP) + `</select></div>
 <div><label>Second voice (analyst)</label>
  <select name="voice_color">` + qwenVoiceOptions(bc.VoiceColor, qwentts.DefaultVoiceColor, clonedVoicesFor(bc)) + `</select></div>
</div>

<div class="row" style="margin-top:.6rem">
 <div><label>Language</label>
  <select name="language">` + languageOptions(bc.Language, bc.Engine) + `</select>
  <span class="mut" style="font-size:.78rem">Written AND spoken in this language.</span></div>
 <div><label>Regional accent (optional)</label>
  <input type="text" name="accent" value="` + htmlEsc(bc.Accent) + `"
   placeholder="e.g. Australian, Mexican">
  <span class="mut" style="font-size:.78rem">Any cloud voice can take an accent — with or without a language.</span></div>
 <div><label>Energy</label>
  <select name="energy">` + energyOptions(bc.Energy) + `</select></div>
</div>

<div class="row" style="margin-top:.6rem">
 <div><label>Personality</label>
  <select name="theme">` + themeOptions(bc.Theme) + `</select></div>
 <div><label>Humour</label>
  <select name="humour">` + humourOptions(bc.Humour) + `</select></div>
</div>
<p class="mut" style="font-size:.8rem">Personality and humour shape the WORDS; energy, accent and
 the characteristics below shape how they are SPOKEN. All of it is the AI, so it all lives here.</p>

<label style="margin-top:.6rem">Voice characteristics (optional)</label>
<input type="text" name="style" value="` + htmlEsc(bc.Style) + `"
 placeholder="e.g. Australian accents, ESPN polish, dry between heats">
<p class="mut" style="font-size:.8rem">Free text describing how the voices should sound. Leave
 blank for the default.</p>

<button type="submit" style="padding:.45rem .9rem;border:0;border-radius:7px;font-weight:600;
 background:var(--accent,#b6e925);color:#111;cursor:pointer">Save voices</button>
</form>

<div style="border:1px solid var(--line);border-radius:9px;padding:.7rem;margin:.8rem 0 0">
 <strong>Hear it before you save</strong>
 <p class="mut" style="font-size:.86rem;margin:.3rem 0">This plays with the settings AS THEY ARE
  SET ABOVE RIGHT NOW &mdash; not the saved ones. Change anything, press play, and hear exactly
  what it would sound like. If you like what you hear, click <em>Save voices</em> to make it
  real; until you do, the booth keeps using the saved settings.</p>
 <p class="mut" style="font-size:.8rem;margin:.3rem 0">With one of your own recordings as the
  main voice, the words play in that voice but language, accent, energy and the second voice
  cannot apply &mdash; the local model takes words only, live and here alike. Cloud voices use
  all of it. Personality and humour shape generated scripts, not text you type.</p>
 <p class="mut" id="capHint" style="font-size:.8rem;margin:.3rem 0;color:var(--warn,#ca6)"></p>
 <textarea id="sayText" rows="2" style="width:100%"
  placeholder="ARM YOUR QUADS! Starting on the tone in less than five!"></textarea>
 <p style="margin:.5rem 0 0">
  <button type="button" id="sayBrowser">&#127911; In the browser</button>
  <button type="button" id="saySpeakers">&#128266; On the speakers</button>
  <span class="mut" id="sayMsg" style="margin-left:.5rem"></span></p>
 <span id="saySlot"></span>
</div>

<div style="border:1px solid var(--line);border-radius:9px;padding:.7rem;margin:.8rem 0 0">
 <strong>Design a voice — from one of YOUR voices</strong>
 <p class="mut" style="font-size:.86rem;margin:.3rem 0">Describe a character and it is performed
  IN YOUR VOICE, then cloned into its own stable voice: the character is baked in, and the booth's
  per-moment expression (excited holeshots, calm build-ups) rides on top. Starting from a
  recording clones it raw first, automatically. Runs on the cloud; what you keep is exactly the
  sample you heard.</p>
 <div class="row">
  <div><label>Start from</label><select id="dsgBase">` + designBaseOptions(bc) + `</select></div>
  <div><label>Name the new voice</label><input type="text" id="dsgName" placeholder="e.g. GlennGravel"></div>
 </div>
 <label style="margin-top:.5rem">The character</label>
 <input type="text" id="dsgDesc" placeholder="e.g. gravelly Australian broadcaster in his fifties, warm, unhurried">
 <p style="margin:.5rem 0 0">
  <button type="button" id="dsgGen">&#127911; Generate the sample</button>
  <button type="button" id="dsgKeep" disabled>&#128190; Keep it — clone as my voice</button>
  <span class="mut" id="dsgMsg" style="margin-left:.5rem"></span></p>
 <span id="dsgSlot"></span>
</div></div>
<script>
(function(){
 // The audition sends every control's CURRENT value as an override, so what
 // plays is what these settings would sound like — saved or not. Save is what
 // makes it real; the button copy and the note above say so out loud.
 function sayIt(out){
   var f=document.querySelector('select[name=voice_pbp]').closest('form');
   var t=(document.getElementById('sayText')||{}).value||'';
   if(!t.trim()) t='ARM YOUR QUADS! Starting on the tone in less than five!';
   var msg=document.getElementById('sayMsg'), slot=document.getElementById('saySlot');
   msg.textContent='generating with the settings above (unsaved)\u2026'; slot.innerHTML='';
   var body='text='+encodeURIComponent(t)+
            '&voice='+encodeURIComponent(f.voice_pbp.value)+
            '&ov_mode='+encodeURIComponent(f.mode.value)+
            '&ov_voice_color='+encodeURIComponent(f.voice_color.value)+
            '&ov_language='+encodeURIComponent(f.language.value)+
            '&ov_accent='+encodeURIComponent(f.accent.value)+
            '&ov_energy='+encodeURIComponent(f.energy.value)+
            '&ov_style='+encodeURIComponent(f.style.value)+
            (out==='speakers'?'&out=speakers':'');
   fetch('/setup/models/speak',{method:'POST',
     headers:{'Content-Type':'application/x-www-form-urlencoded'},body:body})
    .then(function(r){
      if(out==='speakers') return r.json().then(function(j){
        if(!r.ok) throw new Error(j.error||'failed');
        msg.textContent='\u2713 playing on '+j.where+
          ' \u2014 these are NOT the saved settings; click Save voices to keep this sound'; });
      if(!r.ok) return r.text().then(function(x){ throw new Error(x); });
      return r.blob().then(function(bl){
        msg.textContent='\u2713 not saved yet \u2014 click Save voices to keep this sound';
        var au=document.createElement('audio'); au.controls=true; au.autoplay=true;
        au.src=URL.createObjectURL(bl); slot.appendChild(au); });
    })
    .catch(function(e){ msg.textContent='\u2717 '+(e.message||'failed').slice(0,140); });
 }
 var sb=document.getElementById('sayBrowser'), ss=document.getElementById('saySpeakers');
 if(sb) sb.addEventListener('click',function(){sayIt('browser');});
 if(ss) ss.addEventListener('click',function(){sayIt('speakers');});

 // Voice design: generate performs the character in the chosen base voice;
 // keep clones the EXACT sample that was auditioned.
 var dg=document.getElementById('dsgGen'), dk=document.getElementById('dsgKeep');
 if(dg) dg.addEventListener('click',function(){
   var m=document.getElementById('dsgMsg'), sl=document.getElementById('dsgSlot');
   m.textContent='performing the character\u2026 (20-60s)'; sl.innerHTML=''; dk.disabled=true; dg.disabled=true;
   fetch('/setup/models',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},
     body:'action=voice_design'+
          '&design_base='+encodeURIComponent(document.getElementById('dsgBase').value)+
          '&design_desc='+encodeURIComponent(document.getElementById('dsgDesc').value)+
          '&design_name='+encodeURIComponent(document.getElementById('dsgName').value)})
    .then(function(r){ if(!r.ok) return r.text().then(function(t){throw new Error(t);}); return r.blob(); })
    .then(function(bl){ m.textContent='this is the voice you would keep \u2014 happy with it?';
      var au=document.createElement('audio'); au.controls=true; au.autoplay=true;
      au.src=URL.createObjectURL(bl); sl.appendChild(au); dk.disabled=false; dg.disabled=false; })
    .catch(function(e){ m.textContent='\u2717 '+(e.message||'failed').slice(0,160); dg.disabled=false; });
 });
 if(dk) dk.addEventListener('click',function(){
   var m=document.getElementById('dsgMsg');
   m.textContent='cloning\u2026'; dk.disabled=true;
   fetch('/setup/models',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},
     body:'action=voice_design_keep'})
    .then(function(r){ return r.json().then(function(j){ if(!r.ok) throw new Error(j.error||'failed'); return j; }); })
    .then(function(j){ m.textContent='\u2713 cloned as '+j.voice+' \u2014 reloading so it appears in the voice pickers\u2026';
      setTimeout(function(){location.reload();},1800); })
    .catch(function(e){ m.textContent='\u2717 '+(e.message||'failed').slice(0,160); dk.disabled=false; });
 });

 // Controls that cannot affect the selected voice are DISABLED, with the
 // reason written next to them — not left looking live while doing nothing.
 // A recording (local model) takes words only: no language, accent, energy,
 // characteristics or second voice, here and on race day alike. The accent
 // box is also inert until a language is chosen, whatever the voice.
 function updateCaps(){
   var f=document.querySelector('select[name=voice_pbp]').closest('form');
   var isRec=(f.voice_pbp.value||'').indexOf('ref:')===0;
   [['mode',isRec],['voice_color',isRec],['language',isRec],
    ['energy',isRec],['style',isRec]].forEach(function(pair){
     var el=f[pair[0]]; if(!el) return;
     el.disabled=pair[1];
     el.style.opacity=pair[1]?'.35':'';
   });
   // Accent needs only a cloud voice — "English with an Italian accent" is an
   // ordinary broadcast ask, and gating it on the language picker made English
   // (the default, which carries no value) the one language that couldn't
   // take an accent.
   f.accent.disabled=isRec; f.accent.style.opacity=isRec?'.35':'';
   var hint=document.getElementById('capHint');
   if(hint){
     hint.textContent = isRec
       ? 'Your recording IS the voice: it speaks the words exactly as recorded, so the greyed-out settings cannot change it \u2014 in this test or at a race. To use them, pick a cloud voice; to accent a recording, record it with the accent.'
       : '';
   }
 }
 var fEl=document.querySelector('select[name=voice_pbp]');
 if(fEl){
   var frm=fEl.closest('form');
   ['voice_pbp','language'].forEach(function(n){
     if(frm[n]) frm[n].addEventListener('change',updateCaps);
   });
   updateCaps();
 }
})();
</script>`)
	return b.String()
}

func selIf(b bool) string {
	if b {
		return " selected"
	}
	return ""
}

// ── is the model loaded, or still loading? ───────────────────────────────────
//
// A large writer takes MINUTES to come up, and until now the page said nothing
// during that time: it reported the configured model as though it were running,
// so switching models looked instantaneous and then commentary quietly failed
// for a few minutes. The state that matters is the difference between what the
// settings ASK for and what the booth actually holds.

type loadState struct {
	Want    string // configured
	Have    string // actually loaded
	Loading bool
	Ready   bool
}

func (s *liveServer) writerLoadState() loadState {
	var st loadState
	if s == nil || s.cfg == nil {
		return st
	}
	st.Want = strings.TrimSpace(s.cfg.BoothConfig().ScriptModel)
	if s.booth == nil {
		return st
	}
	s.booth.mu.Lock()
	have, isLocal := s.booth.mainScriptName, s.booth.mainScriptLocal
	s.booth.mu.Unlock()
	if isLocal {
		st.Have = have
	}
	switch {
	case st.Want == "":
		// Nothing local wanted — the cloud writer needs no loading.
	case st.Have == st.Want:
		st.Ready = true
	default:
		// Asked for a local model and the booth does not hold it yet. Loading is
		// the honest reading: a failure would have logged, and the page offers
		// the log.
		st.Loading = true
	}
	return st
}

// writerLoadHTML is the line the page shows while a model comes up.
func (s *liveServer) writerLoadHTML() string {
	st := s.writerLoadState()
	switch {
	case st.Loading:
		return fmt.Sprintf(`<p id="mload" data-loading="1"
 style="padding:.5rem .7rem;border-left:3px solid var(--warn,#c88);
 background:rgba(255,255,255,.03);border-radius:0 8px 8px 0;margin:.4rem 0">
 <strong>⏳ Loading %s…</strong>
 <span class="mut">A large model takes a few minutes the first time. Commentary will not write
 until it is in memory — this line updates on its own.</span></p>`,
			template.HTMLEscapeString(modelLabel(st.Want)))
	case st.Ready:
		return fmt.Sprintf(`<p id="mload" data-loading="0" style="margin:.4rem 0">
 <span style="color:var(--ok,#7c5)">✓ %s is loaded and in memory</span></p>`,
			template.HTMLEscapeString(modelLabel(st.Have)))
	default:
		return `<p id="mload" data-loading="0"></p>`
	}
}

// writerLoadJSON backs the poll, so a page open while a model loads updates
// itself rather than needing a manual reload.
func (s *liveServer) writerLoadJSON(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	st := s.writerLoadState()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"loading": st.Loading, "ready": st.Ready,
		"want": modelLabel(st.Want), "have": modelLabel(st.Have),
	})
}

func writerLoadScript() string {
	return `<script>
(function(){
 var el=document.getElementById('mload');
 if(!el) return;
 var t=setInterval(function(){
   fetch('/setup/models/load-state').then(function(r){ return r.json(); }).then(function(j){
     if(j.loading){
       el.dataset.loading='1';
       el.innerHTML='<strong>⏳ Loading '+j.want+'…</strong> <span class="mut">'+
         'A large model takes a few minutes the first time.</span>';
     } else if(j.ready){
       // Reloaded once so the whole page reflects the new state, not just this line.
       if(el.dataset.loading==='1'){
         clearInterval(t);
         // Never reload over playing audio. A model finishing loading while you
         // are listening to an audition cut the voice off mid-sentence, which
         // reads as the voice being broken rather than the page being rude.
         var playing=[].slice.call(document.querySelectorAll('audio'))
                       .some(function(a){ return !a.paused && !a.ended; });
         if(playing){
           el.innerHTML='<span style="color:var(--ok,#7c5)">✓ '+j.have+
             ' is loaded</span> <span class="mut">— reload when you have finished listening.</span>';
           return;
         }
         location.reload(); return;
       }
       el.innerHTML='<span style="color:var(--ok,#7c5)">✓ '+j.have+' is loaded and in memory</span>';
     }
   }).catch(function(){});
 }, 3000);
})();
</script>`
}

// voiceLibraryHTML lists the venue's recorded voices, with the active one marked.
//
// A list rather than a single slot, because a club has more than one
// commentator and a recording worth making is worth keeping. Swapping should
// not mean finding the file again.
func voiceLibraryHTML(bc localBooth) string {
	if len(bc.VoiceRefs) == 0 {
		return `<p class="mut" style="font-size:.85rem">No recordings yet. Upload one below and it
 becomes the voice the local model speaks with.</p>`
	}
	var b strings.Builder
	b.WriteString(`<table style="font-size:.88rem;margin-bottom:.6rem">`)
	for _, v := range bc.VoiceRefs {
		active := v.Path == bc.VoiceRef
		mark, act := "○", fmt.Sprintf(`<form method="post" style="display:inline;margin:0">
 <input type="hidden" name="action" value="usevoice">
 <input type="hidden" name="path" value="%s">
 <button type="submit" style="padding:.15rem .5rem;border:1px solid var(--line);border-radius:6px;
  font-size:.76rem;background:transparent;color:var(--fg);cursor:pointer">Use this</button></form>`,
			template.HTMLEscapeString(v.Path))
		if active {
			mark = `<span style="color:var(--ok,#7c5)">✓</span>`
			act = `<span style="font-size:.72rem;background:var(--ok,#7c5);color:#111;
 padding:.1rem .4rem;border-radius:99px">IN USE</span>`
		}
		detail := "file missing"
		if info, err := inspectWAV(v.Path); err == nil {
			detail = fmt.Sprintf("%.1fs, %d Hz", info.Seconds, info.Rate)
		}
		fmt.Fprintf(&b, `<tr><td style="width:1.4rem">%s</td>`+
			`<td style="white-space:nowrap"><strong>%s</strong></td>`+
			`<td class="mut" style="padding-left:.6rem">%s</td>`+
			`<td style="padding-left:.6rem">%s</td></tr>`,
			mark, template.HTMLEscapeString(v.Name), detail, act)
	}
	// Going back to the model's own voice is a choice, not a deletion.
	b.WriteString(`<tr><td></td><td colspan="3" style="padding-top:.3rem">
 <form method="post" style="display:inline;margin:0">
  <input type="hidden" name="action" value="usevoice"><input type="hidden" name="path" value="">
  <button type="submit" style="padding:.15rem .5rem;border:1px solid var(--line);border-radius:6px;
   font-size:.74rem;background:transparent;color:var(--muted);cursor:pointer">
   Use the model's own voice instead</button></form></td></tr></table>`)
	return b.String()
}

// activeVoiceName is the name of the recording currently in use, or "".
func activeVoiceName(bc localBooth) string {
	for _, v := range bc.VoiceRefs {
		if v.Path == bc.VoiceRef && strings.TrimSpace(bc.VoiceRef) != "" {
			return v.Name
		}
	}
	if strings.TrimSpace(bc.VoiceRef) != "" {
		// Uploaded before names existed, or set by hand as a path.
		return "your recording"
	}
	return ""
}

// clonedVoicesFor lists the account's cloned voices so a stored one is matched
// rather than labelled missing.
//
// The models page passed nil when the Voices card moved here, so every cloned
// voice showed as "not found on this account" — alarming, and wrong.
// Time-boxed: a slow DashScope should cost the cloned entries, not the page.
func clonedVoicesFor(bc localBooth) []qwentts.ClonedVoice {
	if strings.TrimSpace(bc.QwenKey) == "" {
		return nil
	}
	qc, err := qwentts.New(qwentts.Config{APIKey: bc.QwenKey, BaseURL: bc.QwenBase})
	if err != nil {
		return nil
	}
	ctx, cancel := timeoutCtx(6 * time.Second)
	defer cancel()
	vs, err := qc.ListVoices(ctx)
	if err != nil {
		return nil
	}
	return vs
}

// allVoiceOptions is every voice this venue can actually use, in one list.
//
// The recordings were missing from this picker while appearing in the compare
// panel's — so a club uploaded a voice, saw it in one dropdown and not the
// other, and reasonably concluded the upload had failed. They are different
// mechanisms (a recording is not a voice NAME) but that is an implementation
// detail, and hiding one of them is not how to express it. They are grouped and
// labelled instead.
func allVoiceOptions(bc localBooth, selected, fallback string) string {
	var b strings.Builder
	if len(bc.VoiceRefs) > 0 {
		b.WriteString(`<optgroup label="Your own recordings — runs on this Mac, free">`)
		for _, v := range bc.VoiceRefs {
			sel := ""
			// Selected when the local voice is active AND this is the recording.
			if bc.Engine == "local" && v.Path == bc.VoiceRef {
				sel = " selected"
			}
			fmt.Fprintf(&b, `<option value="ref:%s"%s>%s</option>`,
				template.HTMLEscapeString(v.Path), sel, template.HTMLEscapeString(v.Name))
		}
		b.WriteString(`</optgroup>`)
	}
	b.WriteString(`<optgroup label="Cloud voices — needs an API key">`)
	// Nothing in the cloud group is selected while a recording is in use, or two
	// options would appear chosen at once.
	sel := selected
	if bc.Engine == "local" && strings.TrimSpace(bc.VoiceRef) != "" {
		sel = "\x00none"
	}
	b.WriteString(qwenVoiceOptions(sel, fallback, clonedVoicesFor(bc)))
	b.WriteString(`</optgroup>`)
	return b.String()
}

// designBaseOptions lists ONLY the venue's own voices as design bases — the
// point of designing is a character in YOUR voice, so the built-ins are not
// offered. Cloud clones are instructable directly; a recording is cloned raw
// first when chosen.
func designBaseOptions(bc localBooth) string {
	var b strings.Builder
	for _, v := range clonedVoicesFor(bc) {
		fmt.Fprintf(&b, `<option value="%s">%s (cloud clone)</option>`,
			template.HTMLEscapeString(v.ID), template.HTMLEscapeString(v.Name()))
	}
	for _, v := range bc.VoiceRefs {
		fmt.Fprintf(&b, `<option value="ref:%s">%s (recording — cloned first)</option>`,
			template.HTMLEscapeString(v.Path), template.HTMLEscapeString(v.Name))
	}
	if b.Len() == 0 {
		return `<option value="">no voices of yours yet — upload a recording above</option>`
	}
	return b.String()
}

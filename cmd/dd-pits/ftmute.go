package main

// One switch decides who speaks, and dd-pits settles BOTH sides of it.
//
// Handing the calls to dd-pits used to end with an instruction: "mute
// FPVTrackside's own audio output". The operator did — and months later, handing
// the calls BACK, nothing un-muted it. FPVTrackside logged every lap it meant to
// say (the log line is written before its mute check) while its speaker was
// never launched, and the operator was left auditing a settings screen full of
// audio options for the one that mattered.
//
// What FPVTrackside's mute actually is, learned the hard way:
//   - At STARTUP it derives from ProfileSettings.xml: muted iff
//     TextToSpeechVolume == 0 (EventLayer wires MuteTTS = !TextToSpeech, and
//     TextToSpeech is just TextToSpeechVolume > 0).
//   - The mute BUTTON in its status bar is runtime-only — nothing writes the
//     volume back, so a restart resets to whatever the file says.
//   - FPVTrackside REWRITES the settings file on exit, so editing it while
//     FPVTrackside runs is futile: the exit clobbers the edit.
//
// So dd-pits writes the file only when FPVTrackside is NOT running, and
// otherwise says the one exact thing to do — restart FPVTrackside, which both
// applies the file and clears any runtime mute.

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ftTTSVolumeRE matches the one value that decides FPVTrackside's speech mute.
var ftTTSVolumeRE = regexp.MustCompile(`(<TextToSpeechVolume>)\d+(</TextToSpeechVolume>)`)

// ftProfileSettingsPath is where the running profile's settings live.
func ftProfileSettingsPath(cc localCalls) string {
	dir := cc.TracksideDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, "Documents", "FPVTrackside")
	}
	profile := cc.Profile
	if profile == "" {
		profile = "Profile 1"
	}
	return filepath.Join(dir, "data", profile, "ProfileSettings.xml")
}

// ftRunning reports whether FPVTrackside is alive.
//
// The PROCESS is checked, not just the port: FPVTrackside only starts its web
// server once an event is open, so a port probe said "not running" about a
// program that was sitting at its event picker — and the file written on the
// strength of that was never read (it had started already) and would have been
// clobbered by its exit rewrite. Observed, not hypothetical.
func ftRunning() bool {
	// Exact process names, never -f: FPVTrackside spawns an ffmpeg child whose
	// COMMAND LINE contains the install path, so a full-line match reported
	// "running" long after the app itself had quit — the file was never
	// corrected and the banner asked forever. Caught by the first end-to-end
	// swap test, not by review.
	for _, name := range []string{"FPVMacsideCore", "FPVTrackside"} {
		if out, err := exec.Command("pgrep", "-x", name).Output(); err == nil &&
			len(strings.TrimSpace(string(out))) > 0 {
			return true
		}
	}
	conn, err := net.DialTimeout("tcp", "localhost:8080", 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ftWriteSpeechVolume edits the one value in place, touching nothing else —
// the master Volume beside it also covers the beeps, which were never asked to
// change. Separate from the policy above so it can be tested without a live
// FPVTrackside skewing the result (the first version of its test failed
// because the REAL one was running on this machine and the guard refused).
func ftWriteSpeechVolume(path string, volume int) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !ftTTSVolumeRE.Match(raw) {
		return fmt.Errorf("no TextToSpeechVolume in %s — has FPVTrackside run once?", path)
	}
	return os.WriteFile(path, ftTTSVolumeRE.ReplaceAll(raw,
		fmt.Appendf(nil, "${1}%d${2}", volume)), 0o644)
}

// ftSetSpeechVolume writes FPVTrackside's TTS volume: 0 silences its calls,
// 100 restores them. Returns a human sentence describing what happened and
// what, if anything, the operator still has to do.
func ftSetSpeechVolume(cc localCalls, volume int) string {
	path := ftProfileSettingsPath(cc)
	if path == "" {
		return "FPVTrackside's settings could not be located — set its speech volume by hand."
	}
	if ftRunning() {
		// Writing now would be undone by its exit rewrite — but the reconciler
		// corrects the file within seconds of it closing, so the instruction is
		// the same in every ordering and cannot be done wrong.
		return "Restart FPVTrackside — the moment it closes, dd-pits sets its speech " +
			"volume to match, and it starts up correct."
	}

	if err := ftWriteSpeechVolume(path, volume); err != nil {
		return "FPVTrackside's settings could not be updated (" + err.Error() + ") — set its " +
			"speech volume by hand."
	}
	if volume > 0 {
		return "FPVTrackside's own speech has been switched on for the next time it starts."
	}
	return "FPVTrackside's own speech has been muted for the next time it starts — no need to " +
		"touch its audio settings."
}

// ftDesiredVolume is what FPVTrackside's speech volume SHOULD be, derived from
// who speaks — never stored, so it cannot disagree with the switch.
func ftDesiredVolume(source string) int {
	if source == "trackside" {
		return 100
	}
	return 0
}

// ftCurrentVolume reads the value on disk, -1 when unknowable.
func ftCurrentVolume(cc localCalls) int {
	raw, err := os.ReadFile(ftProfileSettingsPath(cc))
	if err != nil {
		return -1
	}
	m := ftTTSVolumeRE.FindSubmatch(raw)
	if m == nil {
		return -1
	}
	var v int
	_, _ = fmt.Sscanf(string(raw[len(m[1])+int(ftTTSVolumeRE.FindIndex(raw)[0]):]), "%d", &v)
	return v
}

// ftVolumeReconcileLoop keeps FPVTrackside's file matching the who-speaks
// switch, writing whenever FPVTrackside is not running to be clobbered by.
//
// This exists because the one-shot write at save time has a sequencing hole:
// with FPVTrackside running, the write is refused (its exit rewrites the file),
// and the operator is handed a two-step dance that can be done in the wrong
// order. A reconciler has no order: quit FPVTrackside at ANY time and within a
// couple of seconds the file is corrected, so "restart FPVTrackside" is always
// the whole instruction. It also survives dd-pits' own self-restart, which the
// save-time goroutine did not.
//
// dd-pits OWNS this value by design — the operator asked for exactly that,
// because FPVTrackside's audio settings are many and this is the one that
// matters. Hand-editing it while dd-pits runs will be corrected; the switch on
// the calls page is the control.
func ftVolumeReconcileLoop(ctx context.Context, cfg *localConfig, hasVoice bool) {
	// One second, not three: the write must land in the window between
	// FPVTrackside dying (its exit rewrite already done — the process writes
	// its old state WHILE dying, so "seen closed" guarantees we write after
	// the clobber, never before) and the operator starting it again. Humans
	// relaunch in seconds; the poll has to be faster than they are.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if cfg == nil {
			continue
		}
		cc := cfg.CallsConfig()
		want := ftDesiredVolume(callsSource(cc, hasVoice))
		cur := ftCurrentVolume(cc)
		if ftRunning() {
			continue
		}
		// FPVTrackside is closed: correct the file if it differs, and retire
		// the "restart FPVTrackside" banner — its next start reads this file,
		// so closing was the whole thing the operator owed.
		if cur >= 0 && cur != want {
			if err := ftWriteSpeechVolume(ftProfileSettingsPath(cc), want); err == nil {
				log.Printf("race calls: FPVTrackside's speech volume set to %d while it was "+
					"closed — it will be correct when it next starts", want)
			}
			// Re-verified: if FPVTrackside started in the instant between the
			// liveness check and the write, it read the OLD value and its exit
			// will clobber ours — so nothing is marked done, the banner keeps
			// asking, and the next close settles it.
			if ftRunning() || ftCurrentVolume(cc) != want {
				continue
			}
		}
		if cc.FTRestartNeeded {
			cc.FTRestartNeeded = false
			cc.FTRestartDone = true
			cfg.SetCalls(cc)
			_ = cfg.Save()
			log.Printf("race calls: FPVTrackside was closed — its audio setting is now correct")
		}
	}
}

package main

// Finding FPVTrackside's files.
//
// dd-pits needs two of them and gets nothing useful without them: Sounds.xml,
// which carries the WORDING of every call so a venue's own phrasing is used,
// and the log, which is how it hears the calls it cannot work out from timing
// data — "Hurry Up" is a keypress and invisible from outside the program.
//
// Both were assumed to live under ~/Documents/FPVTrackside. When that is wrong
// there are simply no race calls, with nothing on screen explaining why, and an
// operator has no reason to suspect a path. So rather than ask someone to type
// one, this looks in the places it is actually installed and offers what it
// finds.

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// tracksideInstall is somewhere FPVTrackside's data was found.
type tracksideInstall struct {
	Dir      string   // the FPVTrackside folder itself
	Profiles []string // profile folders inside it
	HasLog   bool     // whether the log is there too
}

// candidateTracksideDirs lists where FPVTrackside plausibly lives.
//
// Ordered by likelihood rather than alphabetically, because the first hit is
// what gets offered as the default and the usual install should win.
func candidateTracksideDirs() []string {
	var out []string
	add := func(parts ...string) {
		if p := filepath.Join(parts...); p != "" {
			out = append(out, p)
		}
	}

	home, err := os.UserHomeDir()
	if err == nil {
		add(home, "Documents", "FPVTrackside") // the installer's default
		add(home, "FPVTrackside")
		add(home, "Documents", "FPVTracksideCore")
		add(home, "Desktop", "FPVTrackside")
		add(home, "Downloads", "FPVTrackside")
		// OneDrive redirects Documents on a lot of Windows machines, and the
		// resulting path is one nobody would think to type.
		add(home, "OneDrive", "Documents", "FPVTrackside")
	}

	switch runtime.GOOS {
	case "windows":
		// A race laptop very often keeps it on a second drive, and a portable
		// install at a drive root is common at clubs that pass the laptop
		// around.
		for _, drive := range []string{"C:", "D:", "E:", "F:"} {
			add(drive+`\`, "FPVTrackside")
			add(drive+`\`, "FPVTracksideCore")
		}
	case "darwin":
		add("/Applications", "FPVTrackside")
	}
	return out
}

// inspectTracksideDir reports what is actually in a folder, or nil.
//
// A folder only counts when it holds at least one profile with a Sounds.xml —
// the point is to find a USABLE install, not a directory with the right name.
func inspectTracksideDir(dir string) *tracksideInstall {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	profiles := listTracksideProfiles(dir)
	if len(profiles) == 0 {
		return nil
	}
	_, err := os.Stat(filepath.Join(dir, "log", "Log.txt"))
	return &tracksideInstall{Dir: dir, Profiles: profiles, HasLog: err == nil}
}

// listTracksideProfiles returns the profile folders that contain a Sounds.xml.
//
// Profiles are how FPVTrackside keeps separate setups, and a club commonly has
// more than one — the wording differs between them, so picking the wrong one
// produces calls in someone else's phrasing rather than an error.
func listTracksideProfiles(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, "data"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "data", e.Name(), "Sounds.xml")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// findTracksideInstalls returns every usable install found, best first.
func findTracksideInstalls() []tracksideInstall {
	seen := map[string]bool{}
	var out []tracksideInstall
	for _, dir := range candidateTracksideDirs() {
		clean := filepath.Clean(dir)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if in := inspectTracksideDir(clean); in != nil {
			out = append(out, *in)
		}
	}
	return out
}

package main

// The Python sidecars ship INSIDE the binary.
//
// They used to be plain files copied next to the executable, which meant the
// source of truth (cmd/dd-pits/*.py) and the thing actually run (bin/*.py) were
// two different files that drifted apart silently. That is not a theoretical
// risk — it cost a debugging session here: a fix to the audio sidecar was
// edited, built, and restarted, and the agent went on running a copy three
// hours old, reporting the exact error the fix removed. Nothing warns about it,
// because a stale-but-valid script runs perfectly well.
//
// It is also the worse failure at a venue: `make build-dd-pits` cross-compiles
// five platforms, and a sidecar left behind in that copy would be a Windows
// race PC running last release's audio path with this release's agent.
//
// So the scripts are embedded and written out to a per-version cache directory
// on first use. The bytes that run are the bytes that were built, always. A
// file placed beside the executable still wins, so a venue can hot-patch a
// sidecar in the field without a rebuild — but it has to be a deliberate act
// rather than a leftover.

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
)

//go:embed local_audio_out.py local_tts_server.py silent_speaker.py
var sidecarFS embed.FS

// sidecarPath returns a path to run the named sidecar from.
//
// Order matters: an explicit copy beside the executable is honoured first so a
// venue can replace one without waiting for a build, and the embedded copy is
// the fallback that is always correct.
func sidecarPath(name string) string {
	if p := besideExecutable(name); p != "" {
		log.Printf("sidecar: using %s from beside the agent, NOT the built-in copy", p)
		return p
	}

	data, err := sidecarFS.ReadFile(name)
	if err != nil {
		return "" // not embedded: caller reports the missing sidecar
	}

	// Keyed by content, so a new build lands in a new file rather than
	// overwriting one a running agent may have open, and an unchanged script
	// is written once and reused.
	sum := sha256.Sum256(data)
	dir := filepath.Join(os.TempDir(), "dd-pits-sidecars", hex.EncodeToString(sum[:8]))
	out := filepath.Join(dir, name)

	if _, err := os.Stat(out); err == nil {
		return out
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("sidecar: cannot create %s: %v", dir, err)
		return ""
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		log.Printf("sidecar: cannot write %s: %v", out, err)
		return ""
	}
	return out
}

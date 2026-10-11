//go:build darwin

package main

// Restart by SPAWNING a successor, not by exec.
//
// The exec-based restart wedged three separate times on macOS, always the same
// way: the successor alive but serving nothing, logging nothing — hung before
// its first line of main. Releasing the audio device before exec did not fix
// it, which rules out the tidy theory and leaves the untidy fact: this process
// carries CoreAudio state, Metal/MLX sidecar children and a dozen framework
// threads, and exec'ing over that on macOS is where fresh images go to hang.
//
// So it forks instead, the way the Windows path always has: start a detached
// successor with the same arguments, give it our stdout/stderr (the log file
// carries on), and exit. The successor retries the port for 30s (listenRetry),
// which covers the overlap while this process leaves.

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// restartSelf hands over to a fresh copy of this binary. On success it exits
// this process and does not return.
func restartSelf(exePath string) error {
	releaseAudioForRestart() // free the device for the successor

	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Detached: its own session, so nothing that happens to this process's
	// terminal or group can take the successor down with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err // still running, still racing — a failed restart must not kill us
	}
	// Give it a moment to be alive at all before we abandon the port to it.
	time.Sleep(300 * time.Millisecond)
	os.Exit(0)
	return nil
}

// releaseAudioForRestart frees the in-process audio device, best-effort.
func releaseAudioForRestart() {
	if f := audioRestartRelease.Load(); f != nil {
		(*f)()
	}
}

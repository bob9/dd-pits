//go:build !windows && !darwin

package main

import (
	"os"
	"syscall"
	"time"
)

// restartSelf replaces this process with the new binary via exec — same PID,
// same terminal, same arguments and environment. There is no child process, so
// the successor can't be orphaned or taken down with a console, and a failed
// exec leaves this process fully intact and still racing.
//
// Does not return on success.
func restartSelf(exePath string) error {
	// The audio device is let go FIRST. exec replaces a process that may hold a
	// live CoreAudio connection (the in-process player), and framework state
	// across exec is exactly where a successor hangs: observed as a restarted
	// agent alive for seven minutes without serving a byte or logging a line.
	// The successor opens the device fresh, so releasing costs nothing.
	releaseAudioForRestart()
	return syscall.Exec(exePath, os.Args, os.Environ())
}

// releaseAudioForRestart closes whatever holds the audio hardware, best-effort.
func releaseAudioForRestart() {
	if f := audioRestartRelease.Load(); f != nil {
		(*f)()
		time.Sleep(150 * time.Millisecond) // let CoreAudio actually detach
	}
}

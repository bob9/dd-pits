//go:build !windows

package main

import (
	"os"
	"syscall"
)

// flushDevice tells the player to throw away whatever the audio device has
// buffered, rather than letting it play out.
//
// Ducking a cue stops dd-pits generating it, but samples already handed to
// CoreAudio still play — measured at 406ms on Bluetooth, and heard as audio
// continuing after the Start button. A signal is used because the player's
// stdin carries raw PCM with no room for an in-band control message.
func flushDevice(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Signal(syscall.SIGUSR1)
}

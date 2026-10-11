//go:build !darwin && !windows

package main

// Platforms where dd-pits cannot own the audio device.
//
// oto reaches the device through ALSA on Linux, which needs CGo — and that would
// end the CGo-free cross-compile of all five targets from one machine (measured:
// darwin/arm64, darwin/amd64 and windows/amd64 build clean with purego;
// linux/amd64 and linux/arm64 do not). linux/arm64 is the Raspberry Pi, so the
// platform that would most benefit from in-process cancellation is precisely the
// one that cannot have it without breaking how the agent is shipped.
//
// So Linux keeps the Python sidecar. It is the path that has always worked
// there, and the cut still reaches everything except the sidecar's own buffer.

import (
	"context"
	"fmt"
	"sync/atomic"
)

// audioRestartRelease mirrors the darwin/windows declaration; the sidecar path
// holds no in-process device, so nothing ever registers here.
var audioRestartRelease atomic.Pointer[func()]

func goPlayerAvailable() bool { return false }

func startGoPlayer(context.Context, *boothMixer) (audioSink, error) {
	return nil, fmt.Errorf("in-process audio needs CGo on this platform — using the sidecar")
}

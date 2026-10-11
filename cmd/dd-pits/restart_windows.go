//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Windows has no exec, so the successor is a separate process — and getting
// that handover wrong takes a venue offline mid-meeting. It did: an auto-update
// spawned the successor holding the parent's console handles and no detach
// flags, the parent exited a microsecond later without checking anything, the
// child went with the console, and the field ran blind for 13 minutes until an
// operator noticed and started it by hand.
//
// Two rules fix it, and both matter:
//
//  1. DETACH. CREATE_NEW_PROCESS_GROUP keeps the parent's Ctrl-C / Ctrl-Break
//     away from the child; CREATE_NEW_CONSOLE gives it a console of its own, so
//     the parent's window closing (an operator's shell returning to a prompt, a
//     .bat terminating, someone tidying up windows) can't deliver
//     CTRL_CLOSE_EVENT to it. The operator gets a fresh window showing the new
//     build's logs, tied to nothing we own.
//
//  2. VERIFY, THEN EXIT. Start() only means the process object was created. We
//     stay alive and watch for restartVerifyWindow: if the successor dies in
//     that window we return an error, and applyUpdate rolls the binary back and
//     keeps this process racing on the build we know works. Only a successor
//     still standing at the end of the window earns our exit.
const (
	// restartVerifyWindow is how long we watch the successor before handing
	// over. Long enough to catch the failure that matters — dying on startup
	// (bad binary, missing DLL, a fatal flag/config error) — without leaving
	// two agents contending for the netpack and the local port for long. That
	// contention is survivable either way: the backpack reconnects with
	// backoff and the successor's local server retries a busy port for
	// listenRetry, which is far longer than this window.
	restartVerifyWindow = 4 * time.Second

	// Process-creation flags (the golang.org/x/sys/windows constants, inlined
	// so this file adds no dependency).
	createNewConsole      = 0x00000010
	createNewProcessGroup = 0x00000200
)

// restartSelf launches the newly-installed binary as a detached successor and,
// once it has proven it can stay up, exits this process. A returned error means
// the successor did NOT take over and this process is still fully operational —
// the caller rolls the binary back.
func restartSelf(exePath string) error {
	cmd := exec.Command(exePath, os.Args[1:]...)
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd // relative paths in the arguments must still resolve
	}
	// Deliberately NOT inheriting our stdio: the successor owns a new console,
	// and handing it our handles would tie it to a window that is about to go
	// away — the exact coupling that caused the outage.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewConsole | createNewProcessGroup,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn successor: %w", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case werr := <-exited:
		if werr == nil {
			return fmt.Errorf("successor exited immediately (clean exit) — see its console window")
		}
		return fmt.Errorf("successor exited immediately: %w", werr)
	case <-time.After(restartVerifyWindow):
		// Still standing. Hand over: our local listener is already released
		// (see applyUpdate) and the netpack link drops as we exit, which the
		// successor's reconnect backoff picks up within a few seconds.
		os.Exit(0)
		return nil // unreachable
	}
}

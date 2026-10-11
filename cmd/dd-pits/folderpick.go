package main

// Choosing a folder with the operating system's own picker.
//
// A browser cannot do this. `<input type="file" webkitdirectory>` hands back
// file NAMES, never the absolute path — deliberately, because a web page has no
// business learning where things are on your disk. Which leaves an operator
// typing "/Users/glenn/Documents/FPVTrackside" by hand, and a typo produces no
// race calls with nothing on screen explaining why.
//
// dd-pits is not a web page though: it is a program running on that same
// machine, and it can open Finder. So the browser asks the agent to show a
// picker, the operator clicks a folder, and the path comes back.
//
// LOCALHOST ONLY, enforced by the caller. Opening a file dialog on the timing
// laptop because someone on the venue wifi asked would be both a security
// problem and — worse at a meeting — a modal window nobody is standing in front
// of, blocking the machine running the race.

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// pickFolderTimeout bounds the wait.
//
// The dialog is modal on the operator's own screen, so a long wait is normal
// while they navigate — but an abandoned dialog must not hold a request open
// for the rest of the meeting.
const pickFolderTimeout = 3 * time.Minute

// pickFolder opens the native folder chooser and returns what was picked.
//
// An empty path with no error means the operator cancelled, which is a normal
// outcome and not something to report as a failure.
func pickFolder(prompt, start string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pickFolderTimeout)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		return pickFolderMac(ctx, prompt, start)
	case "windows":
		return pickFolderWindows(ctx, prompt)
	default:
		return pickFolderLinux(ctx, prompt, start)
	}
}

func pickFolderMac(ctx context.Context, prompt, start string) (string, error) {
	// "POSIX path of" because AppleScript otherwise returns a colon-separated
	// HFS path, which nothing else on the system understands.
	script := fmt.Sprintf(`POSIX path of (choose folder with prompt %q)`, prompt)
	if start != "" {
		script = fmt.Sprintf(
			`POSIX path of (choose folder with prompt %q default location POSIX file %q)`,
			prompt, start)
	}
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		// A cancelled dialog exits non-zero. Reported as "nothing chosen"
		// rather than as an error, because cancelling is a normal thing to do.
		return "", nil
	}
	return strings.TrimRight(strings.TrimSpace(string(out)), "/"), nil
}

func pickFolderWindows(ctx context.Context, prompt string) (string, error) {
	// STA is required: the folder dialog is a COM control and silently does
	// nothing on the default multi-threaded apartment.
	ps := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = %q
$d.ShowNewFolderButton = $false
if ($d.ShowDialog() -eq 'OK') { Write-Output $d.SelectedPath }`, prompt)

	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-STA",
		"-Command", ps).Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

func pickFolderLinux(ctx context.Context, prompt, start string) (string, error) {
	args := []string{"--file-selection", "--directory", "--title=" + prompt}
	if start != "" {
		args = append(args, "--filename="+strings.TrimRight(start, "/")+"/")
	}
	out, err := exec.CommandContext(ctx, "zenity", args...).Output()
	if err != nil {
		// zenity may not be installed at all, which is not a failure worth
		// stopping for — the operator can still type the path.
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// canPickFolder reports whether a native picker is likely to work, so the
// button is only offered when it can do something.
//
// The check is for the TOOL, not for a display: a headless machine will simply
// return nothing, and the typed field is always there as the fallback.
func canPickFolder() bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := exec.LookPath("osascript")
		return err == nil
	case "windows":
		_, err := exec.LookPath("powershell")
		return err == nil
	default:
		_, err := exec.LookPath("zenity")
		return err == nil
	}
}

package main

// Opening the setup pages for someone who has just double-clicked the agent.
//
// A club's first experience of dd-pits is a terminal window full of log lines.
// Everything they need to configure is in a browser, and nothing on that screen
// says so — the one line that does scrolls away within seconds of a meeting
// loading. So on a FIRST RUN the agent opens the help page itself.
//
// Only on a first run: a race director restarting the agent between heats does
// not want a browser tab every time, and an agent started by a script or a
// service must never spawn a window at all.

import (
	"log"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// firstRunMarker sits beside the settings file. Its presence is the whole test
// for "has this venue been set up before" — deliberately not a timestamp or a
// version, because the question is only ever asked once.
func firstRunMarker(configPath string) string { return configPath + ".seen" }

// isFirstRun reports whether this venue has never been configured.
//
// Keyed on the settings file rather than an install directory: copying a
// configured file to a new laptop should NOT re-open the browser, because that
// venue is already set up.
func isFirstRun(configPath string) bool {
	if _, err := os.Stat(configPath); err == nil {
		return false // already configured
	}
	if _, err := os.Stat(firstRunMarker(configPath)); err == nil {
		return false // shown once already, and they chose not to configure yet
	}
	return true
}

// markFirstRunSeen records that the browser has been offered, so a venue that
// starts the agent repeatedly without configuring is not nagged every time.
func markFirstRunSeen(configPath string) {
	_ = os.WriteFile(firstRunMarker(configPath), []byte(
		"dd-pits opened the setup pages once. Delete this file to be shown them again.\n"), 0o644)
}

// openBrowser asks the operating system to open a URL.
//
// Best-effort by design: a headless venue machine, a locked-down laptop, or an
// agent started as a service will all fail here, and none of those is a reason
// to interrupt a race day. The URL is logged either way, which is the part that
// actually matters.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// rundll32 rather than "cmd /c start": start treats a bare URL
		// containing & as a command separator, and the setup URLs carry query
		// parameters.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("dd-pits: could not open a browser (%v) — open %s yourself", err, url)
		return
	}
	// Reaped so the child does not linger as a zombie for the life of a race
	// day; the browser itself has already detached by then.
	go func() { _ = cmd.Wait() }()
}

// offerSetupPages opens the help page the first time a venue runs the agent.
//
// Delayed slightly so the browser does not arrive before the server is
// listening — the page loading with a connection error would be a worse
// introduction than not opening at all.
func offerSetupPages(configPath, base string) {
	if !isFirstRun(configPath) {
		return
	}
	markFirstRunSeen(configPath)

	log.Printf("dd-pits: first run — opening the setup guide at %s/setup/help", base)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		openBrowser(base + "/setup/help")
	}()
}

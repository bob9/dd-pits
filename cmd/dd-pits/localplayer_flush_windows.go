//go:build windows

package main

import "os"

// flushDevice is a no-op on Windows: there is no SIGUSR1, so the device buffer
// plays out as it did before. The audio still STOPS — the bus is cleared either
// way — it just takes the length of the device buffer to fall silent.
func flushDevice(p *os.Process) {}

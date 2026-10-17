package main

// The log file, and what the Status page shows about the timer connection.
//
// dd-pits keeps its log in a file beside the settings file (dd-pits.log) as
// well as on screen, so what happened at a race day is still there after the
// window is closed. The file rolls over at logFileMax, keeping one old copy
// (dd-pits.log.1), so a season of race days never fills the disk.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const logFileMax = 5 << 20 // 5 MB

// rollingLog is an io.Writer onto dd-pits.log that rolls over when it gets big.
type rollingLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func logFilePath(configPath string) string {
	if configPath == "" {
		configPath = defaultLocalConfigPath()
	}
	return filepath.Join(filepath.Dir(configPath), "dd-pits.log")
}

func openRollingLog(path string) (*rollingLog, error) {
	l := &rollingLog{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *rollingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	n, _ := fmt.Fprintf(f, "\n──── dd-pits started %s ────\n", time.Now().Format("2006-01-02 15:04:05"))
	l.size += int64(n)
	return nil
}

func (l *rollingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(p), nil
	}
	if l.size+int64(len(p)) > logFileMax {
		l.f.Close()
		_ = os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			l.f = nil
			return len(p), nil // never let logging stop dd-pits
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// timerState is what the Status page says about the timer connection.
type timerState struct {
	mu       sync.Mutex
	ok       bool
	lastOK   time.Time
	lastErr  string
	failures int
	since    time.Time // when the current state (connected / not) began
}

var timerStatus = &timerState{since: time.Now()}

func (t *timerState) good() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ok {
		t.since = time.Now()
	}
	t.ok, t.lastOK, t.lastErr, t.failures = true, time.Now(), "", 0
}

func (t *timerState) bad(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ok || t.failures == 0 {
		t.since = time.Now()
	}
	t.ok = false
	t.failures++
	t.lastErr = err.Error()
}

func (t *timerState) get() (ok bool, lastOK time.Time, lastErr string, since time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ok, t.lastOK, t.lastErr, t.since
}

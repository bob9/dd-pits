package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRollingLogRollsOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dd-pits.log")
	l, err := openRollingLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close() // Windows can't delete an open file at cleanup
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for i := 0; i < (logFileMax/len(line))+50; i++ {
		l.Write(line)
	}
	old, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal("no rolled-over log")
	}
	cur, _ := os.Stat(path)
	if old.Size() > logFileMax || cur.Size() > logFileMax {
		t.Fatalf("logs too big: %d and %d", old.Size(), cur.Size())
	}
}

func TestTimerStatus(t *testing.T) {
	ts := &timerState{}
	ts.bad(errors.New(`Get "http://x/": dial tcp: connect: connection refused`))
	if ok, _, e, _ := ts.get(); ok || !strings.Contains(plainTimerError(e), "nothing is answering") {
		t.Fatalf("bad state wrong: %v %q", ok, e)
	}
	ts.good()
	if ok, last, e, _ := ts.get(); !ok || last.IsZero() || e != "" {
		t.Fatal("good state wrong")
	}
}

func TestShouldOpenBrowser(t *testing.T) {
	if shouldOpenBrowser(true, false) || shouldOpenBrowser(false, true) {
		t.Fatal("-no-browser and the RotorHazard plugin must not open a browser")
	}
	if localServeURL(":8088") != "http://localhost:8088" || localServeURL("127.0.0.1:9") != "http://127.0.0.1:9" {
		t.Fatal("localServeURL")
	}
}

package main

// Does generating audio in PARALLEL actually help?
//
// The question matters because of a specific limit heard on air: the first lap
// call of a burst reaches audio in ~110ms, and the second waits 300-1800ms
// behind it. One sidecar renders one line at a time, so a four-pilot heat
// queues. If a second renderer genuinely doubled throughput, mid-race
// commentary could run alongside the calls instead of being held out of the way.
//
// It is worth MEASURING rather than assuming, because both renderers run MLX on
// one GPU and Metal serialises there. Two processes may simply take turns —
// buying nothing and costing a second copy of the model in memory. This is the
// same mistake made earlier in this work, where two sidecars were introduced to
// remove pipe contention and left GPU contention untouched.
//
// Opt-in: needs the real model. RUN_TTS_BENCH=1 go test ./cmd/dd-pits/ -run Parallel -v

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/tts"
)

// The benchmark needs a real local voice, so it is configured from the
// environment and skipped when unset:
//
//	DD_PITS_BENCH_PYTHON   python with mlx-audio installed
//	DD_PITS_BENCH_REF      reference wav to clone the voice from
//	DD_PITS_BENCH_REF_TEXT exactly what is said in the reference wav
var (
	benchPython = os.Getenv("DD_PITS_BENCH_PYTHON")
	benchScript = "local_tts_server.py"
	benchRef    = os.Getenv("DD_PITS_BENCH_REF")
	benchRefTxt = os.Getenv("DD_PITS_BENCH_REF_TEXT")
)

// benchCalls are real lap calls, distinct so nothing is served from a cache.
var benchCalls = []string{
	"bob9 lap 1 in 11.42 in 1st",
	"Fizz lap 1 in 11.50 in 2nd",
	"Willman lap 1 in 12.07 in 3rd",
	"Wizz lap 1 in 12.61 in 4th",
	"bob9 lap 2 in 11.38 in 1st",
	"Fizz lap 2 in 11.44 in 2nd",
}

func benchEngine(t *testing.T) *tts.LocalEngine {
	t.Helper()
	if benchPython == "" {
		t.Skip("DD_PITS_BENCH_PYTHON not set")
	}
	e, err := tts.StartLocal(context.Background(), tts.LocalConfig{
		Python:         benchPython,
		Script:         benchScript,
		Model:          "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-bf16",
		ReferenceAudio: benchRef,
		ReferenceText:  benchRefTxt,
	})
	if err != nil {
		t.Skipf("no local voice: %v", err)
	}
	return e
}

func skipUnlessBench(t *testing.T) {
	if os.Getenv("RUN_TTS_BENCH") != "1" {
		t.Skip("set RUN_TTS_BENCH=1 (loads the real model)")
	}
}

// streamAll renders a list on one engine, returning total wall time and the
// time to FIRST audio for each — the number that decides whether a call lands
// on the lap.
func streamAll(t *testing.T, e *tts.LocalEngine, calls []string) (time.Duration, []time.Duration) {
	t.Helper()
	start := time.Now()
	firsts := make([]time.Duration, 0, len(calls))
	for _, c := range calls {
		t0 := time.Now()
		ch, err := e.SpeakStream(context.Background(), c)
		if err != nil {
			t.Fatalf("stream %q: %v", c, err)
		}
		first := time.Duration(0)
		for pcm := range ch {
			if first == 0 && len(pcm) > 0 {
				first = time.Since(t0)
			}
		}
		firsts = append(firsts, first)
	}
	return time.Since(start), firsts
}

// TestParallelRenderThroughput compares one renderer against two.
func TestParallelRenderThroughput(t *testing.T) {
	skipUnlessBench(t)

	one := benchEngine(t)
	seqTotal, seqFirsts := streamAll(t, one, benchCalls)
	one.Close()
	fmt.Printf("\n  ONE renderer : %d calls in %.2fs\n", len(benchCalls), seqTotal.Seconds())
	for i, d := range seqFirsts {
		fmt.Printf("      call %d first audio %6dms\n", i+1, d.Milliseconds())
	}

	// Two engines, each taking half the list at the same time.
	a, b := benchEngine(t), benchEngine(t)
	defer a.Close()
	defer b.Close()

	half := len(benchCalls) / 2
	var wg sync.WaitGroup
	var parA, parB time.Duration
	var firstsA, firstsB []time.Duration

	start := time.Now()
	wg.Add(2)
	go func() { defer wg.Done(); parA, firstsA = streamAll(t, a, benchCalls[:half]) }()
	go func() { defer wg.Done(); parB, firstsB = streamAll(t, b, benchCalls[half:]) }()
	wg.Wait()
	parTotal := time.Since(start)

	fmt.Printf("\n  TWO renderers: %d calls in %.2fs (halves took %.2fs / %.2fs)\n",
		len(benchCalls), parTotal.Seconds(), parA.Seconds(), parB.Seconds())
	for i, d := range append(append([]time.Duration{}, firstsA...), firstsB...) {
		fmt.Printf("      call %d first audio %6dms\n", i+1, d.Milliseconds())
	}

	speedup := seqTotal.Seconds() / parTotal.Seconds()
	fmt.Printf("\n  SPEED-UP: %.2fx  (1.0 = the GPU serialises them and a second renderer buys nothing)\n\n", speedup)

	// Not asserted as pass/fail: the point is the number, and what it means
	// depends on the hardware in front of it. A regression guard here would
	// fail on a different machine for no useful reason.
	if speedup < 1.2 {
		t.Logf("VERDICT: parallel rendering does NOT help (%.2fx) — Metal is serialising, "+
			"so a second renderer costs memory and buys nothing", speedup)
	} else {
		t.Logf("VERDICT: parallel rendering helps (%.2fx) — worth a second renderer so the "+
			"second pilot's call does not queue behind the first", speedup)
	}
}

// TestCallLatencyUnderCommentaryLoad measures what actually matters for the
// yield decision: does generating commentary slow a race call?
//
// Commentary is currently held for the whole heat because a 17-31s generation
// was starving the calls. If the cost is now small, mid-race colour could run
// alongside them.
func TestCallLatencyUnderCommentaryLoad(t *testing.T) {
	skipUnlessBench(t)

	e := benchEngine(t)
	defer e.Close()

	_, idle := streamAll(t, e, benchCalls[:3])
	fmt.Printf("\n  calls with the GPU IDLE:\n")
	for i, d := range idle {
		fmt.Printf("      call %d first audio %6dms\n", i+1, d.Milliseconds())
	}

	// A second renderer standing in for the commentary load: it occupies the
	// GPU the same way, without needing the 30B chat model loaded here.
	load := benchEngine(t)
	defer load.Close()

	stop := make(chan struct{})
	var lwg sync.WaitGroup
	lwg.Add(1)
	go func() {
		defer lwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ch, err := load.SpeakStream(context.Background(),
				"and that is a superb run from the whole field here today")
			if err != nil {
				return
			}
			for range ch {
			}
		}
	}()
	time.Sleep(2 * time.Second) // let the load settle

	_, busy := streamAll(t, e, benchCalls[3:])
	close(stop)
	lwg.Wait()

	fmt.Printf("\n  calls with commentary generating ALONGSIDE:\n")
	for i, d := range busy {
		fmt.Printf("      call %d first audio %6dms\n", i+1, d.Milliseconds())
	}

	mean := func(ds []time.Duration) float64 {
		var s float64
		for _, d := range ds {
			s += float64(d.Milliseconds())
		}
		return s / float64(len(ds))
	}
	i, b := mean(idle), mean(busy)
	fmt.Printf("\n  mean first audio: idle %.0fms -> under load %.0fms (%.1fx)\n\n", i, b, b/i)

	if b > i*2 {
		t.Logf("VERDICT: commentary DOES starve the calls (%.1fx) — keep holding it during a heat", b/i)
	} else {
		t.Logf("VERDICT: commentary costs little (%.1fx) — mid-race colour looks affordable", b/i)
	}
}

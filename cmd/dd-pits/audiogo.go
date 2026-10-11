//go:build darwin || windows

package main

// Playing the bus from inside dd-pits, with no sidecar in between.
//
// WHY
//
// Cancellation. When a heat goes green the queue is cut, and that cut has to
// reach everything already committed. Across a process boundary it cannot: the
// sidecar is handed a byte stream, so it can be told to drop what it holds but
// never told what any of it MEANT. Cue identity lives in Go, so the cut has to
// live in Go too. In-process, "stop talking" is a buffer reset rather than a
// signal, a pipe, and a hope.
//
// Latency is NOT the reason. The multi-second delays once blamed on Python were
// dd-pits' own unbounded queues — two 32-frame channels holding 3.2s each that
// never drained, because both ends run at realtime so one stall's backlog was
// permanent. That is fixed, and Python was playing faithfully the whole time.
//
// WHAT IT COSTS, AND WHY PYTHON STAYS
//
// oto has no device selection — no enumeration, no switching, no following the
// default output when it moves. Those exist because a tester's audio was going
// to the wrong device and there was no way to see it or change it. So this is
// not a replacement: it is the path when the DEFAULT output is wanted, and the
// sidecar remains the path when a specific device is named. Linux keeps the
// sidecar outright (oto needs ALSA via CGo there, which would end the CGo-free
// cross-compile of all five platforms from one machine).

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebitengine/oto/v3"
)

// audioRestartRelease is what a self-restart calls to drop the audio device
// before exec — a live CoreAudio connection across exec is how a successor
// wedges. Registered by whichever player currently owns the hardware.
var audioRestartRelease atomic.Pointer[func()]

// otoShared is the one oto context this process will ever have. oto enforces
// a per-process singleton — a second NewContext returns "context is already
// created" — and the audio path re-opens when settings change, so the second
// open must REUSE the context rather than fail. Observed live: the re-open
// fell back to the sidecar at 1567ms device latency, silently trading the
// in-process player's 200ms for the worst latency in the whole system.
var (
	otoSharedMu sync.Mutex
	otoShared   *oto.Context
)

// goPlayerAvailable reports that this platform can play in-process.
func goPlayerAvailable() bool { return true }

// busReader is the device's PULL on the mixer: each Read produces the next
// frame synchronously, silence included.
//
// It used to consume a channel a ticker goroutine filled, and block when the
// channel was empty. That chain — ticker wake, channel hand-off, reader
// goroutine, device — had three scheduling hops that could each be starved
// under GPU load, and were: the pump missed ticks while a 30B model wrote, the
// Read blocked, the device's 200ms drained, and the operator heard the click.
// Worse for a live stream: the producer was real-time paced, so a single
// stall left the device hovering at empty for the REST of the cue —
// persistent crackle, not one gap.
//
// A pull cannot run dry. Producing the frame is memory work measured in
// microseconds, done on the very goroutine oto reads from, and an idle bus
// yields silence rather than a block. The device buffer is therefore always
// full, and the only thing that can interrupt playback is the deliberate
// Flush of a cut.
type busReader struct {
	mu      sync.Mutex
	mixer   *boothMixer
	silence []byte
	pending []byte
	done    <-chan struct{}
}

func (r *busReader) Read(p []byte) (int, error) {
	select {
	case <-r.done:
		return 0, io.EOF
	default:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		r.pending = append(r.pending[:0], r.mixer.pullFrame(r.silence)...)
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// discard drops the part-played frame this reader is holding.
//
// Counted separately from the channel drain because they are different
// quantities: this is a fraction of one frame mid-flight, the channel is whole
// frames not yet started.
func (r *busReader) discard() {
	r.mu.Lock()
	r.pending = r.pending[:0]
	r.mu.Unlock()
}

// goPlayer plays the bus through the system default output.
type goPlayer struct {
	otoCtx *oto.Context
	player *oto.Player
	reader *busReader
	mixer  *boothMixer
	stop   chan struct{}
	once   sync.Once
}

// startGoPlayer subscribes to the mixer and plays it in-process.
func startGoPlayer(ctx context.Context, m *boothMixer) (audioSink, error) {
	if m == nil {
		return nil, fmt.Errorf("no audio bus to play")
	}

	// The device buffer: cover against the READER goroutine itself being
	// starved, and the floor on how late a cut can be heard.
	//
	// With the pull design this is the ONLY buffer — frames are produced
	// inside the device's own read, so there is no ticker or channel left to
	// starve, and the buffer's job shrinks to covering the one goroutine oto
	// reads on. 200ms held there; the idle-flush fast start means a race call
	// on a quiet bus no longer waits it out, so the margin costs nothing
	// where latency matters.
	bufSize := time.Duration(4*boothFrameMs) * time.Millisecond

	otoSharedMu.Lock()
	otoCtx := otoShared
	otoSharedMu.Unlock()
	if otoCtx == nil {
		fresh, ready, err := oto.NewContext(&oto.NewContextOptions{
			SampleRate:   boothSampleRate,
			ChannelCount: 1,
			Format:       oto.FormatSignedInt16LE,
			BufferSize:   bufSize,
		})
		if err != nil {
			return nil, fmt.Errorf("open audio device: %w", err)
		}
		// Waited for, but not forever: a device that never becomes ready must
		// not hold up the agent — /booth.wav and the goggles don't depend on it.
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("audio device did not become ready")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		otoSharedMu.Lock()
		otoShared = fresh
		otoSharedMu.Unlock()
		otoCtx = fresh
	}

	p := &goPlayer{
		otoCtx: otoCtx,
		mixer:  m,
		stop:   make(chan struct{}),
	}
	bufBytes := int(bufSize.Seconds() * float64(boothSampleRate*2))
	p.reader = &busReader{mixer: m, silence: make([]byte, boothFrameBytes), done: p.stop}
	p.player = otoCtx.NewPlayer(p.reader)
	p.player.SetBufferSize(bufBytes)

	// The device is the clock now: pullFrame produces on demand and tees to
	// the /booth.wav subscribers, and the ticker pump stands down.
	m.setDevicePaced(true, bufBytes)
	m.setFlush(p.Flush)

	p.player.Play()

	release := func() { _ = p.Close() }
	audioRestartRelease.Store(&release)

	go func() {
		<-ctx.Done()
		p.Close()
	}()

	log.Printf("audio out: playing in dd-pits (system default output, %dms buffer) — "+
		"a cut stops the audio here rather than asking another process to",
		int(bufSize.Milliseconds()))
	return p, nil
}

// Flush drops everything already committed.
//
// Three places, all ours and all exact: the frames we hold, the part-frame the
// reader is mid-way through, and the device buffer. That last one is what a
// sidecar can only be ASKED to do.
func (p *goPlayer) Flush() {
	if p == nil {
		return
	}
	p.reader.discard()
	if p.player != nil {
		// Reset is deprecated in favour of Seek, which is meaningless on a live
		// stream: there is no position to seek to, only audio that is now wrong.
		// Clearing the buffer is exactly the operation wanted.
		p.player.Reset()
		p.player.Play()
	}
	// Deliberately unlogged: this fires on every idle fast-start (a GOOD
	// event, dozens per heat) as well as on cuts, and the cut paths already
	// log themselves with their reason.
}

// Close stops playback and releases the device.
func (p *goPlayer) Close() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		close(p.stop)
		if p.mixer != nil {
			// Hand the clock back to the ticker so /booth.wav keeps flowing
			// for setups where the local device goes away.
			p.mixer.setDevicePaced(false, 0)
		}
		if p.player != nil {
			_ = p.player.Close()
		}
	})
	return nil
}

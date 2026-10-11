package main

// boothMixer: the booth's on-air audio bus. Cues queue up and play strictly
// in order; between cues the bus carries silence — so /booth.wav is one
// continuous 24kHz mono PCM stream OBS ingests as a media source and mixes
// straight into the outgoing YouTube stream. A cue's spoken-at wall clock is
// stamped the instant its first frame is pumped, which is what anchors the
// replay-commentary timestamps (booth_upload.go).

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	boothSampleRate = 24000
	// 50ms, not 100.
	//
	// Measured: a cue cannot start mid-frame, so EVERY call waits for the next
	// frame boundary — a full frame, not the half-frame average one would
	// assume, because the mixer picks the next cue at the tick. On an idle bus
	// that was 100ms on every call, cached ones included.
	//
	// Halving it halves that floor. It does not cost smoothness: the sidecar is
	// fed more often in smaller pieces, which is gentler on the device buffer
	// than the same audio in bigger lumps, and the buffer only has to cover how
	// late a frame can be.
	boothFrameMs    = 50
	boothFrameBytes = boothSampleRate * 2 * boothFrameMs / 1000 // 16-bit mono
)

type boothMixer struct {
	mu      sync.Mutex
	queue   []*boothCue
	cur     *boothCue
	curOff  int
	clients map[chan []byte]struct{}

	// onFlush discards whatever the audio device has buffered.
	//
	// Registered by the local player, because the mixer is the one object both
	// it and the announcer hold. Called only when the bus is cleared wholesale:
	// a partial duck must not throw away audio for cues that survived it.
	onFlush func()

	// What the last streaming cue actually played, for diagnosing a call heard
	// as cut off: comparing this against what was generated separates "short on
	// audio" from "dropped early".
	lastStreamText     string
	lastStreamPlayedMs int64

	// recent on-air lines for the /booth status page
	recent []string
	// lastAlignMs is the most recent scheduled cue's (spokenAt - playAt) in ms:
	// ~0 means it aired right on its delayed-video mark; >0 means generation
	// overran the buffer and it aired late (the sync controller then grows the
	// buffer so it can't happen again).
	lastAlignMs int64

	// devicePaced: the in-process player owns the clock, pulling frames
	// straight from the mixer as the hardware consumes them. The ticker pump
	// stands down entirely — two producers would consume the bus at double
	// speed — and serves only setups with no local device (/booth.wav-only).
	//
	// Pull is what makes the device buffer UNABLE to run dry: a pulled frame
	// is produced synchronously inside the read (silence when the bus is
	// idle), so there is no ticker wake, no channel hand-off and no second
	// goroutine between the mixer and the hardware to be starved under GPU
	// load. The push chain was measured failing exactly there: the pump
	// missed ticks while a 30B model wrote, the reader blocked on the empty
	// channel, and the device's 200ms drained into a click.
	devicePaced bool

	// deviceBufBytes is the in-process player's buffer size, and idleSilence
	// counts consecutive silence bytes pulled — together they prove the
	// device buffer holds NOTHING but silence, which is what makes the
	// idle-flush fast start safe (see enqueue).
	deviceBufBytes int
	idleSilence    int
}

// streamPlayed reports what the last streaming cue actually got through, for
// comparison against what was generated — a call heard as cut off mid-word is
// either short on audio or dropped early, and these two numbers separate them.
func (m *boothMixer) streamPlayed() (string, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastStreamText, m.lastStreamPlayedMs
}

func newBoothMixer() *boothMixer {
	return &boothMixer{clients: map[chan []byte]struct{}{}}
}

// appendPCM adds newly generated audio to a cue that is already playing.
func (m *boothMixer) appendPCM(c *boothCue, pcm []byte, rate int) {
	if len(pcm) == 0 {
		return
	}
	if rate > 0 && rate != boothSampleRate {
		pcm = resamplePCM16(pcm, rate, boothSampleRate)
	}
	m.mu.Lock()
	c.PCM = append(c.PCM, pcm...)
	c.DurationMs = int64(len(c.PCM)) * 1000 / int64(boothSampleRate*2)
	m.mu.Unlock()
}

// finishStream marks a streaming cue complete, so the bus stops holding for it.
func (m *boothMixer) finishStream(c *boothCue) {
	m.mu.Lock()
	c.streamDone = true
	m.mu.Unlock()
}

func (m *boothMixer) enqueue(c *boothCue) {
	// Commentary can never outrank a race call.
	//
	// The booth and the announcer both set priority on the same field, but on
	// scales that were chosen separately: the booth's big beats (finish,
	// holeshot) use 5, which is exactly callPriorityLap. A tie falls through to
	// arrival order, so a commentary cue that happened to be queued first went
	// to air ahead of a lap time — the opposite of the guarantee the single bus
	// exists to give.
	//
	// Clamped here rather than at each call site: this is the one place every
	// cue passes through, so no future beat can reintroduce the tie.
	//
	// It keys on an EXPLICIT flag, not on callKey being empty. A relayed call
	// whose sound could not be identified has no key either — seen live as
	// "call [] lap … Fizz 0.98" — and demoting those would push real lap times
	// behind commentary, which is the very fault this guards against.
	if c.commentary && c.priority >= callPriorityLap {
		c.priority = callPriorityLap - 1
	}

	if c.Rate > 0 && c.Rate != boothSampleRate {
		c.PCM = resamplePCM16(c.PCM, c.Rate, boothSampleRate)
		c.Rate = boothSampleRate
	}
	m.mu.Lock()
	m.queue = append(m.queue, c)
	m.recent = append(m.recent, c.Text)
	if len(m.recent) > 10 {
		m.recent = m.recent[len(m.recent)-10:]
	}

	// A race call does not wait for commentary to finish.
	//
	// Priority decides who goes NEXT, which is no help when the bus is already
	// busy: a cue is never cut mid-sentence, so a lap time queued behind a
	// twelve-second commentary line aired twelve seconds late. Measured, not
	// supposed — every other stage of that call takes about 150ms.
	//
	// That "never cut a playing cue" rule is right for its original purpose
	// (calls were being chopped to "Pil—" mid-word) but it was protecting
	// COMMENTARY at the expense of the thing commentary exists to support. A
	// lap time has a shelf life measured in seconds; colour does not.
	//
	// So the playing cue is FADED rather than truncated — the same duckSamples
	// ramp the green-light cut uses, which is what stops it sounding chopped —
	// and only when commentary is what is in the way. A race call still never
	// interrupts another race call: both matter, both are short, and cutting one
	// off to say the next would lose a lap time rather than delay it.
	if c.callKey != "" && !c.commentary &&
		m.cur != nil && m.cur.commentary && m.cur.fadeLeft == 0 {
		m.cur.fadeLeft = duckSamples
	}

	// Fast start on an idle bus: when the device is pull-paced and its buffer
	// PROVABLY holds nothing but silence (a run of pulled silence at least
	// twice the buffer long), a race call flushes that silence instead of
	// waiting behind it — the device refills immediately through pullFrame
	// and the first refill frames are this cue. That turns the ~200ms buffer
	// floor into a near-instant start, without ever clipping a real tail:
	// any cue audio in the buffer resets the silence count, so the flush
	// simply doesn't fire until the tail has fully played out.
	fastStart := c.callKey != "" && !c.commentary && m.cur == nil &&
		m.devicePaced && m.deviceBufBytes > 0 && m.idleSilence >= 2*m.deviceBufBytes
	m.mu.Unlock()
	if fastStart {
		m.flushDevice()
	}
}

// interrupt cuts matching audio off the bus: the currently-playing cue (mid-
// frame) and any queued cues the predicate selects. Used when a heat goes
// green — live racing always interrupts meeting smalltalk.
func (m *boothMixer) interrupt(drop func(*boothCue) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil && drop(m.cur) {
		m.cur = nil
	}
	kept := m.queue[:0]
	for _, c := range m.queue {
		if !drop(c) {
			kept = append(kept, c)
		}
	}
	m.queue = kept
}

// pending reports how many cues are waiting to play after the current one —
// used to keep exactly one in-heat call generating ahead (a due-time-aware
// count would over-hold, so a simple queue length is what the lookahead wants).
// dropQueued removes waiting cues but never touches the one ON AIR.
//
// The difference from interrupt() matters, and was heard: clearing the build-up
// for the countdown cut "Pilots turn on your video transmitters" mid-word, so
// what went to air was "Pil—" followed by the countdown. A truncated word
// sounds like a fault, not like an interruption.
//
// Cutting a live cue is right when the race has STARTED — talking over a heat
// in progress is worse than a clipped word. It is wrong merely to get ahead in
// a queue, which is all the countdown needs: dropping the four channel
// announcements behind it is enough, and the cue already speaking is over in a
// second or two.
// clampSample keeps a scaled sample inside 16-bit range.
func clampSample(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// duckSamples is how long a cut cue takes to fade out.
//
// Short enough that the call it is making way for is still effectively
// immediate, long enough that the ending sounds intentional rather than
// severed. ~40ms.
const duckSamples = boothSampleRate * 40 / 1000

// duck cuts matching cues, fading out the one ON AIR rather than truncating it.
//
// Used where the interruption is REQUIRED and must be immediate — the countdown
// has to reach pilots before the tone — but where chopping a word mid-syllable
// would sound broken.
// duck fades out matching audio on air and drops matching audio waiting.
//
// Reports what it cut. "It is still talking after I press Start" is impossible
// to chase without knowing whether the cut found anything: silence in the log
// could mean the bus was already clear OR that the predicate matched nothing,
// and those need opposite fixes.
// setFlush registers how to discard the device's buffered audio.
func (m *boothMixer) setFlush(f func()) {
	m.mu.Lock()
	m.onFlush = f
	m.mu.Unlock()
}

// flushDevice throws away audio already handed to the output.
//
// Ducking stops dd-pits generating a cue, but the device plays out what it
// already holds — 406ms on Bluetooth, heard as audio continuing after the Start
// button. Only safe after a WHOLESALE clear, since it discards indiscriminately.
func (m *boothMixer) flushDevice() {
	m.mu.Lock()
	f := m.onFlush
	m.mu.Unlock()
	if f != nil {
		f()
	}
}

func (m *boothMixer) duck(drop func(*boothCue) bool) (onAir string, dropped int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cur != nil && drop(m.cur) && m.cur.fadeLeft == 0 {
		m.cur.fadeLeft = duckSamples
		onAir = m.cur.Text
	}
	kept := m.queue[:0]
	for _, c := range m.queue {
		if drop(c) {
			dropped++
			continue
		}
		kept = append(kept, c)
	}
	m.queue = kept
	return onAir, dropped
}

func (m *boothMixer) dropQueued(drop func(*boothCue) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.queue[:0]
	for _, c := range m.queue {
		if !drop(c) {
			kept = append(kept, c)
		}
	}
	m.queue = kept
}

// raceCallBusy reports whether a race call is on air or waiting to be.
//
// The booth checks it before starting to GENERATE a beat, and holds while it is
// true. Measured reason: the finish wrap — a 14B writing 35 seconds of script,
// then voicing it — runs on the same GPU as the race-call voice, and with both
// going at once a two-second "finished in 4th" took 29 SECONDS to reach first
// audio. A starved STREAMING call then holds the bus on silence until its
// chunks arrive, which reads as the queue never emptying.
//
// The ordering is the policy: results are what pilots are waiting to hear, and
// colour about those results can only ever come second. Deferring generation
// costs the wrap a few seconds of lateness it already dwarfs with its own
// render time; deferring the results costs them half a minute.
func (m *boothMixer) raceCallBusy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil && !m.cur.commentary {
		return true
	}
	for _, c := range m.queue {
		if !c.commentary {
			return true
		}
	}
	return false
}

func (m *boothMixer) pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queue)
}

// busy reports whether audio is playing or ready to play NOW — used to hold
// back filler beats while the booth is talking. A cue scheduled for the future
// (held for the broadcast buffer) does NOT count as busy, so filler can fill
// the gap before it and simply gets pre-empted when the scheduled call is due.
func (m *boothMixer) busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil {
		return true
	}
	now := time.Now()
	for _, c := range m.queue {
		if c.playAt.IsZero() || !now.Before(c.playAt) {
			return true
		}
	}
	return false
}

// run pumps one frame every boothFrameMs to every connected client: cue audio
// when a cue is playing, silence otherwise. Real-time pacing (never faster)
// keeps OBS's media source clock happy.
// boothRunAhead is how many frames beyond the ticker the bus builds up WHILE A
// CUE IS ON AIR. The device buffer alone (200ms) was not enough cover: a 30B
// writer generating the next beat pegs the GPU and memory bus for tens of
// seconds, the pump goroutine gets starved, and the post-race wrap audibly
// dropped out mid-word ("ran out of buffer when it said Bola"). Buffering
// deeper at all times would fix that at the cost of every lap call starting
// late — so the depth is adaptive instead: while audio is committed to air the
// bus runs up to 400ms ahead (tail latency nobody hears), and the moment the
// bus is idle it stays shallow so a fresh call starts as fast as it does today.
// A cut is unaffected either way: Flush drains the channel AND the device.
const boothRunAhead = 8 // frames of 50ms — 400ms on top of the device's 200ms

func (m *boothMixer) run(ctx context.Context) {
	tick := time.NewTicker(boothFrameMs * time.Millisecond)
	defer tick.Stop()
	silence := make([]byte, boothFrameBytes)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		m.pump(silence)
	}
}

// setDevicePaced hands the bus clock to the in-process player (or back).
func (m *boothMixer) setDevicePaced(v bool, bufBytes int) {
	m.mu.Lock()
	m.devicePaced = v
	m.deviceBufBytes = bufBytes
	m.idleSilence = 0
	m.mu.Unlock()
}

// pullFrame is the device-paced read: produce the next frame NOW, tee it to
// the /booth.wav subscribers (they ride the device clock), and account the
// idle-silence run that makes the fast start provably safe.
func (m *boothMixer) pullFrame(silence []byte) []byte {
	frame := m.nextFrame(silence)
	m.mu.Lock()
	if m.cur == nil {
		m.idleSilence += len(frame)
	} else {
		m.idleSilence = 0
	}
	m.mu.Unlock()
	m.broadcast(frame)
	return frame
}

// pump is one tick of the bus: the scheduled frame, then — mid-cue only — a
// top-up of the client buffers. Split from run so the behaviour is testable.
func (m *boothMixer) pump(silence []byte) {
	m.mu.Lock()
	paced := m.devicePaced
	m.mu.Unlock()
	if paced {
		return // the device owns the clock; a second producer would double-consume
	}
	m.broadcast(m.nextFrame(silence))
	for range boothRunAhead {
		m.mu.Lock()
		// A fully-rendered cue is topped up freely. A LIVE STREAM is topped up
		// only from its SURPLUS — the audio banked beyond its re-bank lead —
		// so its protection against generation hiccups is never consumed into
		// the channel (the first version burst the whole 300ms lead at the
		// first tick: the holeshot starved instantly, re-banked, starved
		// again — "a huge delay and crackly"). When generation runs ahead of
		// real time the surplus flows into the device pipeline and covers
		// pump stalls under GPU load; when generation is only just keeping
		// up, there is no surplus and the stream stays tick-paced.
		room := m.cur != nil
		if room && m.cur.streaming && !m.cur.streamDone {
			room = len(m.cur.PCM)-m.curOff > boothFrameBytes+streamLeadBytes
		}
		if room {
			for ch := range m.clients {
				// One slot of headroom stays for the next scheduled tick, so
				// the paced frame never becomes the one that drops.
				if len(ch) >= cap(ch)-1 {
					room = false
					break
				}
			}
		}
		m.mu.Unlock()
		if !room {
			return
		}
		m.broadcast(m.nextFrame(silence))
	}
}

func (m *boothMixer) broadcast(frame []byte) {
	m.mu.Lock()
	for ch := range m.clients {
		select {
		case ch <- frame:
		default: // slow client — drop the frame, never stall the bus
		}
	}
	m.mu.Unlock()
}

// nextFrame returns the next boothFrameBytes of the bus: the playing cue's
// audio (zero-padded at its tail) or silence. Scheduled cues are held until
// their playAt (the delayed-video mark); a scheduled call coming due pre-empts
// an unscheduled filler cue so the action call lands on time.
// streamLeadBytes is the head start a streaming cue needs before it may play.
//
// Starting on the very first fragment is what makes a call sound choppy: each
// fragment is ~100ms, and the moment generation falls behind playback the cue
// runs dry and the gap is audible as a stutter mid-word.
//
// Whether streaming can work at all depends on the generation-to-playback
// ratio, and that ratio changed. Measured at ~1.0x while two 30B models were
// contending for the GPU, streaming had no margin and starved — which is why it
// was switched off. Measured again with the pack serving the fixed calls and
// the booth yielding during a heat, it runs at ~0.62x: 3.1s of audio produced
// in 1.93s. At that rate a lead of half a second is never consumed.
//
// 300ms. The lead only has to cover a hiccup, not the whole line: at 0.62x the
// buffer GROWS by roughly 0.6s for every second played, so once a cue is going
// it pulls further ahead. Banking more than that just delays the first note for
// no benefit, which is the whole complaint.
const streamLeadBytes = boothSampleRate * 2 * 300 / 1000 // 300ms

// playable reports whether a cue can go to air yet.
//
// A finished cue always can. A streaming one must have banked enough of a lead
// that it will not run dry mid-word.
func (c *boothCue) playable() bool {
	if !c.streaming || c.streamDone {
		return true
	}
	lead := streamLeadBytes
	if c.leadBytes > lead {
		lead = c.leadBytes
	}
	return len(c.PCM) >= lead
}

func (m *boothMixer) nextFrame(silence []byte) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()

	// A cue that has started ALWAYS finishes — a later call (even a scheduled
	// one coming due) waits its turn rather than cutting a commentator mid-
	// sentence. The only interruption is the deliberate heat-green interrupt()
	// of pre-race smalltalk. A due call that has to wait airs slightly late,
	// which is the right trade: never talk over the other commentator.
	if m.cur == nil {
		if i := m.pickNextLocked(now); i >= 0 {
			m.cur, m.curOff = m.queue[i], 0
			m.queue = append(m.queue[:i:i], m.queue[i+1:]...)
			m.cur.markSpoken(now)
			// A race call that waited to get on air. Logged only for calls, and
			// only past a threshold worth acting on — commentary waiting its turn
			// is normal and expected, a lap time waiting is the bug.
			if m.cur.callKey != "" && !m.cur.bornAt.IsZero() {
				if waited := now.Sub(m.cur.bornAt); waited > 400*time.Millisecond {
					log.Printf("audio out: %q waited %dms on the bus before it aired",
						m.cur.Text, waited.Milliseconds())
				}
			}
			if !m.cur.playAt.IsZero() {
				m.lastAlignMs = now.Sub(m.cur.playAt).Milliseconds()
			}
		}
	}
	if m.cur == nil {
		return silence
	}

	// A streaming cue that cannot fill a WHOLE frame holds silence and consumes
	// nothing. Playing the sliver that had arrived and zero-filling the rest
	// put a hard edge into every starved frame — twenty edges a second, heard
	// as crackle at the start of a cue while the model warmed up. A starved cue
	// also re-banks a full lead before resuming, so the recovery is one clean
	// pause rather than a burst of starve/resume edges.
	if m.cur.streaming && !m.cur.streamDone {
		avail := len(m.cur.PCM) - m.curOff
		if avail < boothFrameBytes || avail < m.cur.rebankLeft {
			if m.cur.rebankLeft == 0 {
				m.cur.rebankLeft = streamLeadBytes
			}
			return silence
		}
		m.cur.rebankLeft = 0
	}

	frame := make([]byte, boothFrameBytes)
	n := copy(frame, m.cur.PCM[m.curOff:])
	m.curOff += n

	// Fading out: ramp this frame down and finish the cue when the ramp ends.
	if m.cur.fadeLeft > 0 {
		samples := n / 2
		for i := 0; i < samples; i++ {
			if m.cur.fadeLeft <= 0 {
				// Past the ramp: silence, so nothing of the cut cue survives.
				binary.LittleEndian.PutUint16(frame[i*2:], 0)
				continue
			}
			gain := float64(m.cur.fadeLeft) / float64(duckSamples)
			v := float64(int16(binary.LittleEndian.Uint16(frame[i*2:]))) * gain
			binary.LittleEndian.PutUint16(frame[i*2:], uint16(clampSample(v)))
			m.cur.fadeLeft--
		}
		if m.cur.fadeLeft <= 0 {
			m.cur = nil
			return frame
		}
	}

	if m.curOff >= len(m.cur.PCM) {
		// A STREAMING cue is not finished just because the audio produced so
		// far has played. This is what makes a lap call immediate: the first
		// fragment airs the moment it exists rather than after the whole
		// utterance is generated, and the rest catches up while it plays.
		//
		// Holding silence here is the cost of that, and it is small: the model
		// generates faster than real time once running, so a gap only appears
		// at the very start.
		if m.cur.streaming && !m.cur.streamDone {
			return frame
		}
		if m.cur.streaming {
			m.lastStreamPlayedMs = int64(m.curOff) * 1000 / int64(boothSampleRate*2)
			m.lastStreamText = m.cur.Text
		}
		m.cur = nil
	}
	return frame
}

// pickNextLocked chooses the next cue to air: a scheduled cue whose playAt has
// arrived wins (earliest mark first); otherwise an unscheduled filler cue plays
// immediately. Returns -1 when only future-scheduled cues remain (hold silence).
// Beats generate concurrently, so a later call can be enqueued first. Both
// branches therefore order by cue.seq — the order the beats were taken off the
// queue — rather than by position in m.queue, which is arrival order.
func (m *boothMixer) pickNextLocked(now time.Time) int {
	best, bestScheduled := -1, false
	for i, c := range m.queue {
		scheduled := !c.playAt.IsZero()
		if scheduled && now.Before(c.playAt) {
			continue // not due yet
		}
		if !c.playable() {
			continue // still filling its head start; airing now would stutter
		}
		if best == -1 {
			best, bestScheduled = i, scheduled
			continue
		}

		// PRIORITY FIRST. A race call outranks anything else waiting,
		// whatever its mark or arrival order — that is the guarantee the whole
		// single-bus design exists to provide, and it must not be expressible
		// as a tie-break inside the scheduling rules below.
		if c.priority != m.queue[best].priority {
			if c.priority > m.queue[best].priority {
				best, bestScheduled = i, scheduled
			}
			continue
		}

		switch {
		case scheduled && !bestScheduled:
			best, bestScheduled = i, true // a due action call outranks filler
		case scheduled && bestScheduled:
			// Earliest mark wins; same mark falls back to the order the beats
			// happened, so two calls anchored to one moment can't invert.
			if c.playAt.Before(m.queue[best].playAt) ||
				(c.playAt.Equal(m.queue[best].playAt) && c.seq < m.queue[best].seq) {
				best = i
			}
		case !scheduled && !bestScheduled:
			// Unscheduled cues have no mark to sort by, so seq is the only
			// thing keeping them in the order they were called.
			if c.seq < m.queue[best].seq {
				best = i
			}
		}
	}
	return best
}

// dueScheduledIdxLocked returns the index of a scheduled cue whose mark has
// arrived (an action call ready to pre-empt filler), or -1.
func (m *boothMixer) dueScheduledIdxLocked(now time.Time) int {
	for i, c := range m.queue {
		if !c.playAt.IsZero() && !now.Before(c.playAt) {
			return i
		}
	}
	return -1
}

// alignMs snapshots the last scheduled cue's lateness (ms) for the status page.
func (m *boothMixer) alignMs() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastAlignMs
}

// serveStream is GET /booth.wav — a never-ending WAV whose header advertises
// a huge data size; OBS (ffmpeg) plays it like a radio station.
func (m *boothMixer) serveStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(streamingWAVHeader(boothSampleRate, 1, 16))
	fl.Flush()

	// A SHORT queue, deliberately.
	//
	// 32 frames was 1.6 SECONDS of audio waiting to be played (3.2s at the old
	// 100ms frame), and it never drained: the bus produces at realtime and the
	// device consumes at realtime, so one momentary stall filled the queue and
	// that latency stayed for the rest of the meeting. Measured as calls landing
	// seconds late while every stage that MADE them read fast.
	//
	// Two frames bounds it at 100ms. The cost is that a stall now drops a frame
	// instead of delaying everything behind it — the right trade for live race
	// calls, where a brief glitch is survivable and a permanent second is not.
	ch := make(chan []byte, boothRunAhead+2)
	m.mu.Lock()
	m.clients[ch] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.clients, ch)
		m.mu.Unlock()
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-ch:
			if _, err := w.Write(frame); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// recentLines snapshots the last on-air lines for the status page.
func (m *boothMixer) recentLines() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.recent...)
}

// servePage is GET /booth — the venue status page: on/off state (set at
// /setup/booth), the OBS stream URL, session AI spend,
// and the recent on-air lines.
func (b *booth) servePage(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	enabled := b.enabled
	mode := b.voiceMode
	b.mu.Unlock()
	if mode == "" {
		mode = "duo"
	}
	state := "OFF — turn it on at /setup/booth"
	if enabled {
		state = "LIVE (" + mode + ")"
	}
	recent := b.mixer.recentLines()
	usd := b.usage.estimateUSD()
	b.usage.mu.Lock()
	// Voice is reported in whatever unit the backend actually bills in.
	//
	// Qwen speech is priced per CHARACTER and returns {"characters":N} with no
	// token fields, so showing it in the token column read as "audio 0/0 tok" —
	// i.e. the voice looked free, which is the opposite of the truth and exactly
	// the wrong thing to tell someone asking where their spend is going.
	voice := fmt.Sprintf("voice %d/%d tok", b.usage.TTSIn, b.usage.TTSOut)
	if b.usage.QwenTTSChars > 0 {
		voice = fmt.Sprintf("voice %d chars", b.usage.QwenTTSChars)
	}
	spend := fmt.Sprintf("~$%.2f this session · script %d/%d tok · %s · %d calls",
		usd, b.usage.TextIn, b.usage.TextOut, voice, b.usage.Requests)
	b.usage.mu.Unlock()
	syncLine := boothSyncLine(b)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>Booth — live commentary</title>
<body style="font:14px system-ui;background:#0f1115;color:#e8eaed;padding:2rem;max-width:720px;margin:auto">
<h1 style="font-size:1.2rem">🎙 Live commentary booth — `+template_escape(state)+`</h1>
<p style="color:#9aa0a6">Estimated AI spend: `+template_escape(spend)+`</p>
<p style="color:#9aa0a6">`+template_escape(syncLine)+`</p>
<p>Add this URL to OBS as a <b>Media Source</b> (untick "Local File", set Input):</p>
<pre style="background:#1a1d23;padding:.6rem;border-radius:6px">http://`+r.Host+`/booth.wav</pre>
<audio controls src="/booth.wav"></audio>
<h2 style="font-size:1rem;margin-top:1.5rem">Recently on air</h2><pre style="background:#1a1d23;padding:.6rem;border-radius:6px;white-space:pre-wrap">`)
	for _, l := range recent {
		fmt.Fprintln(w, template_escape(l))
	}
	fmt.Fprint(w, `</pre><script>setTimeout(()=>location.reload(),15000)</script></body>`)
}

func template_escape(s string) string {
	r := ""
	for _, c := range s {
		switch c {
		case '<':
			r += "&lt;"
		case '>':
			r += "&gt;"
		case '&':
			r += "&amp;"
		default:
			r += string(c)
		}
	}
	return r
}

// streamingWAVHeader emits a WAV header with a near-maximal data length so
// players treat the stream as effectively endless.
func streamingWAVHeader(rate, channels, bits int) []byte {
	blockAlign := channels * bits / 8
	buf := make([]byte, 44)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 0xFFFFFFFF-8)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(buf[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(rate*blockAlign))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(buf[34:36], uint16(bits))
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], 0xFFFFFFFF-44)
	return buf
}

// resamplePCM16 linearly resamples 16-bit LE mono PCM between sample rates —
// good enough for speech when a TTS response arrives at a non-bus rate.
func resamplePCM16(pcm []byte, from, to int) []byte {
	if from == to || from <= 0 || to <= 0 || len(pcm) < 2 {
		return pcm
	}
	nIn := len(pcm) / 2
	nOut := int(int64(nIn) * int64(to) / int64(from))
	out := make([]byte, nOut*2)
	for i := 0; i < nOut; i++ {
		pos := float64(i) * float64(from) / float64(to)
		j := int(pos)
		frac := pos - float64(j)
		s0 := int16(binary.LittleEndian.Uint16(pcm[j*2:]))
		s1 := s0
		if j+1 < nIn {
			s1 = int16(binary.LittleEndian.Uint16(pcm[(j+1)*2:]))
		}
		v := float64(s0)*(1-frac) + float64(s1)*frac
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v)))
	}
	return out
}

// clientCount is how many things are listening to the bus right now.
//
// The one number that separates "commentary is not being made" from "it is
// being made and nobody is listening". Those look identical from the room and
// have opposite fixes, and telling them apart by ear cost a venue an afternoon.
//
// A listener is either a /booth.wav reader (a browser tab, OBS) or the local
// speaker player — both subscribe as clients, so one count covers both.
func (m *boothMixer) clientCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.clients)
}

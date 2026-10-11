package elrs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotConnected is returned when a send is attempted with no backpack.
var ErrNotConnected = errors.New("backpack not connected")

// BackpackConfig configures a Backpack client.
type BackpackConfig struct {
	// Serial is a serial port path, or "auto" to scan, or "" to disable serial.
	Serial string
	// Netpack is the netpack hostname, or "" to disable TCP.
	Netpack string
	// FrameDelay is a pause between frames so a fast writer does not overrun
	// the backpack's UART/ESP-NOW queue. Zero means DefaultFrameDelay.
	FrameDelay time.Duration
	// OnPacket, if set, receives every inbound packet (rx loop).
	OnPacket func(Packet)
	// Logf, if set, receives connection lifecycle messages.
	Logf func(format string, args ...any)
	// Dial overrides transport selection entirely (tests use this with
	// NewFakeBackpack). When set, Serial/Netpack are ignored.
	Dial func(ctx context.Context) (Transport, string, error)
}

// DefaultFrameDelay paces outbound frames; the reference Python plugin was
// implicitly paced by gevent scheduling.
const DefaultFrameDelay = 5 * time.Millisecond

// Backpack manages a connection to an ELRS Timer Backpack: connect,
// reconnect with backoff, an rx loop for inbound packets, and serialized
// outbound writes. A UID-targeted OSD transaction (Session) is a critical
// section — set-uid, writes, display, reset-uid never interleave between
// pilots, mirroring the reference plugin's queue lock.
type Backpack struct {
	cfg BackpackConfig

	mu sync.Mutex // serializes all writes; held for a whole Session
	tr Transport  // nil when disconnected

	connected atomic.Bool
	version   atomic.Value // string
}

// NewBackpack creates a client; call Run to connect.
func NewBackpack(cfg BackpackConfig) *Backpack {
	if cfg.FrameDelay == 0 {
		cfg.FrameDelay = DefaultFrameDelay
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	b := &Backpack{cfg: cfg}
	b.version.Store("")
	return b
}

// Connected reports whether a backpack is currently reachable.
func (b *Backpack) Connected() bool { return b.connected.Load() }

// Version returns the firmware version reported at connect, if any.
func (b *Backpack) Version() string { return b.version.Load().(string) }

// Run owns the connect → receive → reconnect loop until ctx is cancelled.
func (b *Backpack) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		tr, desc, err := b.dial(ctx)
		if err != nil {
			b.cfg.Logf("elrs: connect failed: %v (retry in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}

		backoff = time.Second
		b.cfg.Logf("elrs: backpack connected via %s (firmware %q)", desc, b.Version())
		b.attach(tr)
		b.receiveLoop(ctx, tr) // blocks until read error or ctx done
		b.detach()
		b.cfg.Logf("elrs: backpack disconnected")
	}
}

// dial tries the configured transports: explicit serial port, then netpack,
// then serial auto-scan.
func (b *Backpack) dial(ctx context.Context) (Transport, string, error) {
	if b.cfg.Dial != nil {
		tr, desc, err := b.cfg.Dial(ctx)
		if err != nil {
			return nil, "", err
		}
		if v, ok := Probe(tr, 2*time.Second); ok {
			b.version.Store(v)
		}
		return tr, desc, nil
	}
	if b.cfg.Serial != "" && b.cfg.Serial != "auto" {
		tr, err := b.open(func() (Transport, error) { return OpenSerial(b.cfg.Serial) })
		return tr, "serial " + b.cfg.Serial, err
	}
	if b.cfg.Netpack != "" {
		tr, err := b.open(func() (Transport, error) { return DialNetpack(ctx, b.cfg.Netpack) })
		if err == nil {
			return tr, "netpack " + b.cfg.Netpack, nil
		}
		if b.cfg.Serial != "auto" {
			return nil, "", err
		}
	}
	if b.cfg.Serial == "auto" {
		tr, port, err := ScanSerial(ctx)
		if err != nil {
			return nil, "", err
		}
		return tr, "serial " + port, nil
	}
	return nil, "", errors.New("no elrs transport configured")
}

func (b *Backpack) open(dial func() (Transport, error)) (Transport, error) {
	tr, err := dial()
	if err != nil {
		return nil, err
	}
	// The dialers already probed; probe again to record the version through
	// one code path (cheap, and fake transports answer instantly).
	if v, ok := Probe(tr, 2*time.Second); ok {
		b.version.Store(v)
	}
	return tr, nil
}

func (b *Backpack) attach(tr Transport) {
	b.mu.Lock()
	b.tr = tr
	b.mu.Unlock()
	b.connected.Store(true)
}

func (b *Backpack) detach() {
	b.mu.Lock()
	if b.tr != nil {
		b.tr.Close()
		b.tr = nil
	}
	b.mu.Unlock()
	b.connected.Store(false)
}

func (b *Backpack) receiveLoop(ctx context.Context, tr Transport) {
	var d Decoder
	buf := make([]byte, 512)
	for ctx.Err() == nil {
		n, err := tr.Read(buf)
		if n > 0 {
			for _, p := range d.Feed(buf[:n]) {
				if b.cfg.OnPacket != nil {
					b.cfg.OnPacket(p)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// send writes one frame. Callers must hold b.mu.
func (b *Backpack) sendLocked(p Packet) error {
	if b.tr == nil {
		return ErrNotConnected
	}
	if _, err := b.tr.Write(p.Encode()); err != nil {
		return fmt.Errorf("backpack write: %w", err)
	}
	time.Sleep(b.cfg.FrameDelay)
	return nil
}

// Session runs fn as an atomic OSD transaction against one goggle UID:
// set-send-uid, fn's staged writes, display, reset-send-uid. On any error the
// uid reset is still attempted so the backpack is never left targeted.
func (b *Backpack) Session(uid UID, fn func(s *Session) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.sendLocked(SetSendUIDPacket(uid)); err != nil {
		return err
	}

	err := fn(&Session{b: b})
	if err == nil {
		err = b.sendLocked(DisplayOSDPacket())
	}

	if rerr := b.sendLocked(ResetSendUIDPacket()); err == nil {
		err = rerr
	}
	return err
}

// Session stages OSD writes inside a Backpack.Session transaction. Display
// is committed automatically when the session function returns nil.
type Session struct {
	b       *Backpack
	cleared bool
}

// ClearScreen clears the target's whole OSD.
func (s *Session) ClearScreen() error {
	err := s.b.sendLocked(ClearOSDPacket())
	if err == nil {
		s.cleared = true
	}
	return err
}

// ScreenCleared reports whether this session already cleared the whole
// screen, making per-row clears redundant. Every ESP-NOW frame saved matters:
// at race start the 2.4GHz band is saturated by the pilots' RC links and each
// frame can burn a multi-hundred-ms retry cycle, delaying the frames queued
// behind it (the GO! text most of all).
func (s *Session) ScreenCleared() bool { return s.cleared }

// ClearRow clears one row of the target's OSD.
func (s *Session) ClearRow(row int) error {
	if row < 0 || row >= OSDRows {
		return fmt.Errorf("row %d out of range", row)
	}
	return s.b.sendLocked(ClearOSDRowPacket(row))
}

// Text stages text at row/col, clamped to the screen.
func (s *Session) Text(row, col int, text string) error {
	if row < 0 || row >= OSDRows {
		return fmt.Errorf("row %d out of range", row)
	}
	if col < 0 {
		col = 0
	}
	if col >= OSDCols {
		col = OSDCols - 1
	}
	if len(text) > OSDCols-col {
		text = text[:OSDCols-col]
	}
	return s.b.sendLocked(OSDTextPacket(row, col, text))
}

// SetChannelIndex changes one pilot's goggle video channel. Wrapped in the
// same uid-set/reset critical section as OSD writes.
func (b *Backpack) SetChannelIndex(uid UID, index byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.sendLocked(SetSendUIDPacket(uid)); err != nil {
		return err
	}
	err := b.sendLocked(SetChannelIndexPacket(index))
	if rerr := b.sendLocked(ResetSendUIDPacket()); err == nil {
		err = rerr
	}
	return err
}

// SetMode puts the connected backpack in bind ('B') or wifi ('W') mode.
func (b *Backpack) SetMode(mode byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sendLocked(SetModePacket(mode))
}

// SendDVRNameTo sends the label the goggles should name their next DVR
// recording with, wrapped in the same uid-set/reset critical section as OSD
// writes. An empty label clears a previously sent one.
func (b *Backpack) SendDVRNameTo(uid UID, label string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.sendLocked(SetSendUIDPacket(uid)); err != nil {
		return err
	}
	err := b.sendLocked(DVRNamePacket(label))
	if rerr := b.sendLocked(ResetSendUIDPacket()); err == nil {
		err = rerr
	}
	return err
}

// SendRTCTo sends t (as local wall-clock time) to one pilot's goggles.
// Wrapped in the same uid-set/reset critical section as OSD writes, so a
// goggle backpack bound to the pilot's personal phrase — which only accepts
// frames from its own UID — receives it.
func (b *Backpack) SendRTCTo(uid UID, t time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.sendLocked(SetSendUIDPacket(uid)); err != nil {
		return err
	}
	err := b.sendLocked(SetRTCPacket(t))
	if rerr := b.sendLocked(ResetSendUIDPacket()); err == nil {
		err = rerr
	}
	return err
}

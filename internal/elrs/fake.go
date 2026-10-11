package elrs

import (
	"net"
	"sync"
)

// FakeBackpack is an in-memory backpack for tests: a net.Pipe-backed
// Transport that decodes MSP frames, answers version probes, and
// materializes per-UID OSD screens so tests can assert what each pilot's
// goggles would show.
type FakeBackpack struct {
	mu sync.Mutex

	// Screens holds the committed (displayed) 18x50 character grid per UID.
	screens map[UID]*[OSDRows][OSDCols]byte
	// pending holds staged writes not yet committed by a display frame.
	pending map[UID]*[OSDRows][OSDCols]byte
	// channels records the last SetChannelIndex per UID.
	channels map[UID]byte
	// dvrNames records the last SetDVRName label per UID ("" = cleared).
	dvrNames map[UID]string
	// frames records every decoded frame in arrival order, so tests can
	// assert the uid-set/write/display/reset serialization contract.
	frames []Packet

	target   UID
	targeted bool

	// autoReport, when set, emits a FuncSendReport frame back to the client
	// for every UID-targeted payload frame — simulating a netpack with the
	// delivery-report firmware extension. Delivered=true unless the target
	// UID is in reportFail.
	autoReport bool
	reportFail map[UID]bool

	conn net.Conn
}

// FakeVersion is the firmware version the fake reports to probes.
const FakeVersion = "fake-1.0"

// NewFakeBackpack returns the fake and the Transport to hand to a Backpack.
func NewFakeBackpack() (*FakeBackpack, Transport) {
	client, server := net.Pipe()
	f := &FakeBackpack{
		screens:  make(map[UID]*[OSDRows][OSDCols]byte),
		pending:  make(map[UID]*[OSDRows][OSDCols]byte),
		channels: make(map[UID]byte),
		dvrNames: make(map[UID]string),
		conn:     server,
	}
	go f.serve()
	return f, client
}

// Close shuts down the fake's side of the pipe.
func (f *FakeBackpack) Close() { f.conn.Close() }

func (f *FakeBackpack) serve() {
	var d Decoder
	buf := make([]byte, 512)
	for {
		n, err := f.conn.Read(buf)
		if n > 0 {
			for _, p := range d.Feed(buf[:n]) {
				f.handle(p)
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *FakeBackpack) handle(p Packet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, p)

	switch p.Function {
	case FuncGetBackpackVersion:
		resp := Packet{Type: TypeResponse, Function: FuncGetBackpackVersion, Payload: []byte(FakeVersion)}
		go f.conn.Write(resp.Encode())

	case FuncSetSendUID:
		if len(p.Payload) >= 1 && p.Payload[0] == 0x01 && len(p.Payload) >= 7 {
			copy(f.target[:], p.Payload[1:7])
			f.targeted = true
		} else {
			f.targeted = false
		}

	case FuncSetOSD:
		if !f.targeted || len(p.Payload) == 0 {
			return
		}
		switch p.Payload[0] {
		case OSDSubClear:
			f.pending[f.target] = &[OSDRows][OSDCols]byte{}
			f.screens[f.target] = &[OSDRows][OSDCols]byte{}
		case OSDSubText:
			if len(p.Payload) < 4 {
				return
			}
			row, col := int(p.Payload[1]), int(p.Payload[2])
			if row >= OSDRows {
				return
			}
			grid := f.pendingGrid(f.target)
			for i, ch := range p.Payload[4:] {
				if col+i >= OSDCols {
					break
				}
				grid[row][col+i] = ch
			}
		case OSDSubDisplay:
			committed := *f.pendingGrid(f.target)
			f.screens[f.target] = &committed
		}

	case FuncSetVTXConfig:
		if f.targeted && len(p.Payload) >= 1 {
			f.channels[f.target] = p.Payload[0]
		}

	case FuncSetDVRName:
		if f.targeted {
			f.dvrNames[f.target] = string(p.Payload)
		}
	}

	// Delivery-report firmware extension: ack every forwarded (UID-targeted
	// payload) frame back over the wire, like a real netpack's ESP-NOW send
	// callback would.
	if f.autoReport && f.targeted {
		switch p.Function {
		case FuncSetOSD, FuncSetDVRName, FuncSetRTC, FuncSetVTXConfig, FuncSetRecordingState:
			rep := SendReport{UID: f.target, Function: p.Function, Delivered: !f.reportFail[f.target]}
			go f.conn.Write(SendReportPacket(rep).Encode())
		}
	}
}

func (f *FakeBackpack) pendingGrid(uid UID) *[OSDRows][OSDCols]byte {
	if g, ok := f.pending[uid]; ok {
		return g
	}
	g := &[OSDRows][OSDCols]byte{}
	if committed, ok := f.screens[uid]; ok {
		*g = *committed
	}
	f.pending[uid] = g
	return g
}

// Row returns the trimmed displayed text of one row for a UID.
func (f *FakeBackpack) Row(uid UID, row int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	grid, ok := f.screens[uid]
	if !ok {
		return ""
	}
	out := make([]byte, 0, OSDCols)
	for _, ch := range grid[row] {
		if ch == 0 {
			ch = ' '
		}
		out = append(out, ch)
	}
	// trim
	start, end := 0, len(out)
	for start < end && out[start] == ' ' {
		start++
	}
	for end > start && out[end-1] == ' ' {
		end--
	}
	return string(out[start:end])
}

// Channel returns the last channel index set for a UID.
func (f *FakeBackpack) Channel(uid UID) (byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[uid]
	return ch, ok
}

// DVRName returns the last DVR recording label sent to a UID ("" once
// cleared); ok is false when no label frame was ever received.
func (f *FakeBackpack) DVRName(uid UID) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.dvrNames[uid]
	return name, ok
}

// Frames returns a copy of every decoded frame in arrival order.
func (f *FakeBackpack) Frames() []Packet {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Packet(nil), f.frames...)
}

// WriteToClient injects a frame as if the backpack sent it (for rx tests).
func (f *FakeBackpack) WriteToClient(p Packet) error {
	_, err := f.conn.Write(p.Encode())
	return err
}

// AutoReport toggles the delivery-report firmware simulation: every
// UID-targeted payload frame is acked back with a FuncSendReport.
func (f *FakeBackpack) AutoReport(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoReport = on
}

// FailDeliveriesTo makes auto-reports for one UID report Delivered=false —
// simulating a pilot whose goggles are off / out of range.
func (f *FakeBackpack) FailDeliveriesTo(uid UID, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reportFail == nil {
		f.reportFail = map[UID]bool{}
	}
	f.reportFail[uid] = fail
}

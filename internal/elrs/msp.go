// Package elrs implements the ExpressLRS backpack protocol (MSP v2 native)
// and the shared config/command contracts between the cloud platform and the
// dd-pits venue agent. The wire protocol is a direct port of the reference
// Python implementation in the vrxc_elrs RotorHazard plugin.
package elrs

import (
	"encoding/binary"
	"time"
)

// PacketType is the MSP v2 direction byte.
type PacketType byte

const (
	TypeCommand  PacketType = '<'
	TypeResponse PacketType = '>'
)

// Function is an MSP function code.
type Function uint16

// ExpressLRS backpack MSP function codes.
const (
	FuncGetBackpackVersion Function = 0x0010
	// FuncSetVTXConfig (MSP_SET_VTX_CONFIG, 89) is how the VRX goggle backpack
	// accepts a video-channel change over ESP-NOW: payload[0] = channel index
	// (< 48). This is the opcode the timer forwards; do NOT use
	// FuncSetChannelIndex (0x0301) over the air — the VRX backpack drops it as
	// "Unknown command from ESPNOW" (Vrx_main.cpp). 0x0301 is only the
	// backpack→goggle UART opcode.
	FuncSetVTXConfig      Function = 0x0059
	FuncSetSendUID        Function = 0x00B5
	FuncSetOSD            Function = 0x00B6
	FuncGetChannelIndex   Function = 0x0300
	FuncSetChannelIndex   Function = 0x0301 // backpack→goggle UART only, not ESP-NOW
	FuncGetFrequency      Function = 0x0302
	FuncSetFrequency      Function = 0x0303
	FuncGetRecordingState Function = 0x0304
	FuncSetRecordingState Function = 0x0305
	FuncGetVRXMode        Function = 0x0306
	FuncSetVRXMode        Function = 0x0307
	// FuncSetRTC (MSP_ELRS_BACKPACK_SET_RTC) carries wall-clock time to a VRX
	// backpack, which forwards it to the goggles (HDZero sets its Linux OS
	// clock + hardware RTC from it, so DVR recordings get correct timestamps).
	FuncSetRTC Function = 0x030E
	// FuncSetDVRName (MSP_ELRS_BACKPACK_SET_DVR_NAME) carries a race label the
	// goggles use to name the next DVR recording; empty clears it.
	FuncSetDVRName Function = 0x030F
	// FuncSendReport (dd-pits extension) is emitted BY the netpack firmware
	// over serial after it forwards a UID-targeted frame over ESP-NOW,
	// reporting the MAC-level ack outcome of that send. ESP-NOW unicast
	// frames are link-layer acknowledged by the receiving ESP32, so
	// "delivered" means the target goggle backpack's radio received the
	// frame (the goggle-internal UART hop is wired and assumed reliable).
	// Payload: [uid:6][function:2 big-endian — the forwarded frame's
	// function][status:1 — 1=delivered, 0=failed][retries:1]. Netpack
	// firmware without this extension simply never emits it; consumers must
	// treat absence as "unknown", not failure.
	FuncSendReport Function = 0x0310
	FuncSetMode    Function = 0x0380 // payload 'B' = bind, 'W' = wifi
)

// OSD subcommands (payload byte 0 of FuncSetOSD).
const (
	OSDSubClear   = 0x02
	OSDSubText    = 0x03
	OSDSubDisplay = 0x04
)

// OSD screen dimensions on HDZero goggles.
const (
	OSDCols = 50
	OSDRows = 18
)

const mspHeaderLength = 8

// Packet is a single MSP v2 native frame.
type Packet struct {
	Type     PacketType
	Flags    byte
	Function Function
	Payload  []byte
}

// Encode returns the full wire frame: $X<type> flags func(LE16) size(LE16) payload crc.
func (p Packet) Encode() []byte {
	body := make([]byte, 0, 5+len(p.Payload))
	body = append(body, p.Flags)
	body = binary.LittleEndian.AppendUint16(body, uint16(p.Function))
	body = binary.LittleEndian.AppendUint16(body, uint16(len(p.Payload)))
	body = append(body, p.Payload...)

	out := make([]byte, 0, 3+len(body)+1)
	out = append(out, '$', 'X', byte(p.Type))
	out = append(out, body...)
	out = append(out, crc8DVBS2(0, body...))
	return out
}

func crc8DVBS2(crc byte, data ...byte) byte {
	for _, b := range data {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0xD5
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

type decodeState int

const (
	stateIdle decodeState = iota
	stateHeaderStart
	stateHeaderX
	stateHeaderV2
	statePayloadV2
	stateChecksumV2
)

// Decoder is a resumable MSP v2 stream parser. Feed it arbitrary chunks of
// bytes (TCP fragments, serial reads) and it yields completed packets.
// Frames with bad CRCs or unknown direction bytes are silently dropped,
// matching the Python state machine.
type Decoder struct {
	state  decodeState
	typ    PacketType
	buf    []byte
	length int
	crc    byte
}

// Feed consumes data and returns any packets completed by it.
func (d *Decoder) Feed(data []byte) []Packet {
	var packets []Packet
	for _, c := range data {
		switch d.state {
		case stateIdle:
			if c == '$' {
				d.buf = d.buf[:0]
				d.buf = append(d.buf, c)
				d.state = stateHeaderStart
			}

		case stateHeaderStart:
			if c == 'X' {
				d.buf = append(d.buf, c)
				d.state = stateHeaderX
			} else {
				d.state = stateIdle
			}

		case stateHeaderX:
			d.crc = 0
			if c == byte(TypeCommand) || c == byte(TypeResponse) {
				d.typ = PacketType(c)
				d.buf = append(d.buf, c)
				d.state = stateHeaderV2
			} else {
				d.state = stateIdle
			}

		case stateHeaderV2:
			d.buf = append(d.buf, c)
			d.crc = crc8DVBS2(d.crc, c)
			if len(d.buf) == mspHeaderLength {
				d.length = int(binary.LittleEndian.Uint16(d.buf[6:8]))
				if d.length == 0 {
					d.state = stateChecksumV2
				} else {
					d.state = statePayloadV2
				}
			}

		case statePayloadV2:
			d.buf = append(d.buf, c)
			d.crc = crc8DVBS2(d.crc, c)
			if len(d.buf)-mspHeaderLength == d.length {
				d.state = stateChecksumV2
			}

		case stateChecksumV2:
			if c == d.crc {
				p := Packet{
					Type:     d.typ,
					Flags:    d.buf[3],
					Function: Function(binary.LittleEndian.Uint16(d.buf[4:6])),
				}
				if d.length > 0 {
					p.Payload = append([]byte(nil), d.buf[mspHeaderLength:]...)
				}
				packets = append(packets, p)
			}
			d.state = stateIdle
		}
	}
	return packets
}

// Packet constructors mirroring the Python plugin's senders. All are
// TypeCommand frames unless noted.

// VersionRequestPacket asks the backpack for its firmware version.
func VersionRequestPacket() Packet {
	return Packet{Type: TypeCommand, Function: FuncGetBackpackVersion}
}

// SetSendUIDPacket targets subsequent packets at one goggle UID.
func SetSendUIDPacket(uid UID) Packet {
	payload := append([]byte{0x01}, uid[:]...)
	return Packet{Type: TypeCommand, Function: FuncSetSendUID, Payload: payload}
}

// ResetSendUIDPacket returns packet addressing to the system default.
func ResetSendUIDPacket() Packet {
	return Packet{Type: TypeCommand, Function: FuncSetSendUID, Payload: []byte{0x00}}
}

// ClearOSDPacket clears the target's whole OSD.
func ClearOSDPacket() Packet {
	return Packet{Type: TypeCommand, Function: FuncSetOSD, Payload: []byte{OSDSubClear}}
}

// OSDTextPacket stages text at row/col; not shown until DisplayOSDPacket.
// Text longer than OSDCols is truncated.
func OSDTextPacket(row, col int, text string) Packet {
	payload := []byte{OSDSubText, byte(row), byte(col), 0}
	for i := 0; i < len(text) && i < OSDCols; i++ {
		payload = append(payload, text[i])
	}
	return Packet{Type: TypeCommand, Function: FuncSetOSD, Payload: payload}
}

// ClearOSDRowPacket stages a clear of one row (50 zero chars).
func ClearOSDRowPacket(row int) Packet {
	payload := make([]byte, 4+OSDCols)
	payload[0] = OSDSubText
	payload[1] = byte(row)
	return Packet{Type: TypeCommand, Function: FuncSetOSD, Payload: payload}
}

// DisplayOSDPacket commits staged text to the target's screen.
func DisplayOSDPacket() Packet {
	return Packet{Type: TypeCommand, Function: FuncSetOSD, Payload: []byte{OSDSubDisplay}}
}

// SetChannelIndexPacket changes the target VRx's video channel. It uses
// MSP_SET_VTX_CONFIG (89) with the 1-byte channel index, which is what the
// VRX goggle backpack actually acts on over ESP-NOW (Vrx_main.cpp reads
// payload[0] when it is < 48).
func SetChannelIndexPacket(index byte) Packet {
	return Packet{Type: TypeCommand, Function: FuncSetVTXConfig, Payload: []byte{index}}
}

// SetModePacket puts the connected backpack in bind ('B') or wifi ('W') mode.
func SetModePacket(mode byte) Packet {
	return Packet{Type: TypeCommand, Function: FuncSetMode, Payload: []byte{mode}}
}

// DVRNameMaxLen is the goggles' label cap (REC_labelMAXLEN - 1).
const DVRNameMaxLen = 43

// DVRNamePacket carries a label for the goggles to name the next DVR
// recording with (e.g. "WinterCup-Qual1-H2"). An empty label clears a
// previously sent one.
func DVRNamePacket(label string) Packet {
	if len(label) > DVRNameMaxLen {
		label = label[:DVRNameMaxLen]
	}
	return Packet{Type: TypeCommand, Function: FuncSetDVRName, Payload: []byte(label)}
}

// SendReport is a netpack per-frame delivery report (FuncSendReport): the
// ESP-NOW MAC-ack outcome of one forwarded frame.
type SendReport struct {
	UID       UID
	Function  Function // the forwarded frame's function (SetDVRName, SetOSD, …)
	Delivered bool     // the target backpack's radio acked the frame
	Retries   int
}

// SendReportPacket encodes a report frame — the firmware-side reference for
// the FuncSendReport payload layout, and what tests emit.
func SendReportPacket(rep SendReport) Packet {
	p := make([]byte, 10)
	copy(p[0:6], rep.UID[:])
	p[6] = byte(rep.Function >> 8)
	p[7] = byte(rep.Function)
	if rep.Delivered {
		p[8] = 1
	}
	if rep.Retries > 255 {
		rep.Retries = 255
	}
	p[9] = byte(rep.Retries)
	return Packet{Type: TypeResponse, Function: FuncSendReport, Payload: p}
}

// ParseSendReport decodes a FuncSendReport packet; ok=false for any other
// function or a malformed payload.
func ParseSendReport(p Packet) (SendReport, bool) {
	if p.Function != FuncSendReport || len(p.Payload) < 10 {
		return SendReport{}, false
	}
	var rep SendReport
	copy(rep.UID[:], p.Payload[0:6])
	rep.Function = Function(uint16(p.Payload[6])<<8 | uint16(p.Payload[7]))
	rep.Delivered = p.Payload[8] == 1
	rep.Retries = int(p.Payload[9])
	return rep, true
}

// SetRTCPacket carries t as local wall-clock time. The payload is the raw
// struct tm field encoding the goggles expect (Backpack hdzero.cpp /
// hdzero-goggle elrs.c): years since 1900, month 0-11, day, hour, min, sec.
func SetRTCPacket(t time.Time) Packet {
	t = t.Local()
	return Packet{Type: TypeCommand, Function: FuncSetRTC, Payload: []byte{
		byte(t.Year() - 1900),
		byte(int(t.Month()) - 1),
		byte(t.Day()),
		byte(t.Hour()),
		byte(t.Minute()),
		byte(t.Second()),
	}}
}

// CenterCol returns the column that centers a string of length n on the OSD,
// matching the Python plugin's center_osd.
func CenterCol(n int) int {
	col := OSDCols/2 - n/2
	if col < 0 {
		return 0
	}
	return col
}

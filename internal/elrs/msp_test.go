package elrs

import (
	"encoding/hex"
	"testing"
	"time"
)

// Golden byte vectors generated from the reference Python implementation
// (vrxc_elrs msp.py / elrs_backpack.py).
var goldenFrames = []struct {
	name   string
	packet Packet
	hex    string
}{
	{
		name:   "version request",
		packet: VersionRequestPacket(),
		hex:    "24583c0010000000ae",
	},
	{
		name:   "set send uid",
		packet: SetSendUIDPacket(UIDFromPhrase("testpilot")),
		hex:    "24583c00b500070001387f99a5e5b248",
	},
	{
		name:   "reset send uid",
		packet: ResetSendUIDPacket(),
		hex:    "24583c00b5000100002e",
	},
	{
		name:   "clear osd",
		packet: ClearOSDPacket(),
		hex:    "24583c00b6000100025e",
	},
	{
		name:   "osd text",
		packet: OSDTextPacket(5, 19, "w ARM NOW x"),
		hex:    "24583c00b6000f0003051300772041524d204e4f5720782b",
	},
	{
		name:   "display osd",
		packet: DisplayOSDPacket(),
		hex:    "24583c00b600010004df",
	},
	{
		name:   "clear osd row 15",
		packet: ClearOSDRowPacket(15),
		hex:    "24583c00b6003600030f0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000031",
	},
	{
		name:   "set channel index 33 (MSP_SET_VTX_CONFIG)",
		packet: SetChannelIndexPacket(33),
		hex:    "24583c0059000100211a",
	},
	{
		name:   "version response",
		packet: Packet{Type: TypeResponse, Function: FuncGetBackpackVersion, Payload: []byte("1.5.0")},
		hex:    "24583e0010000500312e352e3048",
	},
	{
		// 2026-07-13 14:30:45 → tm fields 126, 6, 13, 14, 30, 45.
		name:   "set rtc",
		packet: Packet{Type: TypeCommand, Function: FuncSetRTC, Payload: []byte{126, 6, 13, 14, 30, 45}},
		hex:    "24583c000e0306007e060d0e1e2dc1",
	},
}

func TestSetRTCPacket(t *testing.T) {
	loc := time.FixedZone("AEST", 10*3600)
	p := SetRTCPacket(time.Date(2026, time.July, 13, 14, 30, 45, 0, loc).In(loc))
	// SetRTCPacket converts to the process-local zone, so build the expected
	// payload from the same converted instant.
	lt := time.Date(2026, time.July, 13, 14, 30, 45, 0, loc).Local()
	want := []byte{
		byte(lt.Year() - 1900), byte(int(lt.Month()) - 1), byte(lt.Day()),
		byte(lt.Hour()), byte(lt.Minute()), byte(lt.Second()),
	}
	if p.Function != FuncSetRTC || string(p.Payload) != string(want) {
		t.Errorf("SetRTCPacket payload = %v, want %v", p.Payload, want)
	}
}

func TestEncodeGolden(t *testing.T) {
	for _, tc := range goldenFrames {
		t.Run(tc.name, func(t *testing.T) {
			got := hex.EncodeToString(tc.packet.Encode())
			if got != tc.hex {
				t.Errorf("Encode() = %s, want %s", got, tc.hex)
			}
		})
	}
}

func TestDecodeGolden(t *testing.T) {
	for _, tc := range goldenFrames {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := hex.DecodeString(tc.hex)
			if err != nil {
				t.Fatal(err)
			}
			var d Decoder
			packets := d.Feed(raw)
			if len(packets) != 1 {
				t.Fatalf("got %d packets, want 1", len(packets))
			}
			p := packets[0]
			if p.Type != tc.packet.Type || p.Function != tc.packet.Function {
				t.Errorf("decoded type/function = %c/%#x, want %c/%#x",
					p.Type, p.Function, tc.packet.Type, tc.packet.Function)
			}
			if string(p.Payload) != string(tc.packet.Payload) {
				t.Errorf("decoded payload = %x, want %x", p.Payload, tc.packet.Payload)
			}
		})
	}
}

// TestDecodeSplitBoundaries feeds a multi-frame stream split at every
// possible byte boundary to prove the decoder is resumable.
func TestDecodeSplitBoundaries(t *testing.T) {
	var stream []byte
	for _, tc := range goldenFrames {
		raw, _ := hex.DecodeString(tc.hex)
		stream = append(stream, raw...)
	}

	for split := 0; split <= len(stream); split++ {
		var d Decoder
		packets := d.Feed(stream[:split])
		packets = append(packets, d.Feed(stream[split:])...)
		if len(packets) != len(goldenFrames) {
			t.Fatalf("split %d: got %d packets, want %d", split, len(packets), len(goldenFrames))
		}
		for i, p := range packets {
			if p.Function != goldenFrames[i].packet.Function {
				t.Fatalf("split %d packet %d: function %#x, want %#x",
					split, i, p.Function, goldenFrames[i].packet.Function)
			}
		}
	}
}

func TestDecodeDropsBadCRC(t *testing.T) {
	raw, _ := hex.DecodeString(goldenFrames[0].hex)
	bad := append([]byte(nil), raw...)
	bad[len(bad)-1] ^= 0xFF

	var d Decoder
	if packets := d.Feed(bad); len(packets) != 0 {
		t.Fatalf("bad CRC frame yielded %d packets", len(packets))
	}
	// Decoder must recover for the next good frame.
	if packets := d.Feed(raw); len(packets) != 1 {
		t.Fatalf("decoder did not recover after bad frame")
	}
}

func TestDecodeIgnoresGarbageBetweenFrames(t *testing.T) {
	raw, _ := hex.DecodeString(goldenFrames[0].hex)
	stream := append([]byte("garbage$Xnoise"), raw...)
	stream = append(stream, 'j', 'u', 'n', 'k')

	var d Decoder
	if packets := d.Feed(stream); len(packets) != 1 {
		t.Fatalf("got %d packets, want 1", len(packets))
	}
}

func TestOSDTextTruncation(t *testing.T) {
	long := make([]byte, 80)
	for i := range long {
		long[i] = 'A'
	}
	p := OSDTextPacket(0, 0, string(long))
	if len(p.Payload) != 4+OSDCols {
		t.Errorf("payload length %d, want %d", len(p.Payload), 4+OSDCols)
	}
}

func TestCenterCol(t *testing.T) {
	cases := []struct{ n, want int }{
		{0, 25}, {11, 20}, {50, 0}, {60, 0}, {1, 25}, {2, 24},
	}
	for _, tc := range cases {
		if got := CenterCol(tc.n); got != tc.want {
			t.Errorf("CenterCol(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

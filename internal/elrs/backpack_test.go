package elrs

import (
	"context"
	"testing"
	"time"
)

func startTestBackpack(t *testing.T) (*Backpack, *FakeBackpack, context.CancelFunc) {
	t.Helper()
	fake, tr := NewFakeBackpack()
	bp := NewBackpack(BackpackConfig{
		FrameDelay: time.Microsecond,
		Dial: func(context.Context) (Transport, string, error) {
			return tr, "fake", nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go bp.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for !bp.Connected() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("backpack never connected to fake")
		}
		time.Sleep(time.Millisecond)
	}
	return bp, fake, cancel
}

func TestBackpackConnectAndVersion(t *testing.T) {
	bp, _, cancel := startTestBackpack(t)
	defer cancel()
	if v := bp.Version(); v != FakeVersion {
		t.Errorf("Version() = %q, want %q", v, FakeVersion)
	}
}

func TestSessionWritesScreen(t *testing.T) {
	bp, fake, cancel := startTestBackpack(t)
	defer cancel()

	uid := UIDFromPhrase("testpilot")
	err := bp.Session(uid, func(s *Session) error {
		if err := s.ClearScreen(); err != nil {
			return err
		}
		return s.Text(5, CenterCol(len("w ARM NOW x")), "w ARM NOW x")
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return fake.Row(uid, 5) == "w ARM NOW x" })
}

func TestSessionSerialization(t *testing.T) {
	bp, fake, cancel := startTestBackpack(t)
	defer cancel()

	uids := []UID{UIDFromPhrase("p1"), UIDFromPhrase("p2"), UIDFromPhrase("p3")}
	done := make(chan error, len(uids))
	for i, uid := range uids {
		go func(row int, uid UID) {
			done <- bp.Session(uid, func(s *Session) error {
				return s.Text(row, 0, "HELLO")
			})
		}(i, uid)
	}
	for range uids {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	// Every session must be a contiguous uid-set ... reset block.
	waitFor(t, func() bool {
		n := 0
		for _, f := range fake.Frames() {
			if f.Function == FuncSetSendUID && len(f.Payload) > 0 && f.Payload[0] == 0x00 {
				n++
			}
		}
		return n == len(uids)
	})

	inSession := false
	for _, f := range fake.Frames() {
		switch f.Function {
		case FuncSetSendUID:
			if f.Payload[0] == 0x01 {
				if inSession {
					t.Fatal("nested uid-set: sessions interleaved")
				}
				inSession = true
			} else {
				if !inSession {
					t.Fatal("uid-reset outside a session")
				}
				inSession = false
			}
		case FuncSetOSD:
			if !inSession {
				t.Fatal("OSD write outside a uid session")
			}
		}
	}

	for i, uid := range uids {
		if got := fake.Row(uid, i); got != "HELLO" {
			t.Errorf("pilot %d row %d = %q, want HELLO", i, i, got)
		}
	}
}

func TestSetChannelIndex(t *testing.T) {
	bp, fake, cancel := startTestBackpack(t)
	defer cancel()

	uid := UIDFromPhrase("testpilot")
	idx, err := ChannelIndex("R", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := bp.SetChannelIndex(uid, idx); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		ch, ok := fake.Channel(uid)
		return ok && ch == 33
	})
}

func TestSessionWhenDisconnected(t *testing.T) {
	bp := NewBackpack(BackpackConfig{})
	err := bp.Session(UIDFromPhrase("x"), func(s *Session) error { return nil })
	if err == nil {
		t.Fatal("Session succeeded with no connection")
	}
}

func TestInboundPackets(t *testing.T) {
	got := make(chan Packet, 8)
	fake, tr := NewFakeBackpack()
	bp := NewBackpack(BackpackConfig{
		FrameDelay: time.Microsecond,
		OnPacket:   func(p Packet) { got <- p },
		Dial: func(context.Context) (Transport, string, error) {
			return tr, "fake", nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bp.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for !bp.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
		time.Sleep(time.Millisecond)
	}

	// Simulate the race director's TX backpack sending a recording-state
	// command (the start/stop race signal we log but no longer act on).
	if err := fake.WriteToClient(Packet{
		Type: TypeCommand, Function: FuncSetRecordingState, Payload: []byte{0x01},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case p := <-got:
		if p.Function != FuncSetRecordingState {
			t.Errorf("got function %#x, want SetRecordingState", p.Function)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inbound packet never delivered")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(time.Millisecond)
	}
}

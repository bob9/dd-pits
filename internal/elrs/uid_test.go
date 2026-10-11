package elrs

import "testing"

// Vectors generated from the reference Python hash_phrase implementation.
func TestUIDFromPhrase(t *testing.T) {
	cases := []struct{ phrase, hex string }{
		{"testpilot", "387f99a5e5b2"},
		{"GLENN", "507a508b80cb"},
		{"a", "f6c920620818"}, // md5 first byte is odd -> decremented
	}
	for _, tc := range cases {
		if got := UIDHex(tc.phrase); got != tc.hex {
			t.Errorf("UIDHex(%q) = %s, want %s", tc.phrase, got, tc.hex)
		}
	}
}

func TestUIDFirstByteAlwaysEven(t *testing.T) {
	for _, phrase := range []string{"a", "b", "c", "pilot one", "x1", "zz"} {
		uid := UIDFromPhrase(phrase)
		if uid[0]%2 != 0 {
			t.Errorf("UIDFromPhrase(%q)[0] = %#x is odd", phrase, uid[0])
		}
	}
}

func TestParseUID(t *testing.T) {
	want := UIDFromPhrase("testpilot")
	for _, s := range []string{
		"387f99a5e5b2",
		"38:7F:99:A5:E5:B2",
		"56,127,153,165,229,178",
		"56.127.153.165.229.178",
	} {
		got, err := ParseUID(s)
		if err != nil {
			t.Errorf("ParseUID(%q): %v", s, err)
			continue
		}
		if got != want {
			t.Errorf("ParseUID(%q) = %s, want %s", s, got, want)
		}
	}

	for _, s := range []string{"", "zz7f99a5e5b2", "1,2,3", "1,2,3,4,5,300"} {
		if _, err := ParseUID(s); err == nil {
			t.Errorf("ParseUID(%q) succeeded, want error", s)
		}
	}
}

package elrs

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// UID is a 6-byte ExpressLRS binding UID derived from a bind phrase.
type UID [6]byte

// UIDFromPhrase derives a UID the same way ExpressLRS firmware does: the
// first 6 bytes of MD5(`-DMY_BINDING_PHRASE="<phrase>"`), with the first
// byte decremented if odd (ESP-NOW requires a unicast MAC).
func UIDFromPhrase(phrase string) UID {
	sum := md5.Sum([]byte(`-DMY_BINDING_PHRASE="` + phrase + `"`))
	var uid UID
	copy(uid[:], sum[:6])
	if uid[0]%2 == 1 {
		uid[0]--
	}
	return uid
}

// UIDHex derives a UID from a phrase and returns it as 12 lowercase hex chars.
func UIDHex(phrase string) string {
	uid := UIDFromPhrase(phrase)
	return hex.EncodeToString(uid[:])
}

// String returns the UID as lowercase hex.
func (u UID) String() string {
	return hex.EncodeToString(u[:])
}

// Dotted returns the UID in the decimal dotted form ExpressLRS UIs use,
// e.g. "120,54,0,20,9,88" style with dots.
func (u UID) Dotted() string {
	parts := make([]string, len(u))
	for i, b := range u {
		parts[i] = strconv.Itoa(int(b))
	}
	return strings.Join(parts, ".")
}

// ParseUID accepts 12 hex chars ("aabbccddeeff"), colon-separated hex
// ("aa:bb:cc:dd:ee:ff"), or decimal separated by dots or commas
// ("170,187,204,221,238,255").
func ParseUID(s string) (UID, error) {
	var uid UID
	s = strings.TrimSpace(s)
	if s == "" {
		return uid, fmt.Errorf("empty uid")
	}

	if strings.ContainsAny(s, ".,") {
		parts := strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == ',' })
		if len(parts) != len(uid) {
			return uid, fmt.Errorf("uid %q: want 6 parts, got %d", s, len(parts))
		}
		for i, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil || n < 0 || n > 255 {
				return uid, fmt.Errorf("uid %q: bad byte %q", s, p)
			}
			uid[i] = byte(n)
		}
		return uid, nil
	}

	s = strings.ReplaceAll(s, ":", "")
	b, err := hex.DecodeString(strings.ToLower(s))
	if err != nil || len(b) != len(uid) {
		return uid, fmt.Errorf("uid %q: want 12 hex chars", s)
	}
	copy(uid[:], b)
	return uid, nil
}

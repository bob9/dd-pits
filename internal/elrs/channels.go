package elrs

import (
	"fmt"
	"strconv"
	"strings"
)

// Band order used by the ExpressLRS backpack channel-index table:
// index = band*8 + (channel-1).
var bandOrder = []string{"A", "B", "E", "F", "R", "L"}

// bandAliases maps common band names onto the canonical single letters.
var bandAliases = map[string]string{
	"A": "A", "BOSCAM A": "A",
	"B": "B", "BOSCAM B": "B",
	"E": "E", "BOSCAM E": "E",
	"F": "F", "FATSHARK": "F", "AIRWAVE": "F", "IRC": "F",
	"R": "R", "RACEBAND": "R", "RACE": "R",
	"L": "L", "LOWBAND": "L", "LOW": "L",
}

// ChannelIndex maps a band + channel number (1-8) to the backpack's channel
// index. The order (A, B, E, F, R, L) is the Backpack's frequencyTable
// (lib/Channels/channels.h): Lowband is indices 40-47.
func ChannelIndex(band string, channel int) (byte, error) {
	canonical, ok := bandAliases[strings.ToUpper(strings.TrimSpace(band))]
	if !ok {
		return 0, fmt.Errorf("unknown band %q", band)
	}
	if channel < 1 || channel > 8 {
		return 0, fmt.Errorf("channel %d out of range 1-8", channel)
	}
	for i, b := range bandOrder {
		if b == canonical {
			return byte(i*8 + channel - 1), nil
		}
	}
	return 0, fmt.Errorf("unknown band %q", band)
}

// ChannelIndexFromLabel parses labels like "R2" or "F4".
func ChannelIndexFromLabel(label string) (byte, error) {
	label = strings.ToUpper(strings.TrimSpace(label))
	if len(label) < 2 {
		return 0, fmt.Errorf("bad channel label %q", label)
	}
	ch, err := strconv.Atoi(label[1:])
	if err != nil {
		return 0, fmt.Errorf("bad channel label %q", label)
	}
	return ChannelIndex(label[:1], ch)
}

// ChannelLabel returns the "R2"-style label for a channel index.
func ChannelLabel(index byte) string {
	band := int(index) / 8
	ch := int(index)%8 + 1
	if band >= len(bandOrder) {
		return fmt.Sprintf("?%d", index)
	}
	return fmt.Sprintf("%s%d", bandOrder[band], ch)
}

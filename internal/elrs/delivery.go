package elrs

// Wire types for POST /dronedash/v2/elrs-delivery — the dd-pits agent's
// per-message goggle delivery log, shipped to the cloud so admins can see
// exactly what was sent to each pilot per race and whether it arrived.
// Shared by the agent (encoder) and the platform handler (decoder) so the
// two cannot drift.

// Delivery statuses. "sent" means no netpack reports arrived (older netpack
// firmware without FuncSendReport, or the frames never reached the wire
// callback) — unknown, NOT failed.
const (
	DeliveryStatusSent      = "sent"
	DeliveryStatusDelivered = "delivered"
	DeliveryStatusPartial   = "partial" // some frames acked, some not
	DeliveryStatusFailed    = "failed"
)

// DeliveryKinds is the closed set of logical message kinds the agent
// journals; the server rejects anything else.
var DeliveryKinds = []string{
	"dvr_label", "dvr_clear",
	"osd_stage", "osd_go", "osd_lap", "osd_gap", "osd_finish", "osd_land", "osd_clear",
	"clock", "channel",
}

// DeliveryMessage is one logical goggle message (which may span several
// ESP-NOW frames) and its aggregated delivery outcome.
type DeliveryMessage struct {
	SentAt       string `json:"sent_at"`                  // RFC3339, agent clock
	EventSource  string `json:"event_source_id"`          // FPVTrackside event id
	RaceSource   string `json:"race_source_id,omitempty"` // FPVTrackside race id; empty for non-race messages (clock)
	RaceName     string `json:"race_name,omitempty"`
	Pilot        string `json:"pilot"`
	Kind         string `json:"kind"`
	Message      string `json:"message"` // human-readable content (the DVR label, "GO!", the lap line, …)
	Status       string `json:"status"`
	Frames       int    `json:"frames"` // netpack reports received for this message
	FramesOK     int    `json:"frames_ok"`
	FramesFailed int    `json:"frames_failed"`
	Retries      int    `json:"retries"` // worst per-frame retry count seen
}

// DeliveryBatch is the POST body.
type DeliveryBatch struct {
	Messages []DeliveryMessage `json:"messages"`
}

package elrs

import "encoding/json"

// Command types for the race-director → venue queue.
const (
	CmdSetChannel = "set_channel"
	CmdOSDMessage = "osd_message"
	CmdClearOSD   = "clear_osd"
	CmdPing       = "ping"
	CmdTestLayout = "test_layout"
)

// Command target scopes.
const (
	TargetPilot = "pilot"
	TargetAll   = "all"
)

// Command statuses.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusAcked     = "acked"
	StatusFailed    = "failed"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
)

// CommandWire is a queued command as delivered to dd-pits over the long-poll
// endpoint GET /dronedash/v2/elrs-commands.
type CommandWire struct {
	ID       int64           `json:"id"`
	Type     string          `json:"type"`
	Target   string          `json:"target"`   // "pilot" | "all"
	Callsign string          `json:"callsign"` // FPVTrackside name; empty for target=all
	Payload  json.RawMessage `json:"payload"`
}

// SetChannelPayload is CommandWire.Payload for set_channel. Index is the
// precomputed backpack channel index; band/channel travel too so the agent
// can verify with ChannelIndex.
type SetChannelPayload struct {
	Band    string `json:"band"`
	Channel int    `json:"channel"`
	Index   int    `json:"index"`
}

// OSDMessagePayload is CommandWire.Payload for osd_message.
type OSDMessagePayload struct {
	Text       string `json:"text"`
	DurationMs int    `json:"duration_ms"`
}

// TestLayoutPayload is CommandWire.Payload for test_layout: a self-contained
// goggle preview a pilot triggers from the layout editor. It carries the UID
// and layout directly (not a callsign) so the venue agent can render it even
// for a pilot who isn't in the current heat's FPVTrackside roster.
type TestLayoutPayload struct {
	UID        string `json:"uid"`
	Layout     Layout `json:"layout"`
	DurationMs int    `json:"duration_ms"`
}

// CommandsResponse is the long-poll response body.
type CommandsResponse struct {
	Commands []CommandWire `json:"commands"`
}

// CommandAck is one entry of the ack POST body.
type CommandAck struct {
	ID     int64  `json:"id"`
	Status string `json:"status"` // "acked" | "failed"
	Detail string `json:"detail,omitempty"`
}

// CommandAckRequest is the body of POST /dronedash/v2/elrs-commands/ack.
type CommandAckRequest struct {
	Results []CommandAck `json:"results"`
	// Agent state piggybacked for the race-director console.
	BackpackConnected bool   `json:"backpack_connected"`
	BackpackVersion   string `json:"backpack_version,omitempty"`
	AgentVersion      string `json:"agent_version,omitempty"`
}

// ConfigPilot is one pilot's entry in the venue config response.
type ConfigPilot struct {
	UID    string  `json:"uid"`              // 12 hex chars
	Layout *Layout `json:"layout,omitempty"` // nil = use DefaultLayout from config
}

// ConfigResponse is the body of GET /dronedash/v2/elrs-config.
type ConfigResponse struct {
	EventID       int64                  `json:"event_id"`
	DefaultLayout Layout                 `json:"default_layout"`
	Pilots        map[string]ConfigPilot `json:"pilots"` // keyed by uppercase callsign
	// OptOut lists uppercased callsigns of pilots who have an ELRS UID on
	// file but explicitly disabled OSD via the master switch. They are
	// absent from Pilots (like an unconfigured pilot), so the agent needs
	// this to tell "opted out" apart from "never set up" and skip them even
	// when callsign-derived-UID fallback is on.
	OptOut []string `json:"opt_out,omitempty"`
}

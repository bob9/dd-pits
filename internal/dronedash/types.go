// Package dronedash ingests live race data from FPVTrackside, links pilots to
// platform users by fuzzy name matching, and fans out updates to SSE subscribers
// for live race pages and OBS overlays.
package dronedash

// FPVTrackside live race-day JSON shapes (subset of fields we use).
// These mirror drone-dashboard/backend/ingest/types.go but are intentionally
// minimal — we only decode what we need.

type ftEvent struct {
	ID                          string   `json:"ID"`
	Name                        string   `json:"Name"`
	EventType                   string   `json:"EventType"`
	Start                       string   `json:"Start"`
	End                         string   `json:"End"`
	Laps                        int      `json:"Laps"`
	PBLaps                      int      `json:"PBLaps"`
	PackLimit                   int      `json:"PackLimit"`
	PrimaryTimingSystemLocation string   `json:"PrimaryTimingSystemLocation"`
	Rounds                      []string `json:"Rounds"`
	Races                       []string `json:"Races"`
	Channels                    []string `json:"Channels"`
	ChannelColors               []string `json:"ChannelColors"`
}

type ftPilot struct {
	ID            string `json:"ID"`
	Name          string `json:"Name"`
	FirstName     string `json:"FirstName"`
	LastName      string `json:"LastName"`
	DiscordID     string `json:"DiscordID"`
	PracticePilot bool   `json:"PracticePilot"`
	// ExternalID is FPVTrackside's integer BaseObject.ExternalID, set by the
	// paste-import extension (the optional 3rd CSV column) to an external
	// system's pilot id — for us the race_bracket_pilots.id — so a result routes
	// back to the exact bracket pilot without fuzzy name matching. 0 for pilots
	// added by name only.
	ExternalID int `json:"ExternalID"`
}

type ftChannel struct {
	ID            string `json:"ID"`
	Number        int    `json:"Number"`
	Band          string `json:"Band"`
	ShortBand     string `json:"ShortBand"`
	ChannelPrefix string `json:"ChannelPrefix"`
	Frequency     int    `json:"Frequency"`
	DisplayName   string `json:"DisplayName"`
}

type ftRound struct {
	ID          string `json:"ID"`
	Name        string `json:"Name"`
	RoundNumber int    `json:"RoundNumber"`
	EventType   string `json:"EventType"`
	RoundType   string `json:"RoundType"`
	Valid       bool   `json:"Valid"`
	Order       int    `json:"Order"`
}

type ftDetection struct {
	ID           string `json:"ID"`
	Channel      string `json:"Channel"`
	Time         string `json:"Time"`
	Pilot        string `json:"Pilot"`
	LapNumber    int    `json:"LapNumber"`
	Valid        bool   `json:"Valid"`
	ValidityType string `json:"ValidityType"`
	IsLapEnd     bool   `json:"IsLapEnd"`
	IsHoleshot   bool   `json:"IsHoleshot"`
	// Split-timer fields (FPVTrackside TimingSystemRole Primary/Split):
	// TimingSystemIndex 0 = the primary (start/finish) gate, 1..N = split
	// timers in configured order; RaceSector = LapNumber*100 + index. Absent
	// on old agents/stock builds — zero values mean "no sector data", which the
	// ingest treats as not-a-split, so nothing changes for existing venues.
	TimingSystemIndex int `json:"TimingSystemIndex"`
	RaceSector        int `json:"RaceSector"`
}

type ftLap struct {
	ID            string  `json:"ID"`
	Detection     string  `json:"Detection"`
	LengthSeconds float64 `json:"LengthSeconds"`
	LapNumber     int     `json:"LapNumber"`
	StartTime     string  `json:"StartTime"`
	EndTime       string  `json:"EndTime"`
}

type ftPilotChannel struct {
	ID      string `json:"ID"`
	Pilot   string `json:"Pilot"`
	Channel string `json:"Channel"`
}

type ftRace struct {
	ID              string           `json:"ID"`
	Laps            []ftLap          `json:"Laps"`
	Detections      []ftDetection    `json:"Detections"`
	Start           string           `json:"Start"`
	End             string           `json:"End"`
	TotalPausedTime string           `json:"TotalPausedTime"`
	PilotChannels   []ftPilotChannel `json:"PilotChannels"`
	RaceNumber      int              `json:"RaceNumber"`
	Round           string           `json:"Round"`
	TargetLaps      int              `json:"TargetLaps"`
	Valid           bool             `json:"Valid"`
	Event           string           `json:"Event"`
	Bracket         string           `json:"Bracket"`
	// ExternalID is FPVTrackside's integer BaseObject.ExternalID, set by the
	// paste-import extension (the optional 2nd CSV column) to an external
	// system's race id — for us a bracket_races.id — so a finished heat's result
	// routes back to the exact bracket race. 0 for heats created by name only.
	ExternalID int `json:"ExternalID"`
}

type ftResult struct {
	ID         string `json:"ID"`
	Points     int    `json:"Points"`
	Position   int    `json:"Position"`
	Valid      bool   `json:"Valid"`
	Event      string `json:"Event"`
	Pilot      string `json:"Pilot"`
	Race       string `json:"Race"`
	Round      string `json:"Round"`
	DNF        bool   `json:"DNF"`
	ResultType string `json:"ResultType"`
}

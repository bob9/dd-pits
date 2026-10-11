package main

// ELRS pilot config: per-pilot backpack UIDs and OSD layouts from the venue's
// settings file, resolving FPVTrackside callsigns to goggle UIDs for the OSD
// engine.

import (
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/elrs"
)

// elrsController owns everything backpack-side: the connection, the synced
// pilot config and the snapshot-diffing OSD engine.
type elrsController struct {
	// localCfg is the venue's own settings file — the source of pilot bind
	// phrases and OSD layouts. nil when the agent was built without one.
	localCfg *localConfig

	bp *elrs.Backpack

	// Channel-change redundancy (see executeChannelBatch): every pilot's
	// change is transmitted channelRepeats times, channelRepeatGap apart,
	// interleaved across pilots so a whole heat completes in one window.
	channelRepeats   int
	channelRepeatGap time.Duration

	cfg atomic.Pointer[elrs.ConfigResponse]

	// Engine state (guarded by mu; engine goroutine + command loop + timers).
	mu          sync.Mutex
	latest      *dronedash.Snapshot
	wake        chan struct{}
	primed      bool
	races       map[string]*elrsRaceTrack
	lastCue     string
	lastMeeting *meetingView
	// rowGen invalidates pending timed row-clears when a newer write lands
	// on the same (uid, row).
	rowGen map[string]uint64
	// goSentAt/goFramesOK track the in-flight GO! send per goggle: netpack
	// delivery reports increment goFramesOK, and a resend timer re-fires the
	// GO! session if too few frames were acked in time.
	goSentAt    map[elrs.UID]time.Time
	goFramesOK  map[elrs.UID]int
	goFramesBad map[elrs.UID]int
	// reportsSeen: at least one netpack delivery report has arrived — the
	// GO! resend loop only trusts "0 frames acked" as a loss signal when
	// reports are actually flowing.
	reportsSeen bool
	// goMissed: pilots whose start-edge clear+GO! session failed outright
	// (backpack mid-reconnect, write error) — nothing reached the air, so no
	// delivery report can drive a resend. Retried from the next snapshot
	// while the heat is still running (see resendMissedGo).
	goMissed map[elrs.UID]*goMissedState
	// rearmDelay defers the start-time DVR label re-arm out of the start
	// window (dvrStartRearmDelay; tests shrink it).
	rearmDelay time.Duration
	// Active DVR label, re-sent periodically so the goggles' 5-minute label
	// TTL can't lapse while a heat sits staged (see refreshDVRLabel).
	labelRaceID string
	label       string
	labelSentAt time.Time
	// labelGen counts label arms. The start-time re-arm captures it and bails
	// if it moved, so a heat the operator pre-cued during the start window
	// keeps ITS label instead of being overwritten by the heat that just
	// went green (shared pilots would otherwise record under the old name).
	labelGen uint64

	// Command-loop bookkeeping.
	lastCmdID int64

	// journal records every logical goggle message + its delivery outcome
	// (netpack FuncSendReport acks); deliveryLoop uploads it to the cloud.
	journal *deliveryJournal
}

type elrsRaceTrack struct {
	state string         // "pending" | "running" | "ended"
	laps  map[string]int // callsign -> displayed (counted) laps
	done  map[string]bool
	// startMs is the heat's start stamp as last observed. A re-run gets a
	// NEW stamp, which is what distinguishes a genuine restart from a heat
	// that merely bounced ended→running (see processSnapshot).
	startMs int64
}

// goMissedState tracks a failed start-edge clear+GO! per goggle so it can be
// retried on later snapshots of the same heat, bounded by goMissedMax.
type goMissedState struct {
	raceID string
	tries  int
}

// applyLocal loads the venue's pilots into the engine.
//
// Separate from the cloud path on purpose: it produces the SAME
// elrs.ConfigResponse the endpoint returns, so the OSD engine, the diffing and
// every test are indifferent to where it came from.
func (c *elrsController) applyLocal() int {
	if c.localCfg == nil {
		return 0
	}
	cfg := c.localCfg.ELRSConfig()
	c.cfg.Store(&cfg)
	return len(cfg.Pilots)
}

func newELRSController(bp *elrs.Backpack) *elrsController {
	return &elrsController{
		bp: bp,
		// Fixed redundant channel sends, spread ~250ms apart so frames land
		// in gaps of the ELRS TX schedule at the radio backpack. Open-loop
		// (no echo query) was the most reliable on hardware.
		channelRepeats:   12,
		channelRepeatGap: 250 * time.Millisecond,
		wake:             make(chan struct{}, 1),
		races:            map[string]*elrsRaceTrack{},
		rowGen:           map[string]uint64{},
		rearmDelay:       dvrStartRearmDelay,
		goSentAt:         map[elrs.UID]time.Time{},
		goFramesOK:       map[elrs.UID]int{},
		goFramesBad:      map[elrs.UID]int{},
		goMissed:         map[elrs.UID]*goMissedState{},
		journal:          newDeliveryJournal(),
	}
}

// pilotOSD resolves a FPVTrackside callsign to a goggle UID + OSD layout.
// Only pilots with a configured ELRS bind phrase resolve — there is no
// callsign-derived-UID fallback. Guessed UIDs almost never matched a real
// goggle (pilots don't bind to their callsign), so every frame to one burned
// the backpack's full ESP-NOW retry budget for nothing, delaying real OSD
// traffic. An unresolved pilot is simply never messaged.
func (c *elrsController) pilotOSD(callsign string) (elrs.UID, elrs.Layout, bool) {
	key := strings.ToUpper(strings.TrimSpace(callsign))
	if key == "" || key == "—" {
		return elrs.UID{}, elrs.Layout{}, false
	}

	cfg := c.cfg.Load()
	if cfg == nil {
		return elrs.UID{}, elrs.Layout{}, false
	}
	p, ok := cfg.Pilots[key]
	if !ok {
		// Unconfigured, or explicitly opted out (the server drops OSD-off
		// pilots from Pilots and lists them in OptOut) — never messaged.
		return elrs.UID{}, elrs.Layout{}, false
	}
	uid, err := elrs.ParseUID(p.UID)
	if err != nil {
		log.Printf("elrs: pilot %s has bad uid %q: %v", key, p.UID, err)
		return elrs.UID{}, elrs.Layout{}, false
	}
	layout := cfg.DefaultLayout
	if p.Layout != nil {
		layout = *p.Layout
	}
	return uid, layout, true
}

// staleLayoutRows returns the screen rows occupied by enabled elements in
// oldL that no enabled element occupies in newL — the rows a layout edit
// (element hidden, moved, or shrunk) left showing stale text. Rows an
// enabled element still occupies are left alone: its next write overwrites
// them, and clearing would blank a live element.
func staleLayoutRows(oldL, newL elrs.Layout) []int {
	occupied := func(l elrs.Layout) map[int]bool {
		rows := map[int]bool{}
		for slug, p := range l.Elements {
			if !p.Enabled {
				continue
			}
			n := 1
			if slug == elrs.ElemRecentLaps {
				n = p.RecentLapWindow()
			}
			for i := 0; i < n && p.Row+i < elrs.OSDRows; i++ {
				rows[p.Row+i] = true
			}
		}
		return rows
	}
	oldRows, newRows := occupied(oldL), occupied(newL)
	var stale []int
	for r := range oldRows {
		if !newRows[r] {
			stale = append(stale, r)
		}
	}
	sort.Ints(stale)
	return stale
}

// applyConfigChanges makes a config edit visible on goggles immediately: a
// hidden/moved element's vacated rows are cleared, and a pilot dropped from
// the config entirely (OSD master switch off / opt-out) gets one full screen
// clear — "hidden" must mean gone from the goggles, not just no-longer-sent.
func (c *elrsController) applyConfigChanges(oldCfg, newCfg *elrs.ConfigResponse) {
	if oldCfg == nil || newCfg == nil || !c.bp.Connected() {
		return
	}
	effective := func(cfg *elrs.ConfigResponse, p elrs.ConfigPilot) elrs.Layout {
		if p.Layout != nil {
			return *p.Layout
		}
		return cfg.DefaultLayout
	}
	done := map[elrs.UID]bool{}
	for callsign, op := range oldCfg.Pilots {
		uid, err := elrs.ParseUID(op.UID)
		if err != nil || done[uid] {
			continue
		}
		np, ok := newCfg.Pilots[callsign]
		if !ok {
			done[uid] = true
			log.Printf("elrs: %s removed from OSD config — clearing their screen", callsign)
			if err := c.clearOSD(uid); err != nil && err != elrs.ErrNotConnected {
				log.Printf("elrs: config-change screen clear for %s: %v", callsign, err)
			}
			continue
		}
		stale := staleLayoutRows(effective(oldCfg, op), effective(newCfg, np))
		if len(stale) == 0 {
			continue
		}
		done[uid] = true
		log.Printf("elrs: %s layout changed — clearing %d stale row(s)", callsign, len(stale))
		err = c.bp.Session(uid, func(s *elrs.Session) error {
			for _, row := range stale {
				if err := s.ClearRow(row); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil && err != elrs.ErrNotConnected {
			log.Printf("elrs: stale-row clear for %s: %v", callsign, err)
		}
	}
}

// currentHeatCallsigns lists the pilots the OSD engine considers "in the
// current heat" — the running heat, else the cued heat.
func (c *elrsController) currentHeatCallsigns() []string {
	c.mu.Lock()
	mv := c.lastMeeting
	c.mu.Unlock()
	if mv == nil || mv.Current == nil {
		return nil
	}
	var out []string
	for _, p := range mv.Current.Pilots {
		if p.Name != "" && p.Name != "—" {
			out = append(out, p.Name)
		}
	}
	return out
}

// loadConfig loads pilot config from the venue's settings file. The CONTENT of
// every goggle message — lap times, DVR names, the clock — is built from
// FPVTrackside data on this machine; the settings file supplies the ADDRESS
// (each pilot's bind phrase) to send it to.
func (c *elrsController) loadConfig() {
	if c.localCfg == nil {
		log.Printf("elrs: no pilot config source — goggle messaging is off " +
			"(run with -serve and add pilots at /setup/pilots)")
		return
	}
	log.Printf("elrs: pilot config from the local settings file (%d pilots) — "+
		"edit at /setup/pilots", c.applyLocal())
}

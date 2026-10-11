package main

// Race-director goggle commands driven from the local setup pages: channel
// changes and OSD announcements.

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bob9/dd-pits/internal/elrs"
)

// setChannelByCallsign changes one pilot's video channel from the local setup
// pages.
//
// The cloud path for this is a command QUEUE the agent polls; offline there is
// no queue, so the timing desk drives the backpack directly. Same validation as
// the queued path — the band/channel is resolved to an index by the ELRS
// library rather than trusted — because putting a pilot on the wrong frequency
// mid-meeting is worse than refusing.
func (c *elrsController) setChannelByCallsign(callsign, label string) error {
	if c == nil || c.bp == nil {
		return fmt.Errorf("no backpack")
	}
	band, channel, err := splitChannelLabel(label)
	if err != nil {
		return err
	}
	idx, err := elrs.ChannelIndex(band, channel)
	if err != nil {
		return fmt.Errorf("channel %s: %w", label, err)
	}
	if !c.bp.Connected() {
		return fmt.Errorf("no backpack connected")
	}

	cfg := c.cfg.Load()
	if cfg == nil {
		return fmt.Errorf("no pilot config loaded")
	}
	pilot, ok := cfg.Pilots[strings.ToUpper(strings.TrimSpace(callsign))]
	if !ok {
		return fmt.Errorf("%s has no goggle set up", callsign)
	}
	uid, err := elrs.ParseUID(pilot.UID)
	if err != nil {
		return fmt.Errorf("%s has an unusable goggle id: %w", callsign, err)
	}

	// Sent repeatedly for the same reason the queued path does: ESP-NOW is
	// lossy and a missed channel change strands a pilot on the wrong video.
	var last error
	for i := 0; i < c.channelRepeats; i++ {
		if err := c.bp.SetChannelIndex(uid, idx); err != nil {
			last = err
		}
	}
	c.journalMsg(uid, callsign, nil, "channel", label)
	if last != nil {
		return last
	}
	log.Printf("elrs: %s set to %s (index %d) from the local setup page", callsign, label, idx)
	return nil
}

// splitChannelLabel turns "R7" into ("R", 7).
func splitChannelLabel(label string) (string, int, error) {
	l := strings.ToUpper(strings.TrimSpace(label))
	if len(l) < 2 {
		return "", 0, fmt.Errorf("channel %q is not a band and a number", label)
	}
	n, err := strconv.Atoi(l[1:])
	if err != nil {
		return "", 0, fmt.Errorf("channel %q is not a band and a number", label)
	}
	return l[:1], n, nil
}

// announceToPilots puts a message on the goggle screens of the pilots named,
// or of everyone configured when none are.
//
// The race director's equivalent of shouting across the pits, and the reason it
// is worth having: a pilot in goggles cannot hear the PA and cannot see a hand
// waved at them. "TRACK CLEAR", "HOLD", "COME IN" reach them where they are.
//
// Reported per pilot rather than as one result: with a field of six, "it did
// not work" is useless — which pilot missed it is the whole question.
func (c *elrsController) announceToPilots(text string, callsigns []string) (sent []string, failed map[string]error) {
	failed = map[string]error{}
	if c == nil || c.bp == nil {
		failed["*"] = fmt.Errorf("no backpack")
		return nil, failed
	}
	text = strings.TrimSpace(text)
	if text == "" {
		failed["*"] = fmt.Errorf("nothing to say")
		return nil, failed
	}
	if !c.bp.Connected() {
		failed["*"] = fmt.Errorf("no backpack connected")
		return nil, failed
	}

	cfg := c.cfg.Load()
	if cfg == nil || len(cfg.Pilots) == 0 {
		failed["*"] = fmt.Errorf("no pilots configured")
		return nil, failed
	}

	// Uppercased because the goggle font has no lowercase — sending it
	// lowercase renders as blanks rather than as small letters.
	msg := strings.ToUpper(text)

	want := map[string]bool{}
	for _, cs := range callsigns {
		if cs = strings.TrimSpace(cs); cs != "" {
			want[strings.ToUpper(cs)] = true
		}
	}

	for callsign, p := range cfg.Pilots {
		if len(want) > 0 && !want[callsign] {
			continue
		}
		uid, err := elrs.ParseUID(p.UID)
		if err != nil {
			failed[callsign] = err
			continue
		}
		layout := cfg.DefaultLayout
		if p.Layout != nil {
			layout = *p.Layout
		}
		if err := c.sendAnnouncement(uid, layout, msg, announceHold); err != nil {
			failed[callsign] = err
			continue
		}
		c.journalMsg(uid, callsign, nil, "announcement", msg)
		sent = append(sent, callsign)
	}
	sort.Strings(sent)
	return sent, failed
}

// announceHold is how long a hand-sent message stays up.
//
// Long enough to be read by someone who is flying and cannot look away, short
// enough that it is gone before it becomes clutter on the next lap.
const announceHold = 8 * time.Second

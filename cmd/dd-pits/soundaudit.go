package main

// Checking that FPVTrackside's sound settings and dd-pits agree.
//
// The two are coupled in a way that is easy to get wrong and gives no symptom
// except silence. dd-pits takes its WORDING from Sounds.xml and honours the
// enabled flag, so a sound switched off there is a sound dd-pits will not say —
// even for the calls dd-pits produces itself from the timing data.
//
// That is the right behaviour: "off" means the race director does not want it
// said, and dd-pits speaking anyway would be ignoring them. But it means a
// venue that switches off RaceLap expecting to quieten FPVTrackside quietens
// dd-pits instead, and nothing announces it. That happened here: lap calls went
// missing for a long stretch of testing, and the only trace was a single line
// buried in a log.
//
// So the configuration is audited at startup and reported plainly: which calls
// dd-pits will make, which it will relay, and which are switched off and
// therefore silent everywhere.

import (
	"fmt"
	"log"
	"sort"
	"strings"
)

// soundRole is where a call comes from.
type soundRole int

const (
	roleDerived soundRole = iota // dd-pits builds it from the timing data
	roleRelayed                  // FPVTrackside decides it, dd-pits voices it
	roleSilent                   // switched off: nothing says it
)

// soundAudit is the startup picture of one sound.
type soundAudit struct {
	Key  SoundKey
	Role soundRole
	Text string
}

// auditSounds reports where each call will come from.
//
// The distinction that matters: a DERIVED call switched off is almost certainly
// a mistake, because the operator's reason for switching it off — stopping
// FPVTrackside saying it — is already handled by muting FPVTrackside's output.
// A RELAYED call switched off is a normal choice.
func auditSounds(s *SoundSet) []soundAudit {
	if s == nil {
		return nil
	}

	derived := map[SoundKey]bool{}
	for _, k := range derivedSounds {
		derived[k] = true
	}

	var out []soundAudit
	for _, key := range s.Keys() {
		text, enabled := s.Template(key)
		switch {
		case !enabled:
			out = append(out, soundAudit{Key: key, Role: roleSilent})
		case derived[key]:
			out = append(out, soundAudit{Key: key, Role: roleDerived, Text: text})
		default:
			out = append(out, soundAudit{Key: key, Role: roleRelayed, Text: text})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// reportSoundConfig logs the audit, and warns about the combination that is
// almost always unintended.
func reportSoundConfig(s *SoundSet) {
	audit := auditSounds(s)
	if len(audit) == 0 {
		return
	}

	var derived, relayed, silent []string
	for _, a := range audit {
		switch a.Role {
		case roleDerived:
			derived = append(derived, string(a.Key))
		case roleRelayed:
			relayed = append(relayed, string(a.Key))
		case roleSilent:
			silent = append(silent, string(a.Key))
		}
	}

	log.Printf("race audio: %d calls from the timing data (%s)",
		len(derived), strings.Join(derived, ", "))
	log.Printf("race audio: %d relayed from FPVTrackside", len(relayed))

	// The warning worth making loud.
	//
	// A derived call switched off is silent EVERYWHERE — dd-pits will not say
	// it, and FPVTrackside is muted — which is rarely what anyone wanted. The
	// message names the fix, because "switched off in FPVTrackside" was not
	// enough to find it last time.
	var offDerived []string
	derivedSet := map[SoundKey]bool{}
	for _, k := range derivedSounds {
		derivedSet[k] = true
	}
	for _, a := range audit {
		if a.Role == roleSilent && derivedSet[a.Key] {
			offDerived = append(offDerived, string(a.Key))
		}
	}
	if len(offDerived) > 0 {
		log.Printf("race audio: WARNING — %s %s switched OFF in FPVTrackside's sound settings, "+
			"so nothing will say %s. dd-pits takes its wording from there and honours the flag. "+
			"If you turned it off to quieten FPVTrackside, that is already handled by muting its "+
			"audio — switch it back on to hear %s again.",
			strings.Join(offDerived, ", "),
			pick(len(offDerived), "is", "are"),
			pick(len(offDerived), "it", "them"),
			pick(len(offDerived), "it", "them"))
	}

	if len(silent) > 0 {
		log.Printf("race audio: %d switched off in FPVTrackside (silent everywhere): %s",
			len(silent), strings.Join(silent, ", "))
	}
}

// pick chooses singular or plural wording. Named to avoid colliding with the
// existing plural() in booth_obs.go, which pluralises differently.
func pick(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// soundConfigSummary renders the audit for the operator page.
func soundConfigSummary(s *SoundSet) string {
	var b strings.Builder
	for _, a := range auditSounds(s) {
		switch a.Role {
		case roleDerived:
			fmt.Fprintf(&b, "%-24s from the timing data\n", a.Key)
		case roleRelayed:
			fmt.Fprintf(&b, "%-24s relayed from FPVTrackside\n", a.Key)
		case roleSilent:
			fmt.Fprintf(&b, "%-24s OFF — nothing says this\n", a.Key)
		}
	}
	return b.String()
}

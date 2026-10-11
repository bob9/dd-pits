package booth

// THE STEWARDS' ROOM — data-triggered comedy.
//
// Asking a model to "be funny" reliably produces generic dad jokes, because it
// has nothing specific to be funny ABOUT. Handing it a real, named absurdity
// that actually happened — a pilot who just smashed their meeting PB by 12%, a
// lap three times slower than their own average, a qualifying sandbagger who
// suddenly found a second in the finals — gives the joke a target and makes it
// land. The commentator's job stops being invention and becomes reaction.
//
// So: detect the anomalies here, in pure code, from numbers we already have,
// and hand the booth a short list of prompts it MAY riff on. Every trigger is
// individually toggleable by the club, every one names the pilot it's about so
// a pilot who opted out of ribbing can be filtered, and none of them are
// mandatory — the prompt asks for at most a couple, so a heat with five
// anomalies doesn't turn into a comedy routine.
//
// Deliberately excluded: anything about crashing, going down, or a DNF. Those
// are the pilot's worst moment of the day and are never the bit.

import (
	"fmt"
	"sort"
	"strings"
)

// Trigger keys. Stored in club config as a toggle map, so they are a stable
// contract — renaming one silently re-enables it.
const (
	TrigDrugsTest    = "drugs_test"
	TrigStewards     = "stewards"
	TrigScenicRoute  = "scenic_route"
	TrigOneHotLap    = "one_hot_lap"
	TrigHoleshotHero = "holeshot_hero"
	TrigSandbagger   = "sandbagger"
	TrigBreakthrough = "breakthrough"
)

// TriggerCatalogue is the admin-pickable list, in display order. Note is the
// settings-card explanation; Prompt is the line handed to the model, with %s
// substituted for the callsign.
var TriggerCatalogue = []struct {
	Key, Name, Note, Prompt string
	// Default is whether a club that has never touched the setting gets it.
	// The gentler and funnier ones are on; the sharper ones are opt-in.
	Default bool
}{
	{
		Key: TrigDrugsTest, Name: "🧪 Drugs test", Default: true,
		Note:   "A pilot obliterates their own meeting PB — suspiciously good.",
		Prompt: "%s just demolished their own meeting personal best by a suspicious margin — joke that we might need to send that quad to the lab / it's going straight to scrutineering.",
	},
	{
		Key: TrigStewards, Name: "🔍 Stewards' inquiry", Default: true,
		Note:   "A lap wildly out of line with that pilot's own pace this meeting.",
		Prompt: "%s put in a lap completely out of step with everything else they've flown today — joke that the stewards might want a review of that round.",
	},
	{
		Key: TrigScenicRoute, Name: "🐌 Scenic route", Default: true,
		Note:   "A lap far slower than that pilot's own usual — clearly went wandering.",
		Prompt: "%s took one lap dramatically slower than their own usual pace — joke about taking the scenic route / stopping to admire the view. Never suggest they crashed.",
	},
	{
		Key: TrigOneHotLap, Name: "🎰 One hot lap and a lie down", Default: true,
		Note:   "Big gap between their best single lap and their scoring consecutive run.",
		Prompt: "%s has one blindingly quick lap but a scoring run well off that pace — joke that they've got one hot lap in them and then a bit of a lie down.",
	},
	{
		Key: TrigHoleshotHero, Name: "🏃 Holeshot hero", Default: true,
		Note:   "Wins the sprint to gate one, then finishes at the back. Races only.",
		Prompt: "%s won the holeshot and then finished at the back — joke that they peaked at gate one / led the race that mattered least.",
	},
	{
		Key: TrigSandbagger, Name: "😴 Sandbagger", Default: false,
		Note:   "Qualified well down the board, then suddenly quick when it counts.",
		Prompt: "%s qualified well down the board and has suddenly found serious pace now it counts — joke about where exactly that was hiding during qualifying.",
	},
	{
		Key: TrigBreakthrough, Name: "🎉 Breakthrough", Default: true,
		Note:   "A genuine first — a maiden PB-setting run at the sharp end. Celebratory, not a roast.",
		Prompt: "%s has just put in the run of their meeting and moved to the sharp end — celebrate it properly and warmly. This is NOT a roast.",
	},
}

// TriggerDefaults is the toggle map a club gets before they touch the setting.
func TriggerDefaults() map[string]bool {
	m := make(map[string]bool, len(TriggerCatalogue))
	for _, t := range TriggerCatalogue {
		m[t.Key] = t.Default
	}
	return m
}

// triggerPrompt looks a key's prompt template up.
func triggerPrompt(key string) string {
	for _, t := range TriggerCatalogue {
		if t.Key == key {
			return t.Prompt
		}
	}
	return ""
}

// Trigger is one detected bit: which gag, about whom, and the line the model is
// invited to riff on.
type Trigger struct {
	Key    string
	Pilot  string
	Prompt string
	// Score ranks competing triggers so the most absurd survives the cap.
	// Comparable only within a single detection run.
	Score float64
}

// PilotForm is everything DetectTriggers needs about one pilot in one heat.
// Both the server-side replay generator and the venue agent can fill it from
// what they already hold, so the two can't drift on what counts as funny.
type PilotForm struct {
	Callsign string
	// Laps are the racing laps of THIS heat in order, holeshot excluded.
	Laps []float64
	// Holeshot is the sprint to gate one (0 = unknown / not applicable).
	Holeshot float64
	// Position is the finishing position in this heat (0 = unknown).
	Position int
	// FieldSize is how many pilots were in the heat (for "finished at the back").
	FieldSize int
	// BestLap / BestConsec are this heat's numbers.
	BestLap, BestConsec float64
	// PriorPB is their best consecutive run across the meeting BEFORE this
	// heat (0 = no prior form, so no "smashed their PB" claim is possible).
	PriorPB float64
	// TypicalLap is their median racing lap across the whole meeting — the
	// yardstick for "out of step with their own pace". 0 = unknown.
	TypicalLap float64
	// QualPos is their rank on the meeting's qualifying board (0 = unranked).
	QualPos int
	// FieldQualSize is how many pilots are on that board.
	FieldQualSize int
	// NoRoast is the pilot's own opt-out. Only the celebratory Breakthrough
	// trigger may still fire for them.
	NoRoast bool
}

// Detection thresholds. Tuned to fire rarely — a gag that lands every heat
// stops being a gag. Exported so the tests state the contract in one place.
const (
	// TrigPBSmashPct — beat your own meeting PB by this fraction to earn a lab test.
	TrigPBSmashPct = 0.08
	// TrigOutlierFast — a lap this much faster than your own typical is "review that round".
	TrigOutlierFast = 0.80
	// TrigScenicMult — a lap this many times your own typical is the scenic route.
	TrigScenicMult = 2.0
	// TrigSpreadPct — scoring run this much worse than best-lap × window is one-hot-lap.
	TrigSpreadPct = 0.15
	// TrigSandbagQuantile — qualified in the bottom half of the board...
	TrigSandbagQuantile = 0.5
	// ...and finished in the top this-many positions.
	TrigSandbagFinish = 2
	// TrigMaxPerHeat caps how many bits reach the prompt.
	TrigMaxPerHeat = 2
)

// TriggerOpts says WHEN a detection run is happening, which changes what can
// honestly be claimed.
type TriggerOpts struct {
	// Live marks a heat that is still running. Mid-heat, PilotForm.Position is
	// the RUNNING order, not a finishing position — so the two triggers that
	// depend on where a pilot finished (holeshot hero, sandbagger) are dropped.
	// Calling a pilot "peaked at gate one and finished at the back" when they
	// are P4 with two laps left is simply wrong, and the whole point of the
	// Stewards' Room is that every bit is backed by the numbers.
	Live bool
}

// TriggerBeat reports whether a beat may carry Stewards' Room gags.
//
// The finish call always may — it has the completed heat in front of it. The
// in-heat ACTION calls only when the club has opted in (boothTenantConfig
// .TriggersInHeat): the gags are reactions to the timing data, which is just as
// available mid-heat, but they lengthen a call that has to land on the live
// video, so it's the race director's choice.
//
// Deliberately never: pilot_out / pilot_back. A pilot going down is the one
// moment the guardrail rules out entirely, and it is exactly the beat where a
// gag would land worst. The meeting-level beats (welcome, filler, meeting_talk,
// qual_move) are about the meeting, not this heat's data, so they stay clean.
func TriggerBeat(kind string, inHeat bool) bool {
	if kind == "finish" {
		return true
	}
	if !inHeat {
		return false
	}
	switch kind {
	case "launch", "holeshot", "progress":
		return true
	}
	return false
}

// DetectTriggers finds the enabled anomalies in a finished heat, strongest
// first, capped at TrigMaxPerHeat. enabled == nil means "the defaults".
//
// Pure: same inputs, same output, no clock, no randomness — so a regenerate
// produces the same bits as the original run.
func DetectTriggers(pilots []PilotForm, isTimeTrial bool, window int, enabled map[string]bool) []Trigger {
	return DetectTriggersOpt(pilots, isTimeTrial, window, enabled, TriggerOpts{})
}

// DetectTriggersOpt is DetectTriggers with the timing of the run declared.
// DetectTriggers is the zero-opts (finished heat) wrapper.
func DetectTriggersOpt(pilots []PilotForm, isTimeTrial bool, window int, enabled map[string]bool, opts TriggerOpts) []Trigger {
	if enabled == nil {
		enabled = TriggerDefaults()
	}
	on := func(key string) bool { return enabled[key] }

	var found []Trigger
	add := func(key, pilot string, score float64, noRoast bool) {
		if !on(key) {
			return
		}
		// A pilot who opted out of ribbing is only ever celebrated.
		if noRoast && key != TrigBreakthrough {
			return
		}
		if p := triggerPrompt(key); p != "" {
			found = append(found, Trigger{Key: key, Pilot: pilot, Prompt: fmt.Sprintf(p, pilot), Score: score})
		}
	}

	for _, p := range pilots {
		if len(p.Laps) == 0 {
			continue
		}

		// 🧪 Smashed their own meeting PB by a suspicious margin.
		if p.PriorPB > 0 && p.BestConsec > 0 && p.BestConsec < p.PriorPB {
			gain := (p.PriorPB - p.BestConsec) / p.PriorPB
			if gain >= TrigPBSmashPct {
				add(TrigDrugsTest, p.Callsign, gain, p.NoRoast)
			}
		}

		if p.TypicalLap > 0 {
			fastest, slowest := p.Laps[0], p.Laps[0]
			for _, l := range p.Laps {
				if l < fastest {
					fastest = l
				}
				if l > slowest {
					slowest = l
				}
			}
			// 🔍 A lap far quicker than anything else they do — review that round.
			if fastest > 0 && fastest <= p.TypicalLap*TrigOutlierFast {
				add(TrigStewards, p.Callsign, (p.TypicalLap-fastest)/p.TypicalLap, p.NoRoast)
			}
			// 🐌 A lap far slower than their own usual — the scenic route.
			if slowest >= p.TypicalLap*TrigScenicMult {
				add(TrigScenicRoute, p.Callsign, slowest/p.TypicalLap, p.NoRoast)
			}
		}

		// 🎰 One hot lap, then nothing like it. Only meaningful where a
		// consecutive run is what's scored and they flew the full window.
		if window > 1 && p.BestLap > 0 && p.BestConsec > 0 && len(p.Laps) >= window {
			ideal := p.BestLap * float64(window)
			if ideal > 0 {
				spread := (p.BestConsec - ideal) / ideal
				if spread >= TrigSpreadPct {
					add(TrigOneHotLap, p.Callsign, spread, p.NoRoast)
				}
			}
		}

		// 🏃 Won the sprint to gate one and finished at the back. Races only —
		// qualifying starts are staggered, so a holeshot means nothing there.
		// Finished heats only: mid-heat, Position is where they're RUNNING.
		if !opts.Live && !isTimeTrial && p.Holeshot > 0 && p.Position > 0 && p.FieldSize >= 3 &&
			p.Position == p.FieldSize && bestHoleshot(pilots) == p.Callsign {
			add(TrigHoleshotHero, p.Callsign, float64(p.FieldSize), p.NoRoast)
		}

		// 😴 Qualified in the bottom half, finished at the sharp end. Also
		// finish-only — "now it counts" needs the result to have counted.
		if !opts.Live && !isTimeTrial && p.QualPos > 0 && p.FieldQualSize > 3 && p.Position > 0 &&
			float64(p.QualPos) > float64(p.FieldQualSize)*TrigSandbagQuantile &&
			p.Position <= TrigSandbagFinish {
			add(TrigSandbagger, p.Callsign, float64(p.QualPos), p.NoRoast)
		}

		// 🎉 The run of their meeting. Same shape as the lab test but the
		// generous reading — a real improvement without being absurd.
		if p.PriorPB > 0 && p.BestConsec > 0 && p.BestConsec < p.PriorPB {
			gain := (p.PriorPB - p.BestConsec) / p.PriorPB
			if gain > 0.02 && gain < TrigPBSmashPct {
				add(TrigBreakthrough, p.Callsign, gain, p.NoRoast)
			}
		}
	}

	// Strongest first, then by key then pilot so the order is fully
	// deterministic for a regenerate.
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Score != found[j].Score {
			return found[i].Score > found[j].Score
		}
		if found[i].Key != found[j].Key {
			return found[i].Key < found[j].Key
		}
		return found[i].Pilot < found[j].Pilot
	})
	// One bit per pilot — two gags about the same pilot in one heat reads as
	// picking on them, which is exactly what the guardrail forbids.
	var out []Trigger
	seen := map[string]bool{}
	for _, t := range found {
		if seen[t.Pilot] {
			continue
		}
		seen[t.Pilot] = true
		out = append(out, t)
		if len(out) == TrigMaxPerHeat {
			break
		}
	}
	return out
}

// bestHoleshot returns the callsign with the quickest holeshot in the heat.
func bestHoleshot(pilots []PilotForm) string {
	best, name := 0.0, ""
	for _, p := range pilots {
		if p.Holeshot <= 0 {
			continue
		}
		if best == 0 || p.Holeshot < best {
			best, name = p.Holeshot, p.Callsign
		}
	}
	return name
}

// TriggerBlock renders detected triggers as the prompt fragment. Empty when
// there's nothing to riff on, so a clean heat's prompt is unchanged.
func TriggerBlock(ts []Trigger) string { return TriggerBlockOpt(ts, TriggerOpts{}) }

// TriggerBlockOpt is TriggerBlock with the timing of the run declared. A live
// (mid-heat) block is worded harder: the heat is still being raced, so the gag
// is an aside the call can drop, never the call itself.
func TriggerBlockOpt(ts []Trigger, opts TriggerOpts) string {
	if len(ts) == 0 {
		return ""
	}
	var b strings.Builder
	if opts.Live {
		b.WriteString("\nSTEWARDS' ROOM — genuine oddities in the data SO FAR this heat that you MAY riff on:\n")
	} else {
		b.WriteString("\nSTEWARDS' ROOM — genuine oddities in this heat's data that you MAY riff on:\n")
	}
	for _, t := range ts {
		b.WriteString("- " + t.Prompt + "\n")
	}
	if opts.Live {
		b.WriteString("The heat is still running, so the LIVE ACTION comes first: work in AT MOST one of these as a " +
			"single short aside, and drop them entirely if there's racing to call. They are real: the numbers above " +
			"back every one up. Keep it good-natured.\n")
	} else {
		b.WriteString("Work in AT MOST one of these, and only where it fits naturally — skip them entirely if the " +
			"racing is more interesting. They are real: the numbers above back every one up. Keep it good-natured.\n")
	}
	return b.String()
}

// MedianLap is the yardstick helper for PilotForm.TypicalLap — exported so the
// server and the agent compute "their own usual pace" identically.
func MedianLap(laps []float64) float64 {
	if len(laps) == 0 {
		return 0
	}
	s := append([]float64(nil), laps...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

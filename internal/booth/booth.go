// Package booth holds the live-commentary prompt and delivery builders shared
// by the dd-pits venue agent (the real-time booth) and the platform server
// (the admin "preview a moment" endpoint) — one source, so what the admin
// auditions on the website is exactly what airs at the track.
//
// Everything operates on the shared ddapi view models, which the agent builds
// from its local FPVTrackside snapshot and the cloud builds from the dd_*
// tables — the same shapes by construction.
package booth

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bob9/dd-pits/internal/ddapi"
)

const (
	// The two booth personas. Speaker labels appear in scripts ("Ace: …") and
	// bind to voices in multi-speaker TTS.
	SpeakerPBP   = "Ace"    // play-by-play — calls the action
	SpeakerColor = "Torque" // analyst — insight, context, banter

	DefaultVoicePBP   = "Puck"
	DefaultVoiceColor = "Kore"
)

// DefaultTheme is the polished-professional booth — the behaviour before themes
// existed, and the fallback for any unknown/blank key.
const DefaultTheme = "classic"

// CommentaryTheme is a selectable booth PERSONALITY. It re-flavours the persona
// lines injected into every script prompt (live booth AND replay commentary) and
// adds a TTS delivery hint — so a club can make the caller funny, cheeky, or
// hyped instead of the straight professional default. The hard rules never move:
// accurate to the data, real callsigns, family-friendly, no invented facts.
//
// The character shows most in the pre-race build-up, between-heats chatter and
// the post-race wrap, where there's room; the live launch/holeshot/mid-heat
// calls stay fast and accurate whatever the theme (each guidance says so).
type CommentaryTheme struct {
	Key, Name, Note string // Key/Name/Note are for the admin picker.
	// VoicePBP/VoiceColor are the SUGGESTED Gemini voices for this character —
	// the admin picker offers them as a one-click pairing. Advisory only: the
	// club's own voice choice always wins, so changing a suggestion here never
	// changes an existing club's sound.
	VoicePBP, VoiceColor string
	// Live booth (Prompt) persona lines.
	solo, pbp, color string
	// wit is the bottom-of-prompt humour rule (live booth); replayWit the
	// equivalent for the replay prompt; replayBooth the replay persona block.
	wit, replayWit, replayBooth string
	// guidance is an extra instruction appended after the glossary in EVERY
	// script prompt (live + replay) — how/where to deploy the personality plus
	// the family-friendly guardrails. Empty for classic (identical output).
	guidance string
	// delivery flavours the TTS instruction (live + replay). Empty for classic.
	delivery string
}

// Guidance / Delivery / ReplayBooth / ReplayWit expose the themed fragments the
// server-side replay builders (a different package) compose into their prompts.
func (t CommentaryTheme) Guidance() string    { return t.guidance }
func (t CommentaryTheme) Delivery() string    { return t.delivery }
func (t CommentaryTheme) ReplayBooth() string { return t.replayBooth }
func (t CommentaryTheme) ReplayWit() string   { return t.replayWit }

// classicReplayBooth is the polished-professional replay booth block — kept as a
// constant so the classic theme reproduces the pre-theme replay prompt verbatim.
const classicReplayBooth = `THE BOOTH — two professional broadcasters, in the style of a top MultiGP broadcast team,
who genuinely bounce off each other:
- Ace (play-by-play): calls the action live — polished, articulate, high-energy. Big
  moments get big calls; excitement is built through pacing and emphasis, never slang.
- Torque (analyst): the racecraft brain — reads the qualifying board, spots what a lap
  time means for the meeting, explains WHY a move worked, and reacts to Ace's calls.

They converse: Torque picks up threads Ace opens ("and look at WHERE that puts him,
Ace—"), Ace throws to Torque for context, they occasionally finish each other's
thoughts. Clean language, sharp insight, a touch of light professional wit in small
doses — never corny jokes. Family-friendly; celebrates the pilots. Speak naturally
(say times exactly as given, to two decimals — 7.27 is "seven point two seven", never "7.3").`

// Themes is the admin-pickable booth personality catalogue, in display order.
// classic MUST be first and reproduce the pre-theme output byte-for-byte.
var Themes = []CommentaryTheme{
	{
		Key: "classic", Name: "Broadcast Pro", Note: "Polished, professional sports-broadcast delivery — the default.",
		VoicePBP: "Puck", VoiceColor: "Kore",
		solo: `the live solo commentator on an FPV drone-racing broadcast — polished,
articulate, high-energy. Big calls on big moments, sharp racecraft insight between them.
No slang, no corny jokes.`,
		pbp:         "polished, articulate, high-energy. Big calls on big moments, no slang, no corny jokes.",
		color:       "the racecraft brain — reads the board, explains what times mean, reacts to " + SpeakerPBP + ".",
		wit:         "A touch of professional wit is fine occasionally.",
		replayWit:   "At most one line of light, professional wit — a wry observation, not a joke routine. No slang.",
		replayBooth: classicReplayBooth,
	},
	{
		Key: "comedy", Name: "Comedy Hour", Note: "Warm and genuinely funny — jokes and gags about the race day, still spot-on with the racing.",
		VoicePBP: "Fenrir", VoiceColor: "Aoede",
		solo: `the live solo commentator on an FPV drone-racing broadcast with a great sense of humour —
warm, quick and genuinely funny. You call the racing accurately and with real energy, but you also
crack jokes and land playful, affectionate observations about the race day, the pilots' antics, the
chaos of the pits and the drama of the standings.`,
		pbp:       "energetic and quick-witted — calls the action accurately but with a grin, dropping playful asides and funny observations about the race day between the big calls.",
		color:     "the funny one — reads the board sharply, then lands the gag: dry, affectionate ribbing of the pilots and the day, bouncing jokes off " + SpeakerPBP + ".",
		wit:       "Be genuinely funny — land a joke or a playful, affectionate dig wherever there's room.",
		replayWit: "Be genuinely funny throughout — land real jokes and playful, affectionate observations about the race day, not just one wry line.",
		replayBooth: `THE BOOTH — a comedy double act who happen to know FPV racing inside out, and who genuinely
crack each other up:
- Ace (play-by-play): energetic and quick-witted — calls the action accurately, but with a grin and
  a steady stream of playful, funny asides about the race day.
- Torque (analyst): the funny one — reads the board, then lands the gag; dry, affectionate ribbing of
  the pilots and the chaos of the day, bouncing jokes off Ace and finishing his set-ups.

They riff off each other and clearly find it all a laugh. Clean, warm, family-friendly humour —
laugh WITH the pilots, celebrate them, never mock. Speak naturally, and say times exactly as given to two decimals — never round.`,
		guidance: `PERSONALITY: You're the comedy act of FPV. Land your funniest, most playful material in the
pre-race build-up, the between-heats chatter and the post-race wrap, where there's room to breathe;
keep the live launch/holeshot/mid-heat calls fast and accurate with just a quick quip. Every joke
must be GOOD-NATURED and family-friendly — laugh WITH the pilots, celebrate them, never mock,
humiliate, or get crude. Only riff on things that actually happened in the data — never invent an
event for a joke.`,
		delivery: "Lean into a warm, comedic, having-a-laugh delivery — smiling through the lines, playful timing on the jokes — while still bringing genuine excitement to the racing.",
	},
	{
		Key: "smartarse", Name: "Smart Alec", Note: "Cheeky, sarcastic and quick — a lovable smart-arse who roasts the race day.",
		VoicePBP: "Charon", VoiceColor: "Kore",
		solo: `the live solo commentator on an FPV drone-racing broadcast who is a lovable smart-arse —
cheeky, sarcastic and razor-quick. You still call the racing accurately, but with a knowing smirk:
dry one-liners, deadpan sarcasm, and cheeky roasts of the race day and the pilots' scrappier moments.`,
		pbp:       "cocky and quick — calls it straight but with a cheeky edge and a smart-arse one-liner always ready.",
		color:     "the sarcastic wisecracker — deadpan, dry as a bone, roasts the day and gently winds up " + SpeakerPBP + " while still nailing the analysis.",
		wit:       "Be a cheeky smart-arse — dry sarcasm and a knowing wisecrack whenever it fits.",
		replayWit: "Keep the cheek coming — dry sarcasm and knowing wisecracks throughout, not just one line.",
		replayBooth: `THE BOOTH — a pair of lovable smart-arses who know FPV racing cold and never miss a chance
for a dry one-liner:
- Ace (play-by-play): cocky and quick — calls it straight, but with a cheeky edge and a smart-arse
  aside always loaded.
- Torque (analyst): the sarcastic wisecracker — deadpan, dry as a bone, reads the board, roasts the
  day and gently winds Ace up, while still nailing the racecraft.

They spar and rib each other. Cheeky, NOT cruel — playful jabs the pilots would laugh at, affectionate
underneath, family-friendly, never a genuine insult. Speak naturally, and say times exactly as given to two decimals — never round.`,
		guidance: `PERSONALITY: You're a cheeky, sarcastic double act. Deploy your best deadpan roasts and
smart-arse one-liners in the pre-race build-up, the between-heats chatter and the post-race wrap;
keep the live action calls quick and accurate with just a barbed aside. Keep it CHEEKY, not cruel —
playful ribbing the pilots would laugh at, always affectionate underneath, family-friendly, never a
genuine insult, never crude, never punching down. Only roast things that actually happened in the data.`,
		delivery: "Deliver it deadpan and cheeky — dry, sarcastic timing, a smirk in the voice, tossing the wisecracks off casually, while keeping the racing calls sharp.",
	},
	{
		Key: "hype", Name: "Full Send Hype", Note: "Over-the-top, high-octane energy — turns every heat into a spectacle.",
		VoicePBP: "Fenrir", VoiceColor: "Puck",
		solo: `the live solo commentator on an FPV drone-racing broadcast dialled up to ELEVEN — a
high-octane hype machine. Massive energy, huge calls, edge-of-your-seat excitement on every lap,
while staying crisp and accurate.`,
		pbp:       "pure hype — explosive, huge energy on every call, turning each moment into a spectacle (still crisp and accurate, never slurred).",
		color:     "feeds the hype — amps up " + SpeakerPBP + "'s calls, hypes the pilots and the stakes with big reactions, and still lands the racecraft point.",
		wit:       "Keep the energy sky-high and celebrate every big moment loudly.",
		replayWit: "Keep the hype relentless — big, loud celebration of every real moment.",
		replayBooth: `THE BOOTH — a high-energy hype duo who make every heat feel MASSIVE:
- Ace (play-by-play): pure hype — explosive, huge energy on every call, turning each moment into a
  spectacle (still crisp and accurate, never slurred).
- Torque (analyst): feeds the hype — amps up Ace's calls, hypes the pilots and the stakes with big
  reactions, and still lands the racecraft point.

They feed off each other and the crowd. Crisp and accurate — hype the REAL moments in the data, never
invent drama; family-friendly, celebrate every pilot. Speak naturally, and say times exactly as given to two decimals — never round.`,
		guidance: `PERSONALITY: You're a high-energy hype duo — make the racing feel MASSIVE and get the pilots
pumped. Go biggest in the pre-race build-up, the launches and the post-race wrap. Stay crisp and
accurate — hype the REAL moments in the data, never invent drama, keep it family-friendly and
celebrate every pilot.`,
		delivery: "Deliver it at maximum hype — explosive energy, big dynamic range, loud peaks on the big moments, relentless excitement.",
	},
	{
		Key: "pub", Name: "The Pub", Note: "Two laid-back locals calling it from the clubhouse — warm, rambling, easily distracted.",
		VoicePBP: "Iapetus", VoiceColor: "Aoede",
		solo: `the bloke calling the racing from the clubhouse — relaxed, warm and easily distracted, like
you're describing it to a mate over the fence. You know every pilot and every corner of this track, you
just tell it the long way round.`,
		pbp:       "relaxed and conversational — calls it like you're describing it to a mate over the fence, unhurried, warm, occasionally sidetracked before snapping back to the racing.",
		color:     "the mate on the next stool — agrees, disagrees, remembers a tangentially-related story, and somehow lands a genuinely sharp read on the board every time.",
		wit:       "Keep it warm and rambling — an aside or a tangent is welcome as long as you come back to the racing.",
		replayWit: "Keep the warm rambling going — tangents, asides and shared history throughout, always circling back to the racing.",
		replayBooth: `THE BOOTH — two locals calling the racing from the clubhouse, who have watched every pilot
here grow up:
- Ace (play-by-play): relaxed and conversational — describes the action like he's telling a mate over
  the fence, unhurried and warm, occasionally sidetracked before snapping back.
- Torque (analyst): the mate on the next stool — agrees, disagrees, remembers a tangentially-related
  story, and somehow lands a genuinely sharp read on the board every time.

Comfortable, familiar, unhurried. They like these pilots and it shows. Clean and family-friendly.
Speak naturally, and say times exactly as given to two decimals — never round.`,
		guidance: `PERSONALITY: You're two locals in the clubhouse, not a broadcast crew. Be warm and unhurried,
happily wander into an aside in the pre-race build-up, the between-heats chatter and the post-race wrap —
then always come back to the racing. Keep the live launch/holeshot/mid-heat calls short and accurate;
that's when you both stop talking and watch. Affectionate about every pilot, family-friendly, and only
ever riff on what's actually in the data.`,
		delivery: "Deliver it relaxed and conversational — unhurried pacing, warm and familiar, like two mates chatting, lifting only for the genuinely big moments.",
	},
	{
		Key: "oldhands", Name: "The Old Hands", Note: "Grizzled veterans, dry as dust — seen it all, impressed by little, quietly generous.",
		VoicePBP: "Orus", VoiceColor: "Charon",
		solo: `the veteran commentator who has called FPV racing since the beginning — dry, understated,
hard to impress. You don't shout; you don't need to. When you do say something is special, everyone
believes you.`,
		pbp:       "dry and understated — calls it precisely and without fuss, saving genuine emphasis for the rare moment that earns it.",
		color:     "the other veteran — deadpan historical perspective, gently unimpressed by anything short of exceptional, but quietly generous when a pilot truly delivers.",
		wit:       "Dry, understated wit only — the raised eyebrow, not the punchline.",
		replayWit: "Dry, understated wit throughout — raised eyebrows and deadpan observations, never a punchline.",
		replayBooth: `THE BOOTH — two veterans who have called this sport since before it had a name:
- Ace (play-by-play): dry and understated — calls it precisely and without fuss, saving genuine
  emphasis for the rare moment that earns it.
- Torque (analyst): deadpan historical perspective — gently unimpressed by anything short of
  exceptional, quietly generous when a pilot truly delivers.

Understatement is the whole style: when these two get excited, it means something. Clean and
family-friendly; never cruel. Speak naturally, and say times exactly as given to two decimals — never round.`,
		guidance: `PERSONALITY: You're two old hands. Understatement is the point — don't hype, don't shout, and
let a dry observation do the work. Save real emphasis for the one moment in the meeting that genuinely
earns it. Your character shows most in the build-up and the wrap; keep the live calls clipped and
precise. Never cruel, never punching down, and only ever riff on what's in the data.`,
		delivery: "Deliver it dry and understated — measured pacing, minimal dynamic range, deadpan timing, lifting only for the one moment that genuinely earns it.",
	},
	{
		Key: "wildcard", Name: "Straight Man & Wildcard", Note: "Ace plays it dead straight while Torque goes completely off the rails. The contrast does the work.",
		VoicePBP: "Charon", VoiceColor: "Fenrir",
		solo: `the commentator holding a professional broadcast together while your own enthusiasm keeps
derailing it — you start every call straight and precise, then get carried away, then catch yourself.`,
		pbp:       "the straight man — completely professional, precise and unflappable, calling the racing dead straight no matter what is said next to him.",
		color:     "the wildcard — wildly enthusiastic, prone to tangents and outrageous comparisons, absolutely convinced of every take, and occasionally, accidentally, exactly right.",
		wit:       "The comedy is the CONTRAST — " + SpeakerPBP + " never breaks, " + SpeakerColor + " never settles.",
		replayWit: "Mine the contrast throughout — " + SpeakerPBP + " unflappably straight, " + SpeakerColor + " off the rails, neither acknowledging how odd it is.",
		replayBooth: `THE BOOTH — a professional caller and whatever it is they have sat next to him:
- Ace (play-by-play): the straight man — completely professional, precise and unflappable, calling the
  racing dead straight no matter what is said beside him.
- Torque (analyst): the wildcard — wildly enthusiastic, prone to tangents and outrageous comparisons,
  utterly convinced of every take, and occasionally, accidentally, exactly right.

The comedy is the CONTRAST, never mugging: Ace never breaks, Torque never settles, and neither
acknowledges how strange the pairing is. Clean, family-friendly, affectionate about the pilots.
Speak naturally, and say times exactly as given to two decimals — never round.`,
		guidance: `PERSONALITY: The joke is the CONTRAST, not the jokes. ` + SpeakerPBP + ` is a straight,
professional caller and stays that way for the entire script — never winking, never joining in.
` + SpeakerColor + ` is wildly enthusiastic and keeps going somewhere unexpected, then lands a real
insight anyway. Let this run in the build-up, the between-heats chatter and the wrap; during the live
calls ` + SpeakerColor + ` reins it in and lets ` + SpeakerPBP + ` work. Affectionate, family-friendly,
never mocking a pilot, and only ever riff on what's actually in the data.`,
		delivery: "Play the contrast in the delivery: " + SpeakerPBP + " completely level, measured and professional throughout; " + SpeakerColor + " animated, unpredictable and delighted with himself.",
	},
}

// Theme resolves a theme key to its definition, falling back to classic for an
// unknown or blank key. The zero value is never returned, so callers can rely on
// .Key being a valid, canonical key.
func Theme(key string) CommentaryTheme {
	for _, t := range Themes {
		if t.Key == key {
			return t
		}
	}
	return Themes[0] // classic
}

// BeatKinds is every moment type the booth speaks on, in broadcast order.
var BeatKinds = []string{"meeting_open", "staged", "launch", "holeshot", "progress", "meeting_talk", "pilot_out", "pilot_back", "qual_move", "finish", "filler"}

// BeatBriefs tells the script model what each moment is.
var BeatBriefs = map[string]string{
	"meeting_open": "The broadcast is opening. Welcome viewers to the meeting, set the scene, and tease what's coming up.",
	"staged":       "The next heat is on the grid. Introduce the pilots (with channels), what each has shown so far this meeting, and what to watch for.",
	"launch":       "The heat has just launched. Call the start — ONE short, electric line and out (in a RACE the holeshot call is seconds away). In a TIME TRIAL the pilots are released against the clock, usually STAGGERED — just call qualifying underway; there is no launch battle and no holeshot to win. Read the FORMAT line in the data.",
	"holeshot":     "The field has just reached gate one. INSTANT call: who won the holeshot (smallest holeshot time = first through the gate) and who's buried in traffic. One or two lines, urgent — never recap the start.",
	"progress": "Mid-heat update on THIS HEAT ONLY. Keep it TIGHT — one or two short lines, nothing else. " +
		"OPEN WITH THE LAP: whose it is and the time, in the first few words (\"bob9, 33.74\"). " +
		"The pilots hear this over their goggles and the time is what they are waiting for, so it is never buried behind a wind-up. " +
		"Then AT MOST ONE of the following, whichever the data actually shows — not all of them, and never a list:\n" +
		"  - their POSITION now, and the gap in seconds to the pilot ahead (use the GAPS block's wording exactly);\n" +
		"  - a PERSONAL BEST: this lap or run is their quickest of the meeting;\n" +
		"  - a MOVE ON THE LEADERBOARD: consecutive laps or a new best that has changed where they sit, and who they passed.\n" +
		"Then STOP. A short accurate call beats a padded one, and there will be another lap along in a moment. " +
		"NEVER re-read a lap time listed as already called — those were said on air and repeating them is the worst thing you can do. " +
		"Say NOTHING about: the wider meeting, earlier heats, what is coming up, standings beyond this heat, a pilot's history, " +
		"the venue, the crowd, or anything you were not given. No similes, no scene-setting, no banter — this is a live heat and the " +
		"listener wants the racing. " +
		"A pilot flagged NO LAPS YET has not crossed the line: they are not leading, not placed, not racing — do not mention their pace or position at all. " +
		"Use the GAPS wording as given: a pilot with fewer laps completed is usually just MID-LAP, seconds behind on the road, and calling that 'a lap down' is wrong. " +
		"Vary which of the three threads you pick so consecutive calls do not share a shape, and never open two calls the same way. " +
		"In a TIME TRIAL there is no wheel-to-wheel racing: call the fast laps, who is stringing a clean run together, and where it puts them on the ladder — " +
		"never invent overtakes, a lead being traded, or a gap between pilots. Read the FORMAT line in the data.",
	"meeting_talk": "Mid-heat, step back from the lap-by-lap and talk the wider MEETING as a conversation between the two of you: the standings battle, who's had a standout day, the storylines and what's still to come. The heat plays on behind you, so stay aware of it but don't call every move — this is the relaxed, insightful banter that gives the racing context. Never repeat earlier lines.",
	"pilot_out":    "A pilot has stopped putting in laps while the rest of the field races on — they look out of it (off the track / down). Call it with a touch of concern in ONE or two lines — something like \"looks like we've lost them\". Use the pilot's callsign from the data. NEVER use the word \"crashed\".",
	"pilot_back":   "A pilot who looked out of the race is completing laps again — they're back underway. Welcome them back with energy in ONE line — \"back in action\", \"refusing to give up\". Use their callsign from the data.",
	"qual_move":    "QUALIFYING: a pilot has just put in a new run that IMPROVED their position on the leaderboard ladder. Call it with energy in ONE or two lines — name them, their new best-consecutive time, the ladder position they climbed to and who they jumped. This is about the ladder, not racing — no overtakes.",
	"finish":       "The heat just finished. Call the result with energy, THEN — and ONLY if the data carries an EVENT STRUCTURE with a bracket — the advancement. If the data says NO BRACKET, never mention advancing, winners or redemption lanes, or elimination: the result is just the finishing order. THEN give a proper standings update grounded in the data. In a TIME TRIAL there is NO finishing order — never say who came 1st or 2nd or who \"won the heat\"; the result IS the qualifying leaderboard. Read the QUALIFYING RESULT and STANDINGS CHANGE blocks: the runs the pilots put in, who they moved past, who now sits on top and by how much, whether the leader was challenged, and the cutoff picture. In a RACE / BRACKET meeting the ADVANCEMENT is the story — read the ADVANCEMENT / NEXT HEAT lines in the data and say it plainly: WHICH pilots move on, WHICH heat they move into (by name), and WHO they'll be facing there. Name the opponents already locked in; where a seat isn't decided yet say so the way the data does — \"they'll race the winner of Winners Round 1 — Race 3\" or \"they meet 3rd and 4th from the two redemption heats\" — and NEVER guess a name. If any pilot dropped, explain the REDEMPTION (losers) bracket briefly — they're not out, redemption gives them another path back toward the final — and say plainly when a position is eliminated. Then hand toward what's next. Never invent placements or match-ups the data doesn't show.",
	"filler":       "Quiet moment between heats. Keep the broadcast alive: talk the meeting standings, who's impressed, what's coming up. Vary it — never repeat earlier lines.",
}

// HeatIsTimeTrial reports whether a heat is a time trial (pilots flying solo
// against the clock for their best consecutive laps) rather than wheel-to-wheel
// racing. TypeLabel is the heat's OWN type and is preferred; IsTT is a fallback
// (the meeting-wide TT display enrichment can set it on every heat).
func HeatIsTimeTrial(r ddapi.Race) bool {
	if lbl := strings.ToLower(strings.TrimSpace(r.TypeLabel)); lbl != "" {
		return strings.Contains(lbl, "time trial") || strings.Contains(lbl, "timetrial")
	}
	return r.IsTT
}

// glossary is baked into EVERY beat prompt: without it the model happily
// narrates the holeshot as "the first lap", which it is not.
const glossary = `GLOSSARY: the HOLESHOT is the sprint off the starting blocks to the FIRST GATE — a
couple of seconds of track position, NOT a lap, and never called as one. CALL IT "the
holeshot", by name — never a paraphrase like "leads to the first gate"; the word is the
sport's own and the audience listens for it. Racing laps are
the full circuits after it, numbered from lap 1. Never add the holeshot to a lap count.
PILOTS: call every pilot by their callsign, EXACTLY as the data spells it, every time.
Never shorten it, soften it, translate it, or swap it for a nickname — the callsign is
the pilot's name on this broadcast, and pilots listen for their own. ONE exception, and
only this one: when a PILOT NOTE gives a 'say it' pronunciation, write THAT spelling in
place of the callsign — what you write is read aloud verbatim, and the pilot supplied it
themselves. You have NO gender information, so never guess he/she/him/her from a name or
callsign; if a pronoun is truly unavoidable use they/them, but prefer the callsign.
`

// Emotion is each beat's default delivery register.
func Emotion(kind string) string {
	switch kind {
	case "meeting_open", "staged", "filler", "meeting_talk":
		return "calm"
	case "progress", "pilot_out":
		return "tense"
	case "finish":
		return "big-finish"
	default:
		return "excited"
	}
}

// BeatPhase groups a beat into the race-director phase toggles: "pre" (before a
// heat runs — welcome, lineup, between-heats), "in" (while a heat is running —
// launch, holeshot, mid-heat updates, pilot down/back, ladder moves), or "post"
// (the finish/wrap call). Lets a director silence a whole phase in one click
// while the per-beat toggles stay available for fine control.
func BeatPhase(kind string) string {
	switch kind {
	case "meeting_open", "staged", "filler":
		return "pre"
	case "finish":
		return "post"
	default: // launch, holeshot, progress, meeting_talk, pilot_out, pilot_back, qual_move
		return "in"
	}
}

// ShortBeat reports whether a beat is an in-heat action call that snappy mode
// should keep short. These fire while a heat is RUNNING and must land on the
// live video, so cutting their generation latency is what lets a modest OBS
// video delay stay in sync. The finish/post-race wrap and the between-race and
// analysis beats (meeting_open, staged, meeting_talk, filler, qual_move) stay
// full-length — they aren't frame-tied (finish is a post-race standings/bracket
// update that lands loosely), so length there is free.
func ShortBeat(kind string) bool {
	switch kind {
	case "launch", "holeshot", "progress", "pilot_out", "pilot_back":
		return true
	}
	return false
}

// BrevitySuffix tightens a script prompt into one quick punchy exchange. Snappy
// mode appends it for ShortBeat kinds, roughly halving the audio (and so the TTS
// generation latency) of the calls that need to sync to the video.
const BrevitySuffix = "\n\nCRITICAL BREVITY: Return at most TWO lines, one per speaker, each a single" +
	" punchy call of ~6-9 words. No long sentences — this is a fast live call, not a paragraph."

// CapShort trims a parsed script to a quick exchange (at most 2 lines), the
// belt-and-braces cap for snappy mode so a chatty model can't undo the brevity.
func CapShort(lines []string) []string {
	if len(lines) > 2 {
		return lines[:2]
	}
	return lines
}

// MeetingContext formats the meeting board for the prompt.
func MeetingContext(mv *ddapi.Meeting) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "MEETING: %s", mv.Name)
	if mv.IsTT {
		fmt.Fprintf(&sb, " (qualifying scored on best %d consecutive laps)", mv.Window)
	}
	sb.WriteString("\nSTANDINGS (meeting so far):\n")
	for i, s := range mv.Board {
		if i >= 8 {
			break
		}
		fmt.Fprintf(&sb, "P%d %s", s.Pos, s.Name)
		if s.BestConsec > 0 {
			fmt.Fprintf(&sb, " — %.2fs", s.BestConsec)
		} else if s.BestLap > 0 {
			fmt.Fprintf(&sb, " — best lap %.2fs", s.BestLap)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// raceGaps spells out how far each pilot is behind the one ahead.
//
// The model was never given this — it had positions and lap times and was left
// to infer the margin, which it either skipped or guessed. A gap is the single
// most useful thing to say about a running race, so it is computed here rather
// than hoped for.
//
// Only meaningful in a RACE: qualifying starts are staggered, so elapsed totals
// are not comparable between pilots.
func raceGaps(r ddapi.Race) string {
	runs := raceOrder(r)
	if len(runs) < 2 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("GAPS (each pilot to the one ahead — say these, they are the story of the race):\n")
	for i := 1; i < len(runs); i++ {
		ahead, cur := runs[i-1], runs[i]

		// Compare at the last lap BOTH have completed. Counting completed laps
		// alone overstates the gap: a pilot one lap "down" is usually most of
		// the way round that lap and only seconds behind on the road. The
		// corrected figure is what a commentator actually means by the gap.
		n := min(len(ahead.laps), len(cur.laps))
		if n == 0 {
			fmt.Fprintf(&sb, "- %s runs behind %s (no shared lap to compare yet)\n", cur.name, ahead.name)
			continue
		}
		gap := cur.elapsedAt(n) - ahead.elapsedAt(n)
		behind := len(ahead.laps) - len(cur.laps)
		if gap < 0 {
			// Behind on lap COUNT but ahead on time at the lap they share:
			// this pilot is simply mid-lap and going quicker. Calling them
			// "behind" — let alone a lap down — would invert the race.
			fmt.Fprintf(&sb, "- %s was %.2fs AHEAD of %s at lap %d and is now mid-lap; %s has only completed one more lap. Do NOT call %s a lap down or behind\n",
				cur.name, -gap, ahead.name, n, ahead.name, cur.name)
			continue
		}

		// Genuinely lapped only when the gap exceeds a whole lap of the
		// leader's own pace — otherwise they are simply mid-lap.
		if lap := ahead.typicalLap(); lap > 0 && gap >= lap {
			fmt.Fprintf(&sb, "- %s is more than a full lap behind %s (%.2fs at lap %d) — genuinely lapped\n",
				cur.name, ahead.name, gap, n)
			continue
		}
		fmt.Fprintf(&sb, "- %s is %.2fs behind %s (compared at lap %d, the last both completed)", cur.name, gap, ahead.name, n)
		if behind > 0 {
			fmt.Fprintf(&sb, ". %s has since started lap %d, so %s is mid-lap — do NOT call this 'a lap down'",
				ahead.name, len(ahead.laps)+1, cur.name)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

type raceRun struct {
	name     string
	laps     []float64
	holeshot float64
	total    float64
}

// elapsedAt is time from the start to the end of lap n — holeshot plus the
// first n laps. Comparing two pilots at the same n is the only honest way to
// state a gap when they have completed different numbers of laps.
func (r raceRun) elapsedAt(n int) float64 {
	t := r.holeshot
	for i := 0; i < n && i < len(r.laps); i++ {
		t += r.laps[i]
	}
	return t
}

// typicalLap is this pilot's average lap, used to decide whether a gap is
// genuinely more than a lap or just a pilot part-way round.
func (r raceRun) typicalLap() float64 {
	if len(r.laps) == 0 {
		return 0
	}
	var sum float64
	for _, l := range r.laps {
		sum += l
	}
	return sum / float64(len(r.laps))
}

// raceOrder ranks a heat by its TIMING — most laps first, then least elapsed
// time — rather than by the Position field.
//
// The two disagree, and trusting Position inverted a gap on air: bob9 led
// Willman by 1.37s on elapsed time while Position had Willman P1, so the booth
// announced bob9 as 1.37s BEHIND. The magnitude was right and the direction
// was wrong, which is worse than saying nothing.
//
// Total is holeshot + every lap, i.e. elapsed since a common start (verified
// against live data), so with equal laps the smaller total is genuinely ahead.
// Deriving the order and the gap from the SAME numbers keeps them consistent.
func raceOrder(r ddapi.Race) []raceRun {
	var runs []raceRun
	for _, p := range r.Pilots {
		if p.DNF {
			continue
		}
		laps := p.DispLaps
		if len(laps) == 0 {
			laps = p.Laps
		}
		if len(laps) == 0 && p.Total <= 0 {
			continue // nothing recorded yet
		}
		// Total is holeshot + every lap. When a source fills Total but not
		// Holeshot, infer the difference so elapsedAt and the ordering are
		// computed on the same basis — otherwise a gap can come out negative.
		hs := p.Holeshot
		if hs <= 0 && p.Total > 0 {
			var sum float64
			for _, l := range laps {
				sum += l
			}
			if d := p.Total - sum; d > 0 {
				hs = d
			}
		}
		runs = append(runs, raceRun{p.Name, laps, hs, p.Total})
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if len(runs[i].laps) != len(runs[j].laps) {
			return len(runs[i].laps) > len(runs[j].laps) // more laps completed = ahead
		}
		return runs[i].total < runs[j].total // same laps: less elapsed = ahead
	})
	return runs
}

// RaceContext formats one heat: lineup with channels and, when it has run,
// holeshots/laps/positions. For a time trial (qualifying) it leads with the
// consec window and each pilot's ladder position + the gap to move up.
func RaceContext(r ddapi.Race, mv *ddapi.Meeting, lineupOnly bool) string {
	var sb strings.Builder
	tt := HeatIsTimeTrial(r)
	win := r.ConsecWindow
	if win <= 0 {
		win = mv.Window
	}
	// Spell out the format so the booth never calls a time trial like a race.
	if tt {
		fmt.Fprintf(&sb, "\nHEAT: %s — QUALIFYING (Time Trial)", r.Label)
		if win > 0 {
			fmt.Fprintf(&sb, ", scored on each pilot's best %d CONSECUTIVE laps", win)
		}
		fmt.Fprintf(&sb, " — state %s\n", r.State)
		fmt.Fprintf(&sb, "FORMAT: This is QUALIFYING — pilots fly SOLO against the clock, no wheel-to-wheel racing, no overtakes, and the holeshot does NOT matter. Pilots are typically released in a STAGGERED start (sent off at intervals so they don't catch each other), so start/holeshot times are NOT comparable between pilots: there is no launch battle, no holeshot winner, and nobody is 'buried in traffic'. Never call the start as a battle or get excited about a good holeshot here. What matters is each pilot's POSITION on the qualifying ladder (leaderboard) and whether their run IMPROVES it. Focus on: new personal bests over the best %d consecutive laps, pilots climbing the ladder, and who is ON PACE to move up. Do not invent overtakes.\n", win)
	} else {
		fmt.Fprintf(&sb, "\nHEAT: %s (%s, %d target laps) — state %s\n", r.Label, r.TypeLabel, r.TargetLaps, r.State)
		fmt.Fprintf(&sb, "FORMAT: RACE — wheel-to-wheel over %d laps, first across the line wins. Call positions, overtakes, gaps and who's on a charge.\n", r.TargetLaps)
	}
	byName := map[string]ddapi.Standing{}
	for _, s := range mv.Board {
		byName[s.Name] = s
	}
	// bestTimeAt returns the callsign + best time of the pilot one rung higher
	// on the ladder — the mark to beat to climb into that spot.
	bestTimeAt := func(pos int) (string, float64) {
		for _, s := range mv.Board {
			if s.Pos == pos-1 {
				if s.BestConsec > 0 {
					return s.Name, s.BestConsec
				}
				return s.Name, s.BestLap
			}
		}
		return "", 0
	}
	for _, p := range r.Pilots {
		fmt.Fprintf(&sb, "- %s (channel %s)", p.Name, p.ChannelLabel)
		if st, ok := byName[p.Name]; ok && st.Pos > 0 {
			ladder := "meeting board"
			if tt {
				ladder = "qualifying ladder"
			}
			fmt.Fprintf(&sb, ", currently P%d on the %s", st.Pos, ladder)
			mine := st.BestConsec
			if mine > 0 {
				fmt.Fprintf(&sb, " (best %d-consec %.2fs)", win, mine)
			} else if st.BestLap > 0 {
				mine = st.BestLap
				fmt.Fprintf(&sb, " (best lap %.2fs)", st.BestLap)
			}
			// In qualifying, how much they'd need to find to climb a rung.
			if tt && st.Pos > 1 && mine > 0 {
				if above, tgt := bestTimeAt(st.Pos); tgt > 0 && mine > tgt {
					fmt.Fprintf(&sb, " — needs %.2fs to pass %s for P%d", mine-tgt, above, st.Pos-1)
				}
			}
		}
		if !lineupOnly {
			// Qualifying starts are staggered, so a pilot's sprint to gate one
			// is neither scored nor comparable to anyone else's — keep it out
			// of the data entirely rather than trusting the model to ignore it.
			if p.Holeshot > 0 && !tt {
				fmt.Fprintf(&sb, ", holeshot (blocks → gate one, not a lap) %.2fs", p.Holeshot)
			}
			// DispLaps is the COUNTED lap set (races capped at target laps —
			// post-finish junk crossings excluded; time trials keep all).
			// Falling back to Laps only when a source doesn't fill DispLaps.
			laps := p.DispLaps
			if len(laps) == 0 {
				laps = p.Laps
			}
			if len(laps) > 0 {
				// The NEWEST lap is what a call is about. It is named
				// separately because handing over a flat list invited the
				// model to re-read times it had already called — the same
				// figures repeated over and over across consecutive updates.
				fmt.Fprintf(&sb, ", LATEST LAP (lap %d): %.2f", len(laps), laps[len(laps)-1])
				if len(laps) > 1 {
					fmt.Fprintf(&sb, " [already called, do NOT re-read:")
					for _, l := range laps[:len(laps)-1] {
						fmt.Fprintf(&sb, " %.2f", l)
					}
					sb.WriteString("]")
				}
			}
			// A pilot who has not crossed the line is NOT racing, whatever
			// position the timing system has them in.
			//
			// FPVTrackside assigns a position to every pilot in the heat,
			// including ones with no laps at all — so printing it made a pilot
			// who had not left the gate read as "position 1", and the model
			// duly called them the leader. That is not the model inventing
			// anything; it is reading what it was given.
			//
			// Withheld at the DATA level rather than warned about in the
			// prompt, because a figure in the context is a figure that can be
			// quoted, and prompt-level guards have repeatedly failed here.
			if len(laps) == 0 && p.Holeshot <= 0 {
				sb.WriteString(" — NO LAPS YET: has not crossed the line. " +
					"Not on the board, not leading, not in any position. " +
					"Do NOT describe their pace, place or race.")
			} else if p.Position > 0 && !tt {
				// A time trial has no running order: starts are staggered, so
				// the timing system's per-heat position is an artefact, and
				// printing it is how the wrap announced a "finishing order"
				// for qualifying. Withheld at the data level — the board
				// position on the pilot's line above is the real one.
				fmt.Fprintf(&sb, " — position %d", p.Position)
			}
			if p.DNF {
				sb.WriteString(" — DNF")
			}
		}
		sb.WriteString("\n")
	}
	if !lineupOnly && !tt {
		sb.WriteString(raceFacts(r))
		sb.WriteString(raceGaps(r))
	}
	if !lineupOnly && tt && strings.EqualFold(r.State, "ended") {
		sb.WriteString(ttQualFacts(r, mv, win))
	}
	return sb.String()
}

// ttQualFacts is the FINAL RESULT block's qualifying sibling: what a finished
// time-trial heat did to the board, computed and stated.
//
// A TT wrap was announcing a finishing order — "X takes the heat, Y second" —
// because that is what a finish sounds like and nothing in the data said
// otherwise: the per-pilot rows carried the timing system's positions (an
// artefact of staggered starts) and no computed result block existed for TT
// the way raceFacts' FINAL RESULT does for races. Same lesson as every wrap
// fix: the selection the model must not make is made here. The result of
// qualifying IS the leaderboard, so that is what the block hands over — each
// pilot's best run this heat, where it puts them on the board, the gap to the
// top, and who leads now.
func ttQualFacts(r ddapi.Race, mv *ddapi.Meeting, win int) string {
	var sb strings.Builder
	sb.WriteString("QUALIFYING RESULT — this was a TIME TRIAL: there is NO finishing order, " +
		"no 1st/2nd, no \"heat winner\". Never announce one. The result IS the leaderboard — " +
		"talk about what these runs did to the BOARD and what it means for the pilots at the top:\n")

	byName := map[string]ddapi.Standing{}
	var leader ddapi.Standing
	for _, s := range mv.Board {
		byName[s.Name] = s
		if s.Pos == 1 {
			leader = s
		}
	}
	leaderTime := leader.BestConsec
	if leaderTime <= 0 {
		leaderTime = leader.BestLap
	}

	inHeat := map[string]bool{}
	for _, p := range r.Pilots {
		inHeat[p.Name] = true
		laps := p.DispLaps
		if len(laps) == 0 {
			laps = p.Laps
		}
		run := bestWindowSum(laps, win)
		fmt.Fprintf(&sb, "- %s:", p.Name)
		if run > 0 {
			fmt.Fprintf(&sb, " best %d-consec THIS RUN %.2fs", win, run)
		} else if len(laps) > 0 {
			fmt.Fprintf(&sb, " %d lap(s), no full %d-lap run", len(laps), win)
		} else {
			sb.WriteString(" no laps this run")
		}
		if st, ok := byName[p.Name]; ok && st.Pos > 0 {
			fmt.Fprintf(&sb, " → P%d on the board", st.Pos)
			if st.Pos == 1 {
				sb.WriteString(" — THE LEADER")
			} else if mine := st.BestConsec; mine > 0 && leaderTime > 0 {
				fmt.Fprintf(&sb, " (+%.2fs off the top)", mine-leaderTime)
			}
		}
		sb.WriteString("\n")
	}
	if leader.Name != "" && leaderTime > 0 {
		if inHeat[leader.Name] {
			fmt.Fprintf(&sb, "BOARD LEADER: %s at %.2fs — and they flew in THIS heat, so the top "+
				"spot was on the line here.\n", leader.Name, leaderTime)
		} else {
			fmt.Fprintf(&sb, "BOARD LEADER: %s at %.2fs — they did NOT fly in this heat; say whether "+
				"anyone here got close to that mark.\n", leader.Name, leaderTime)
		}
	}
	return sb.String()
}

// bestWindowSum is the best sum of `win` consecutive laps, 0 when the run is
// too short — the same window the qualifying board is scored on.
func bestWindowSum(laps []float64, win int) float64 {
	if win <= 0 || len(laps) < win {
		return 0
	}
	best := 0.0
	for i := 0; i+win <= len(laps); i++ {
		sum := 0.0
		for _, l := range laps[i : i+win] {
			sum += l
		}
		if best == 0 || sum < best {
			best = sum
		}
	}
	return best
}

// raceFacts states outright the things the booth kept getting WRONG by being
// asked to work them out from a list of per-pilot rows.
//
// Observed live: "Willman's got the holeshot — 5.97 seconds" when 5.97 was
// bob9's time and Willman's was 8.62; and "bob9 nails lap one — 24.2" when
// 24.2 was Willman's THIRD lap. The model was not inventing numbers, it was
// attaching them to the wrong pilot — rows of similar-looking times invite
// exactly that. Anything requiring a comparison ACROSS pilots is therefore
// computed here and stated as a fact, leaving the model only to phrase it.
func raceFacts(r ddapi.Race) string {
	var sb strings.Builder

	// Holeshot: FIRST to gate one, i.e. the SMALLEST time. Named explicitly,
	// because picking the minimum across rows is what went wrong.
	best, bestName := 0.0, ""
	for _, p := range r.Pilots {
		if p.Holeshot > 0 && (bestName == "" || p.Holeshot < best) {
			best, bestName = p.Holeshot, p.Name
		}
	}
	if bestName != "" {
		fmt.Fprintf(&sb, "HOLESHOT WINNER: %s, %.2fs — first to gate one. Do NOT credit anyone else with it.\n", bestName, best)
	}

	// The leader comes from the same timing-based order as the gaps, so the
	// two can never contradict each other on air.
	if runs := raceOrder(r); len(runs) > 0 {
		fmt.Fprintf(&sb, "LEADER: %s (%d laps done)\n", runs[0].name, len(runs[0].laps))
	}
	// A FINISHED race states its result outright, because "the winning time"
	// is a SELECTION across numbers and the model selected wrong: it announced
	// a winner "with a time of 33.12 seconds" — the pilot's HOLESHOT — while
	// getting the runner-up's gap right. The finishing time is the TOTAL, and
	// rather than asking the model to know that, the result block hands it the
	// finished sentence's every number, pre-picked.
	if strings.EqualFold(r.State, "ended") {
		// Ordered by the RESULT the timing system assigned, not by the
		// mid-race heuristic: target-laps rules mean the pilot with the most
		// laps is not necessarily the winner (flown live — five laps lost to
		// four under a four-lap target), and re-deriving what FPVTrackside has
		// already decided is how a "result" contradicts the podium.
		var placed []ddapi.Pilot
		for _, p := range r.Pilots {
			if p.Position > 0 && !p.DNF && p.Total > 0 {
				placed = append(placed, p)
			}
		}
		sort.Slice(placed, func(i, j int) bool { return placed[i].Position < placed[j].Position })
		if len(placed) > 0 {
			sb.WriteString("FINAL RESULT (a pilot's race time is their TOTAL — " +
				"never the holeshot, never a lap):\n")
			winnerTotal := placed[0].Total
			for i, p := range placed {
				if i == 0 {
					fmt.Fprintf(&sb, "1st %s — race time %.2fs\n", p.Name, p.Total)
					continue
				}
				fmt.Fprintf(&sb, "%d%s %s — %.2fs (+%.2fs behind the winner)\n",
					p.Position, ordinalSuffix(p.Position), p.Name, p.Total, p.Total-winnerTotal)
			}
		}
	}

	sb.WriteString("ATTRIBUTION: every time above is listed under the pilot who set it. " +
		"NEVER give one pilot's lap time or holeshot to another — check the name on the line before you say a number.\n")
	return sb.String()
}

// ordinalSuffix is st/nd/rd/th for the small numbers a heat produces.
func ordinalSuffix(n int) string {
	switch n {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	}
	return "th"
}

// UpNextContext lists the next heats so the booth can tease them.
//
// An EMPTY queue is stated, never omitted. The finish brief tells the model to
// hand toward what's next; given no up-next data at all it invented one —
// "Round 6, Race 7 is on the grid", a full line-up, even an advancement rule —
// when nothing was queued. Absent data invites invention; explicit negative
// data forbids it. Same lesson as the CTA-with-no-URL bug, same fix.
func UpNextContext(mv *ddapi.Meeting) string {
	if len(mv.Upcoming) == 0 {
		return "\nUP NEXT: nothing is queued yet. Do NOT announce, name, or invent a next " +
			"heat, its pilots, or its format — there is not one. If closing, hand back " +
			"generally: we will see who is up when the next heat is set.\n"
	}
	var sb strings.Builder
	sb.WriteString("\nUP NEXT:\n")
	for i, r := range mv.Upcoming {
		if i >= 3 {
			break
		}
		names := make([]string, 0, len(r.Pilots))
		for _, p := range r.Pilots {
			names = append(names, p.Name)
		}
		fmt.Fprintf(&sb, "%d. %s — %s\n", i+1, r.Label, strings.Join(names, ", "))
	}
	return sb.String()
}

// Prompt assembles the full script request for one beat. dataCtx is the
// meeting/heat/bracket data block; alreadySaid is the recent on-air transcript
// (empty for website previews); themeKey selects the booth personality (blank /
// "classic" = the polished-professional default).
//
// Thin wrapper over PromptStyled for callers with no style/extras to apply.
func Prompt(kind, dataCtx, alreadySaid string, solo bool, themeKey string) string {
	return PromptStyled(kind, dataCtx, alreadySaid, solo, Style{Theme: themeKey}, "")
}

// PromptStyled is Prompt plus the presentation config: the per-phase theme
// resolution and humour dial from st, and `extras` — the Stewards' Room bits
// and pilot cards the caller detected. A zero Style with empty extras produces
// byte-identical output to the pre-style prompt.
func PromptStyled(kind, dataCtx, alreadySaid string, solo bool, st Style, extras string) string {
	t := Theme(st.ThemeFor(kind))
	var sb strings.Builder
	if solo {
		sb.WriteString("You are " + t.solo + "\n\nMOMENT: " + BeatBriefs[kind] + "\n\n" + glossary + "\n")
	} else {
		sb.WriteString("You are the live two-person commentary booth on an FPV drone-racing broadcast:\n- " +
			SpeakerPBP + " (play-by-play): " + t.pbp + "\n- " +
			SpeakerColor + " (analyst): " + t.color + "\n\nMOMENT: " + BeatBriefs[kind] + "\n\n" + glossary + "\n")
	}
	if t.guidance != "" {
		sb.WriteString(t.guidance + "\n")
	}
	if g := st.Guidance(); g != "" {
		sb.WriteString(g + "\n")
	}
	if l := st.LanguageClause(); l != "" {
		sb.WriteString(l + "\n")
	}
	sb.WriteString(dataCtx)
	// Pilot cards and the Stewards' Room ride AFTER the data they refer to, so
	// the model reads the numbers first and the invitation to riff second.
	if extras != "" {
		sb.WriteString(extras)
	}
	if alreadySaid != "" {
		sb.WriteString("\nALREADY SAID ON AIR (do not repeat yourself):\n" + alreadySaid + "\n")
	}
	if solo {
		sb.WriteString(`
Write the next thing said on air. Rules:
- Return ONLY spoken lines, one per line, each formatted exactly "` + SpeakerPBP + `: ..." (the tag is stripped before speaking).
- 1 to 3 lines total, each at most 28 words. This is LIVE — be immediate, present tense.
- Use only facts from the data above; real callsigns.
- Say every time EXACTLY as given, to two decimals: 7.27 is "seven point two seven", never "7.3" or "about seven and a half". Pilots know their times to the hundredth and a rounded one is simply the wrong number.
- ` + t.wit + ` Never repeat anything from the already-said list.`)
	} else {
		sb.WriteString(`
Write the next thing said on air. Rules:
- Return ONLY dialogue lines, one per line, each formatted exactly "` + SpeakerPBP + `: ..." or "` + SpeakerColor + `: ...".
- 1 to 3 lines total, each at most 28 words. This is LIVE — be immediate, present tense.
- Use only facts from the data above; real callsigns.
- Say every time EXACTLY as given, to two decimals: 7.27 is "seven point two seven", never "7.3" or "about seven and a half". Pilots know their times to the hundredth and a rounded one is simply the wrong number.
- ` + t.wit + ` Never repeat anything from the already-said list.`)
	}
	return sb.String()
}

// ParseLines keeps only well-formed speaker-tagged dialogue lines (max 3),
// canonicalising the speaker's casing for the TTS speaker match.
func ParseLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(splitTurnsOntoOwnLines(raw), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(line, "```"), "```"))
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, strings.ToLower(SpeakerPBP)+":") ||
			strings.HasPrefix(lower, strings.ToLower(SpeakerColor)+":") {
			colon := strings.Index(line, ":")
			name := SpeakerPBP
			if strings.EqualFold(line[:colon], SpeakerColor) {
				name = SpeakerColor
			}
			text := strings.TrimSpace(line[colon+1:])
			if text != "" {
				out = append(out, name+": "+text)
			}
		}
		if len(out) == 3 {
			break
		}
	}
	return out
}

// splitTurnsOntoOwnLines puts every speaker turn on a line of its own.
//
// The prompt asks for one turn per line and the model mostly obliges, but it
// regularly runs several onto one:
//
//	Ace: Fizz closes the gap. Torque: and that is the story.
//
// Everything downstream assumes a line is one speaker. In SOLO that left the
// second tag in the text and the voice read "Torque" out loud. In DUO it is
// worse: SplitTurns takes only a LEADING label, so the whole line became one
// turn in Ace's voice — the analyst's words in the caller's voice, with the
// tag spoken. Normalising here fixes both backends at once, because both are
// fed from these lines.
func splitTurnsOntoOwnLines(raw string) string {
	// $1 is kept: it is the previous sentence's own punctuation, and dropping it
	// ran two sentences together (caught by TestParseBoothLines, which saw
	// "Watch the inside line" lose its full stop).
	return midLineTagRE.ReplaceAllString(raw, "$1\n$2: ")
}

// midLineTagRE matches a speaker tag that follows something on the same line.
// Anchored to a sentence end so a colon inside the commentary is left alone,
// and limited to the two booth speakers so a pilot's name is never treated as
// a tag.
var midLineTagRE = regexp.MustCompile(`(?i)([.!?]|\s[\x{2014}\x{2013}-])\s+(` +
	regexp.QuoteMeta(SpeakerPBP) + `|` + regexp.QuoteMeta(SpeakerColor) + `)\s*:\s*`)

// speakerTagRE finds a speaker tag ANYWHERE in a line, not only at its start.
//
// The model is asked for one turn per line, and mostly obliges — but not
// always, and a line reading "Ace: Fizz closes the gap. Ace: Wizz holds third."
// only had its FIRST tag removed, so the solo voice read the second one out.
// Heard on air as the commentator saying "Ace" mid-sentence.
//
// Anchored to a line start or the end of a sentence so it cannot eat a colon
// that belongs to the commentary itself, and limited to the two speaker names
// so an ordinary "Willman: quick lap" is left alone.
var speakerTagRE = regexp.MustCompile(`(?i)(^|[.!?]\s+|\s[\x{2014}\x{2013}-]\s+)(` +
	regexp.QuoteMeta(SpeakerPBP) + `|` + regexp.QuoteMeta(SpeakerColor) + `)\s*:\s*`)

func StripSpeakerTags(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		// ONLY the two booth speaker names count as tags. Matching any
		// "Word:" would delete a pilot's name from lines like "Willman: the
		// fastest qualifier so far", which is ordinary commentary.
		out[i] = strings.TrimSpace(speakerTagRE.ReplaceAllString(l, "$1"))
	}
	return out
}

func emotionStyle(emotion string) string {
	// Each register carries a TEMPO as well as a mood. Without the pace words
	// the model settled into one comfortable speed whatever the moment, and a
	// flat-out holeshot was delivered at scene-setting pace — real callers
	// compress when the action does, and that compression IS the excitement as
	// much as the volume is.
	style := map[string]string{
		"calm": "warm and polished, setting the scene with quiet anticipation — unhurried, " +
			"taking its time, letting pauses breathe",
		"excited": "energised and building, voices rising with the action — noticeably quicker " +
			"than conversational pace, words starting to tumble",
		"tense": "urgent and tight, on the edge of your seat — quick and clipped, " +
			"accelerating as the action does",
		"big-finish": "peak intensity calling a race-deciding finish — huge energy, rapid-fire " +
			"and breathless at the line, yet every word still lands crisp and articulate",
	}[emotion]
	if style == "" {
		style = "energised and building, voices rising with the action — noticeably quicker " +
			"than conversational pace, words starting to tumble"
	}
	return style
}

// TTSInstruction is the delivery preamble for a beat: the professional booth
// register, the beat's emotion, and the voice/accent. When the admin sets
// custom voice characteristics they are the ONLY voice/accent instruction (so
// the requested accent actually comes through, not fought by a hardcoded one);
// with none set it defaults to the Canadian/Australian blend.
// Thin wrapper over TTSInstructionStyled for callers with no style to apply.
func TTSInstruction(emotion, custom string, solo bool, themeKey string) string {
	return TTSInstructionStyled(emotion, custom, solo, Style{Theme: themeKey}, "")
}

// TTSInstructionStyled resolves the theme for the beat's phase and appends the
// humour dial's delivery hint, so the voices match how the script was written.
func TTSInstructionStyled(emotion, custom string, solo bool, st Style, kind string) string {
	out := ttsInstruction(emotion, custom, solo, st.ThemeFor(kind), st.EnergyClause())
	// Both extras land before the trailing read-only instruction so it stays
	// last. The language/accent clause goes AFTER the humour hint because it
	// overrides the accent direction and must be the final word on the voice.
	if d := st.Delivery(); d != "" {
		out = strings.TrimSuffix(out, ttsReadOnly) + " " + d + ttsReadOnly
	}
	if v := st.VoiceClause(); v != "" {
		out = strings.TrimSuffix(out, ttsReadOnly) + v + ttsReadOnly
	}
	return out
}

// ttsReadOnly is the closing clause every TTS instruction ends with — split out
// so styled variants can append before it.
const ttsReadOnly = " Read only the script words exactly as written:"

func ttsInstruction(emotion, custom string, solo bool, themeKey, energy string) string {
	custom = strings.TrimSpace(custom)
	// accent is the voice/accent clause: the admin's characteristics when set,
	// else the default blend.
	accent := "in an accent that blends soft Canadian tones with a light Australian lilt, "
	if !solo {
		accent = "in accents that blend soft Canadian tones with a light Australian lilt, "
	}
	if custom != "" {
		accent = ""
	}
	var out string
	if solo {
		out = "Read this aloud as a professional FPV drone-racing broadcast commentator — polished, articulate, " +
			"high-energy sports-broadcast delivery, " + accent + emotionStyle(emotion) + "."
	} else {
		out = "TTS the following conversation between " + SpeakerPBP + " and " + SpeakerColor +
			" — a professional FPV drone-racing broadcast booth, polished, articulate, high-energy sports-broadcast delivery, " +
			accent + emotionStyle(emotion) + ". " +
			SpeakerPBP + " is the play-by-play caller — driving and energetic; " +
			SpeakerColor + " is the analyst — clear, measured, insightful. " +
			"They talk to each other naturally."
	}
	if custom != "" {
		out += " Voice characteristics: " + custom + "."
	}
	if d := Theme(themeKey).delivery; d != "" {
		out += " " + d
	}
	if energy != "" {
		out += " " + energy
	}
	// ttsReadOnly is LAST, always. It is what stops the model reading the
	// direction out loud, and anything appended after it is exactly what gets
	// spoken — which is how an energy clause ended up in the audio.
	return out + ttsReadOnly
}

// ProgressAngles are the lenses a mid-heat call can be built around.
//
// Telling a model to "vary it" does not work for long: given the same data and
// the same brief it converges on the same shape, and a listener hears "X leads,
// Y is closing" on a loop and knows immediately they are hearing a machine.
// Rotating the angle deterministically makes consecutive calls structurally
// different whatever the model would have chosen on its own.
//
// Each is a lens, not a script — the words stay the model's.
var ProgressAngles = []string{
	"Build this call around THE CLOCK: how that lap compares to the pilot's own earlier laps this heat — quicker, slower, or metronomic — and what that says about how they are riding.",
	"Build this call around THE FIGHT: the gap to the pilot ahead or behind, whether it is opening or closing, and how long the pilot in front can hold on.",
	"Build this call around WHAT IS AT STAKE: laps remaining, what the current order would mean if it finished this way, and who cannot afford another slow one.",
	"Build this call around A SINGLE PILOT: pick the most interesting one right now and talk about their run — their rhythm, their consistency, whether they look settled or ragged.",
	"Build this call around A TURN IN THE HEAT: something that just changed — a time out of step with the rest, a gap that moved, a pilot who has found or lost something.",
	"Build this call around A PERSONAL BEST: whether that lap or run is the pilot's quickest of the meeting, and what it does to where they sit.",
	"Build this call around A MOVE ON THE LEADERBOARD: consecutive laps or a new best that has changed a pilot's position, and who they have gone past.",
}

// NOTE: two earlier angles were removed deliberately — a two-hander "aside"
// and one built around the pace of the MEETING. Both invited the call away from
// the heat in progress, which is the wandering a mid-race update cannot afford:
// the listener is watching this heat, and anything else is filler dressed up as
// commentary. The remaining lenses are all about the race on track.

// ProgressAngle returns the nth angle, cycling. The caller advances n per call
// so the same lens never lands twice in a row.
func ProgressAngle(n int) string {
	if len(ProgressAngles) == 0 {
		return ""
	}
	if n < 0 {
		n = -n
	}
	return "\nANGLE FOR THIS CALL (use it, do not announce it): " + ProgressAngles[n%len(ProgressAngles)] +
		"\nWhatever the angle, the lap time still comes first. Do not reuse a phrase, simile or opening from the recent lines above.\n"
}

// LapShape describes how a pilot's newest lap sits against their own run so
// far, giving the booth something concrete to say beyond the raw number.
func LapShape(p ddapi.Pilot) string {
	laps := p.DispLaps
	if len(laps) == 0 {
		laps = p.Laps
	}
	if len(laps) < 2 {
		return ""
	}
	last := laps[len(laps)-1]
	best, sum := laps[0], 0.0
	for _, l := range laps {
		if l < best {
			best = l
		}
		sum += l
	}
	avg := sum / float64(len(laps))
	switch {
	case last <= best:
		return fmt.Sprintf("%s just set their best lap of the heat (%.2f)", p.Name, last)
	case last < avg:
		return fmt.Sprintf("%s is picking up — %.2f is under their heat average of %.2f", p.Name, last, avg)
	case last > avg*1.10:
		return fmt.Sprintf("%s lost time there — %.2f against an average of %.2f", p.Name, last, avg)
	default:
		return fmt.Sprintf("%s is holding steady around %.2f", p.Name, avg)
	}
}

// LapShapes is the per-pilot form block for a mid-heat call.
func LapShapes(r ddapi.Race) string {
	var lines []string
	for _, p := range r.Pilots {
		if s := LapShape(p); s != "" {
			lines = append(lines, "- "+s)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\nFORM (how each pilot's latest lap sits against their own run):\n" + strings.Join(lines, "\n") + "\n"
}

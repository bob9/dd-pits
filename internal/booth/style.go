package booth

// Booth STYLE: the knobs that decide how the commentary sounds, as opposed to
// what it's about. Three of them, deliberately independent:
//
//   - THEME — the personality (Broadcast Pro, Smart Alec, The Pub …). See
//     Themes in booth.go.
//   - PER-PHASE THEME — an optional different personality for the pre-race
//     build-up, the live in-race calls and the post-race wrap. A club that
//     wants a cheeky pre-show and a straight race call can have both; every
//     theme's own guidance already tells it to keep live calls tight, but a
//     phase override makes that a setting rather than a hope.
//   - HUMOUR — how hard to lean on whichever personality is selected. Separate
//     from the theme so you can run Smart Alec at half volume for a champs
//     round without swapping character.
//
// A zero Style is the pre-existing behaviour exactly: classic theme, no humour
// clause, no per-phase overrides.

import "strings"

// Humour levels, weakest to strongest. Blank resolves to HumourDefault, which
// is "as the theme intends" — no extra clause, so a zero Style reproduces the
// pre-humour-dial prompt byte-for-byte.
const (
	HumourDefault  = ""
	HumourStraight = "straight"
	HumourLight    = "light"
	HumourCheeky   = "cheeky"
	HumourSendIt   = "sendit"
)

// HumourOptions is the admin picker list, in display order.
var HumourOptions = []struct{ Key, Name, Note string }{
	{HumourDefault, "As the personality intends", "No extra dial — the selected personality's own level."},
	{HumourStraight, "Straight", "Play it dead straight. No jokes, even on a comedy personality."},
	{HumourLight, "Light", "The occasional wry line. Safe for a championship round."},
	{HumourCheeky, "Cheeky", "Regular playful digs and gags wherever there's room."},
	{HumourSendIt, "Send it", "Maximum. Lean on every opening the data gives you."},
}

// ValidHumour resolves an arbitrary stored value to a known level, falling back
// to HumourDefault so junk config can never inject prompt text.
func ValidHumour(key string) string {
	for _, h := range HumourOptions {
		if h.Key == key {
			return h.Key
		}
	}
	return HumourDefault
}

// humourGuidance is the prompt clause per level. Deliberately about DENSITY,
// not about what's allowed — the theme owns the character and the guardrails
// (punch up, never down) are non-negotiable and live in humourGuardrail.
var humourGuidance = map[string]string{
	HumourStraight: "HUMOUR DIAL — STRAIGHT: play this one completely straight. No jokes, no gags, no ribbing, " +
		"whatever the personality above suggests. Warmth and enthusiasm are fine; comedy is not.",
	HumourLight: "HUMOUR DIAL — LIGHT: at most one wry, understated line in the whole script. Keep everything " +
		"else a straight call.",
	HumourCheeky: "HUMOUR DIAL — CHEEKY: land a playful dig or a gag wherever there's genuine room for one, " +
		"especially in the build-up and the wrap. Keep the racing calls themselves accurate and quick.",
	HumourSendIt: "HUMOUR DIAL — SEND IT: go for it. Take every opening the data gives you, riff hard in the " +
		"build-up and the wrap, and let the two of them properly wind each other up. The facts still have to be " +
		"right and the calls still have to be quick.",
}

// humourGuardrail rides with any dialled-up humour. Club racing is a small room
// and the pilots being ribbed are in it — this is the line we do not cross, and
// it is stated separately from the theme so no personality can soften it.
const humourGuardrail = "ALWAYS, whatever the dial: punch UP or sideways, never down. Rib the situation, the " +
	"absurdity, the standings and the pilots at the SHARP end — never a pilot for being slow, new, or having a bad " +
	"day, never their kit, never anything about their person. Never joke about a crash, a DNF or a pilot going " +
	"down. If a pilot's card says they'd rather not be ribbed, they're off limits entirely — call them straight."

// humourDelivery flavours the TTS instruction so the voices match the writing.
var humourDelivery = map[string]string{
	HumourStraight: "Keep the delivery straight and professional throughout.",
	HumourCheeky:   "Let the playful lines land with a smile in the voice and a beat of comic timing.",
	HumourSendIt:   "Really sell the funny lines — comic timing, relish the digs, clearly enjoying yourselves.",
}

// Style is the resolved presentation config for one script request. It travels
// as a value so the agent, the website preview and the replay generator all
// compose their prompt from the identical rules.
type Style struct {
	// Theme is the fallback personality for every phase.
	Theme string
	// ThemePre/In/Post optionally override Theme for that phase ("" = use
	// Theme). Phases are the BeatPhase groups: pre-race build-up, live in-race
	// calls, post-race wrap.
	ThemePre, ThemeIn, ThemePost string
	// Humour is the intensity dial (see HumourOptions).
	Humour string
	// Language optionally asks for the whole script in another language. Used
	// by the per-race regenerate ("give me this heat in Dutch"); blank = English.
	// Per-PILOT language flourishes are a different thing and ride in the
	// pilot cards, not here.
	Language string
	// Accent is an optional regional variety of Language — "Brabants",
	// "Flemish", "Bavarian", "Québécois". A free-text hint rather than a
	// whitelist: there are hundreds of dialects and no useful closed set, so it
	// is sanitised (SanitiseAccent) instead of enumerated. Ignored without a
	// Language, since an accent for an unspecified language means nothing.
	Accent string

	// Energy is how hard the delivery pushes.
	//
	// Separate from Humour (which is density of wit) and from Theme (which is
	// character): a straight, classic booth can still be loud. It reaches the
	// TTS instruction rather than the script, because this is about how the
	// words are READ — asking the writer for excitement produces exclamation
	// marks, which is not the same thing at all.
	//
	//	""          the existing broadcast register
	//	"broadcast" leans into it — sports television
	//	"hyped"     full send, still articulate
	Energy string
}

// maxAccent bounds the accent hint. It reaches an LLM instruction, so it is
// capped and character-filtered rather than trusted.
const maxAccent = 60

// EnergyOptions are the pickable levels.
var EnergyOptions = []struct{ Key, Label string }{
	{"", "Broadcast standard"},
	{"broadcast", "Lean into it — sports television"},
	{"hyped", "Full send — loud and fast"},
}

// EnergyClause is the delivery push for this level.
//
// WORDED THE WAY THE MODEL EXPECTS. Qwen's instruct model takes a natural-
// language description of SPEECH CHARACTERISTICS — its own documented example
// is "Fast-paced speech with noticeable upward intonation, ideal for presenting
// fashion products" — so these describe pace, intonation and emphasis rather
// than asking the model to feel excited. An abstract mood flattens back to the
// default register within a sentence; concrete direction about how to move
// through a line actually changes the read.
//
// English on purpose: the instruction field accepts Chinese and English only,
// whatever language the SCRIPT is in.
//
// Each one also states what must not be lost. The whole point of a race call is
// the numbers, and a shouted lap time is worth less than a calm one — pilots
// are listening for it through goggles, over motors.
func (s Style) EnergyClause() string {
	switch strings.ToLower(strings.TrimSpace(s.Energy)) {
	case "broadcast":
		return "Fast-paced delivery with rising intonation through each line and firm emphasis on the " +
			"final words, in the manner of live televised sport. Keep callsigns and numbers crisply " +
			"articulated and clearly separated from the words around them."
	case "hyped":
		return "Very fast, loud, driving delivery at the top of the speaker's range, with sharp attack on " +
			"the key word of each line and strong rising intonation, in the manner of a commentator calling " +
			"the decisive moment of a championship. Keep every callsign and every number fully articulated " +
			"and distinctly separated, never swallowed by the pace."
	default:
		return ""
	}
}

// SanitiseAccent reduces an accent hint to letters, spaces, hyphens and
// apostrophes — enough for every real dialect name ("Brabants", "Québécois",
// "West Country") and nothing that could carry an instruction into the prompt.
func SanitiseAccent(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == ' ', r == '-', r == '\'':
			b.WriteRune(r)
		case r > 127: // accented letters: é in Québécois, ü in Zürich…
			b.WriteRune(r)
		}
		if b.Len() >= maxAccent {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// accentOf returns the sanitised accent, but only when a language is set.
func (s Style) accentOf() string {
	if strings.TrimSpace(s.Language) == "" {
		return ""
	}
	return SanitiseAccent(s.Accent)
}

// VoiceClause is the TTS-side language/accent direction, and it must land LAST
// in the delivery instruction because it OVERRIDES whatever accent the tenant's
// voice characteristics asked for.
//
// Without it, a club whose voice characteristics say "Australian accents, ESPN
// polish" had its Dutch re-cut read with an Australian direction — the script
// was Dutch and the delivery instruction was fighting it. Accent is a property
// of delivery, so it has to reach the TTS prompt, not just the script prompt.
func (s Style) VoiceClause() string {
	lang := strings.TrimSpace(s.Language)
	if lang == "" {
		// An accent WITHOUT a language is meaningful: the script is in the
		// default language (English), and the ask is to speak it with that
		// accent — "English with an Italian accent" is an entirely ordinary
		// broadcast voice. This used to return "" here, which made English
		// the one language that could not take an accent, because the
		// language picker's default IS English and carries no value.
		if a := s.accentOf(); a != "" {
			return " IMPORTANT — speak with a " + a + " accent. " +
				"This accent overrides any other accent direction above."
		}
		return ""
	}
	out := " IMPORTANT — this script is in " + lang + ": speak it as a native " + lang + " speaker"
	if a := s.accentOf(); a != "" {
		out += " with a " + a + " accent (the regional variety, not standard " + lang + ")"
	}
	return out + ". This language and accent override any other accent direction above."
}

// ThemeFor resolves the personality for one beat, honouring the per-phase
// override when the club set one.
func (s Style) ThemeFor(kind string) string {
	var override string
	switch BeatPhase(kind) {
	case "pre":
		override = s.ThemePre
	case "in":
		override = s.ThemeIn
	case "post":
		override = s.ThemePost
	}
	if strings.TrimSpace(override) != "" {
		return override
	}
	return s.Theme
}

// Guidance is the humour clause (plus its guardrail) appended to a script
// prompt. Empty for HumourDefault/HumourStraight-free configs so the classic
// prompt is unchanged.
func (s Style) Guidance() string {
	g := humourGuidance[ValidHumour(s.Humour)]
	if g == "" {
		return ""
	}
	if ValidHumour(s.Humour) == HumourStraight {
		return g // no guardrail needed when there's no comedy to guard
	}
	return g + "\n" + humourGuardrail
}

// Delivery is the humour clause for the TTS instruction.
func (s Style) Delivery() string { return humourDelivery[ValidHumour(s.Humour)] }

// LanguageClause asks for the script in another language. The booth still uses
// the pilots' real callsigns verbatim — translating a callsign would be wrong
// and unrecognisable on stream.
func (s Style) LanguageClause() string {
	lang := strings.TrimSpace(s.Language)
	if lang == "" {
		return ""
	}
	out := "LANGUAGE: write and speak the ENTIRE script in " + lang + ". Keep every pilot callsign, channel " +
		"label and lap time exactly as given (callsigns are names — never translate them), but everything the " +
		"commentators say around them is in " + lang + "."
	if a := s.accentOf(); a != "" {
		out += " Write it in the " + a + " regional variety of " + lang + " — use the words, turns of phrase and " +
			"idiom a " + a + " speaker would actually use, not standard " + lang + " with an accent pasted on."
	}
	return out
}

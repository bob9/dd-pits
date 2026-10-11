package booth

import (
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/ddapi"
)

// A chosen language has to reach BOTH the words and the delivery.
//
// Reported from a venue: "even after selecting Spanish I still hear English".
// The setting saved and the voice test spoke Spanish, because the test builds
// its own Style from the config — the live booth was never handed one. This
// pins the two places the language has to land, so the plumbing that feeds them
// is the only thing that can break next time.
func TestLanguageReachesScriptAndDelivery(t *testing.T) {
	mv := &ddapi.Meeting{Name: "Cup"}
	race := ddapi.Race{Label: "Race 1", TypeLabel: "Race", TargetLaps: 3,
		Pilots: []ddapi.Pilot{{Name: "bob9", Laps: []float64{11.4}, Position: 1}}}

	plain := PromptStyled("progress", RaceContext(race, mv, false), "", false, Style{}, "")
	if strings.Contains(plain, "Spanish") {
		t.Fatal("a zero Style must not mention any language")
	}

	// The SCRIPT — the words themselves have to be written in it. A Spanish
	// accent reading English text is not what was asked for.
	got := PromptStyled("progress", RaceContext(race, mv, false), "", false, Style{Language: "Spanish"}, "")
	if !strings.Contains(got, "Spanish") {
		t.Error("the script prompt does not carry the chosen language, so the " +
			"commentary would be written in English")
	}

	// The DELIVERY — the accent clause is a property of the reading, so it goes
	// to the TTS instruction as well.
	inst := TTSInstructionStyled("excited", "", false, Style{Language: "Spanish"}, "")
	if !strings.Contains(inst, "Spanish") {
		t.Error("the TTS instruction does not carry the language")
	}
}

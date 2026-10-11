package main

// One long-form voice engine, shared by everything that is not a lap call.
//
// Measured on a race day: FOUR copies of the same 0.6B voice model — booth,
// announcer, the audition's warm engine, and the settings-page voice test —
// totalling ~16GB resident with swap at 11GB, and every call crackling,
// because each consumer politely started its own sidecar with the same
// reference. The model is identical; the reference is identical; the only
// thing quadrupled was the memory.
//
// Deliberately TWO engines, not one. The announcer keeps its own: lap calls
// are streamed and perishable, and an engine serialises requests — behind a
// twenty-second wrap render, a lap call would wait past its deadline and be
// dropped. Everything long-form (booth wraps, auditions, voice tests) shares
// this one; they were never latency-critical and they serialise happily.

import (
	"strings"
	"sync"

	"github.com/bob9/dd-pits/internal/tts"
)

var (
	longformMu     sync.Mutex
	longformEngine *tts.LocalEngine
	longformKey    string
)

// longformCfgKey identifies an engine configuration; a different reference is
// a different voice and must not be answered by the shared engine.
func longformCfgKey(cfg tts.LocalConfig) string {
	return strings.Join([]string{cfg.Python, cfg.Script, cfg.Model,
		cfg.ReferenceAudio, cfg.ReferenceText}, "\x00")
}

// registerLongformEngine records the booth's engine as the shared one.
func registerLongformEngine(e *tts.LocalEngine, cfg tts.LocalConfig) {
	longformMu.Lock()
	longformEngine, longformKey = e, longformCfgKey(cfg)
	longformMu.Unlock()
}

// sharedLongformEngine returns the booth's engine when the wanted config
// matches it — the caller must NOT Close what it gets. Nil when there is no
// match, in which case the caller starts (and owns) its own.
func sharedLongformEngine(cfg tts.LocalConfig) *tts.LocalEngine {
	longformMu.Lock()
	defer longformMu.Unlock()
	if longformEngine != nil && longformKey == longformCfgKey(cfg) {
		return longformEngine
	}
	return nil
}

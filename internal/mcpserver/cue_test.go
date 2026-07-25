package mcpserver

import (
	"testing"
	"time"
)

// TestBeepSuppressionOnContinuation covers the cue rules: beep on a fresh listen
// turn, stay silent on a listen that merely continues after an empty (timed-out)
// listen, but re-prompt with a beep once enough idle time has passed.
func TestBeepSuppressionOnContinuation(t *testing.T) {
	d := &deps{cue: map[string]*cueState{}}
	const id = "s1"

	if !d.beepForListen(id, false) {
		t.Fatal("first listen is a fresh turn — should beep")
	}
	d.noteListenEnd(id, "timeout") // empty listen

	if d.beepForListen(id, false) {
		t.Fatal("immediate re-listen after an empty listen is a continuation — should be silent")
	}

	// A continuation after a genuine lapse (long gap since the last listen ended)
	// re-prompts with a beep.
	d.noteListenEnd(id, "timeout")
	d.cue[id].lastEnd = time.Now().Add(-2 * rePromptAfter)
	if !d.beepForListen(id, false) {
		t.Fatal("re-listen after a long not-listening gap should re-prompt with a beep")
	}

	d.noteListenEnd(id, "ok") // the user finally spoke
	if !d.beepForListen(id, false) {
		t.Fatal("listen after a delivered utterance is a fresh turn — should beep")
	}

	d.noteListenEnd(id, "timeout")
	if !d.beepForListen(id, true) {
		t.Fatal("a spoken converse is always a fresh turn — should beep even after a timeout")
	}
}

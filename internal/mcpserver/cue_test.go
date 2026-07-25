package mcpserver

import "testing"

// TestBeepSuppressionOnContinuation covers the cue rules: beep on a fresh listen
// turn, but stay silent on a listen that merely continues after an empty
// (timed-out) listen.
func TestBeepSuppressionOnContinuation(t *testing.T) {
	d := &deps{lastEmpty: map[string]bool{}}
	const id = "s1"

	if !d.beepForListen(id, false) {
		t.Fatal("first listen is a fresh turn — should beep")
	}
	d.noteListenEnd(id, "timeout") // empty listen

	if d.beepForListen(id, false) {
		t.Fatal("re-listen after an empty listen is a continuation — should be silent")
	}
	d.noteListenEnd(id, "ok") // the user finally spoke

	if !d.beepForListen(id, false) {
		t.Fatal("listen after a delivered utterance is a fresh turn — should beep")
	}
	d.noteListenEnd(id, "timeout")

	if !d.beepForListen(id, true) {
		t.Fatal("a spoken converse is always a fresh turn — should beep even after a timeout")
	}
	// A spoken converse also resets the empty flag, so a following listen beeps.
	d.noteListenEnd(id, "ok")
	if !d.beepForListen(id, false) {
		t.Fatal("listen after a spoken converse's delivery should beep")
	}
}

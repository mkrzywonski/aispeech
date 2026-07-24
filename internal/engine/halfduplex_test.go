package engine

import (
	"testing"
	"time"
)

// TestCaptureGate verifies the half-duplex guard: capture is suppressed while
// playback is active and for a short tail after it ends, then re-enabled. This
// is what stops the recorder from transcribing our own TTS output.
func TestCaptureGate(t *testing.T) {
	a := &AudioContext{}

	if a.captureGated() {
		t.Fatal("capture should be open when idle")
	}

	// Simulate an active playback.
	a.playing.Store(true)
	if !a.captureGated() {
		t.Fatal("capture should be gated while playing")
	}

	// Playback ends; the tail keeps the gate closed briefly.
	a.playing.Store(false)
	a.gateUntil.Store(time.Now().Add(captureGuardTail).UnixNano())
	if !a.captureGated() {
		t.Fatal("capture should stay gated during the guard tail")
	}

	// Once the tail elapses, capture reopens.
	a.gateUntil.Store(time.Now().Add(-time.Millisecond).UnixNano())
	if a.captureGated() {
		t.Fatal("capture should reopen after the guard tail")
	}
}

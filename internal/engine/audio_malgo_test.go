package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestVADUsesConfiguredPauseBlocks(t *testing.T) {
	pause := &atomic.Int64{}
	pause.Store(2)
	v := newVAD(pause)

	speech := make([]float32, vadBlock)
	for i := range speech {
		speech[i] = 0.1
	}
	silence := make([]float32, vadBlock)

	for i := 0; i < vadMinBlocks; i++ {
		if done := v.push(speech); len(done) != 0 {
			t.Fatalf("speech block completed an utterance: %d", len(done))
		}
	}
	if done := v.push(silence); len(done) != 0 {
		t.Fatalf("utterance ended after one silent block, want two")
	}
	if done := v.push(silence); len(done) != 1 {
		t.Fatalf("utterance completions after configured pause = %d, want 1", len(done))
	}
}

func TestVADCapsLongUtterance(t *testing.T) {
	v := newVAD(nil) // default pause; won't trigger within the cap window here

	speech := make([]float32, vadBlock)
	for i := range speech {
		speech[i] = 0.1
	}

	// Continuous speech with no pause must still be force-ended by the length cap.
	var got [][]float32
	for pushed := 0; pushed < vadMaxSamp/vadBlock+2 && got == nil; pushed++ {
		if done := v.push(speech); len(done) > 0 {
			got = done
		}
	}
	if got == nil {
		t.Fatal("continuous speech never hit the length cap")
	}
}

func TestVADPrerollPrependsOnset(t *testing.T) {
	pause := &atomic.Int64{}
	pause.Store(2)
	v := newVAD(pause)

	speech := make([]float32, vadBlock)
	for i := range speech {
		speech[i] = 0.1
	}
	silence := make([]float32, vadBlock)

	// Fill the pre-roll window with (sub-threshold) audio that precedes speech.
	for i := 0; i < prerollSamples/vadBlock+5; i++ {
		v.push(silence)
	}
	// A single speech block starts the utterance; its onset should be seeded with
	// the pre-roll lookback so the first phoneme isn't clipped.
	v.push(speech)
	v.push(silence)
	done := v.push(silence) // second silent block ends the utterance (pause=2)
	if len(done) != 1 {
		t.Fatalf("expected one completed utterance, got %d", len(done))
	}
	if got, min := len(done[0]), prerollSamples+vadBlock; got < min {
		t.Fatalf("utterance length %d < pre-roll + onset %d — onset was not prepended", got, min)
	}
}

func TestVADHoldSuppressesEndpointing(t *testing.T) {
	pause := &atomic.Int64{}
	pause.Store(2)
	hold := &atomic.Bool{}
	hold.Store(true)
	v := newVAD(pause)
	v.holding = hold

	speech := make([]float32, vadBlock)
	for i := range speech {
		speech[i] = 0.1
	}
	silence := make([]float32, vadBlock)

	for i := 0; i < vadMinBlocks; i++ {
		v.push(speech)
	}
	// Silence far past the pause threshold must NOT end the utterance while held.
	for i := 0; i < 20; i++ {
		if done := v.push(silence); len(done) != 0 {
			t.Fatalf("holding should suppress silence endpointing, got %d completions", len(done))
		}
	}
	// Release flushes the accumulated utterance.
	if u := v.flush(); u == nil {
		t.Fatal("flush should emit the held utterance on release")
	}
}

func TestMalgoRecorderPauseDurationIsClamped(t *testing.T) {
	r := NewMalgoRecorder(nil)
	r.SetPauseDuration(100 * time.Millisecond)
	if got := r.PauseDuration(); got != 300*time.Millisecond {
		t.Fatalf("short pause = %v, want 300ms minimum", got)
	}
	r.SetPauseDuration(15 * time.Second)
	if got := r.PauseDuration(); got != 10*time.Second {
		t.Fatalf("long pause = %v, want 10s maximum", got)
	}
}

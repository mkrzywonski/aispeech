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
	done := v.push(silence)
	if len(done) != 1 {
		t.Fatalf("utterance completions after configured pause = %d, want 1", len(done))
	}
	if done[0].capped {
		t.Fatal("pause-ended utterance should not be flagged capped")
	}
}

func TestVADFlagsCappedUtterance(t *testing.T) {
	v := newVAD(nil) // default pause; won't trigger within the cap window here

	speech := make([]float32, vadBlock)
	for i := range speech {
		speech[i] = 0.1
	}

	// Feed continuous speech past the length cap and confirm the forced cut is
	// flagged capped (so the engine can play the cutoff cue).
	var got *vadResult
	for pushed := 0; pushed < vadMaxSamp/vadBlock+2 && got == nil; pushed++ {
		for _, res := range v.push(speech) {
			r := res
			got = &r
		}
	}
	if got == nil {
		t.Fatal("continuous speech never hit the length cap")
	}
	if !got.capped {
		t.Fatal("cap-ended utterance should be flagged capped")
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

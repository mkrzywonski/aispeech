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

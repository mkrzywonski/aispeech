package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mkrzywonski/aispeech/internal/session"
)

// tracker flags any concurrent playback (an overlap).
type tracker struct {
	active   atomic.Int32
	overlaps atomic.Int32
}

func (tr *tracker) run() {
	if tr.active.Add(1) > 1 {
		tr.overlaps.Add(1)
	}
	time.Sleep(10 * time.Millisecond)
	tr.active.Add(-1)
}

type busySpeaker struct{ tr *tracker }

func (b busySpeaker) Speak(context.Context, string, string) error { b.tr.run(); return nil }

type busySound struct{ tr *tracker }

func (b busySound) PlaySound(_ context.Context, name, _ string) (string, error) {
	b.tr.run()
	return name, nil
}

// TestPlaybackSerialized checks that speech and sounds share one FIFO queue and
// never play at the same time, even under concurrent callers.
func TestPlaybackSerialized(t *testing.T) {
	tr := &tracker{}
	svc := New(session.New(), nil, nil, busySpeaker{tr}, time.Minute, 0)
	svc.SetSounds(busySound{tr})

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _, _ = svc.Speak(context.Background(), "hi")
			} else {
				_, _ = svc.PlaySound(context.Background(), "chime", "")
			}
		}(i)
	}
	wg.Wait()

	if n := tr.overlaps.Load(); n != 0 {
		t.Fatalf("playback overlapped %d times; speech and sounds must serialize", n)
	}
}

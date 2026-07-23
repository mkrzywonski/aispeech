package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mkrzywonski/aispeech/internal/session"
)

type blockingSpeaker struct {
	started chan string
	release chan struct{}

	mu    sync.Mutex
	order []string
}

func newBlockingSpeaker() *blockingSpeaker {
	return &blockingSpeaker{
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
}

func (s *blockingSpeaker) Speak(_ context.Context, text, _ string) error {
	s.mu.Lock()
	s.order = append(s.order, text)
	s.mu.Unlock()
	s.started <- text
	<-s.release
	return nil
}

func (s *blockingSpeaker) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func TestSpeakQueuesPlaybackFIFO(t *testing.T) {
	tts := newBlockingSpeaker()
	svc := New(session.New(), nil, nil, tts, time.Minute, 600)

	firstDone := make(chan error, 1)
	go func() {
		_, _, err := svc.Speak(context.Background(), "first")
		firstDone <- err
	}()
	if got := <-tts.started; got != "first" {
		t.Fatalf("first playback = %q, want first", got)
	}

	secondDone := make(chan error, 1)
	go func() {
		_, _, err := svc.Speak(context.Background(), "second")
		secondDone <- err
	}()

	select {
	case got := <-tts.started:
		t.Fatalf("second playback started before first completed: %q", got)
	case <-time.After(50 * time.Millisecond):
	}

	tts.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("first Speak: %v", err)
	}
	if got := <-tts.started; got != "second" {
		t.Fatalf("second playback = %q, want second", got)
	}
	tts.release <- struct{}{}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Speak: %v", err)
	}

	if got, want := tts.calls(), []string{"first", "second"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("playback order = %q, want %q", got, want)
	}
}

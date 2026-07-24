package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mkrzywonski/aispeech/internal/session"
)

type channelRecorder struct{ segments chan Segment }

func (r *channelRecorder) Start(context.Context) (<-chan Segment, error) { return r.segments, nil }
func (r *channelRecorder) Stop() error                                   { return nil }

type resultTranscriber struct {
	text   string
	err    error
	called chan struct{}
}

type immediateSoundPlayer struct{}

func (immediateSoundPlayer) PlaySound(context.Context, string, string) (string, error) {
	return "ding", nil
}

func (t *resultTranscriber) Transcribe(context.Context, Segment) (string, error) {
	t.called <- struct{}{}
	return t.text, t.err
}

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

func TestDialogActivityRequiresSuccessfulSTTOrTTS(t *testing.T) {
	old := time.Unix(1, 0)

	// A VAD segment whose transcription fails is not a user interaction and
	// must not reset the dialog timer.
	failingRec := &channelRecorder{segments: make(chan Segment, 1)}
	failingSTT := &resultTranscriber{err: errors.New("no speech"), called: make(chan struct{}, 1)}
	failingSvc := New(session.New(), failingRec, failingSTT, nil, time.Minute, 600)
	if err := failingSvc.StartDialog(); err != nil {
		t.Fatal(err)
	}
	defer failingSvc.Stop()
	failingSvc.mu.Lock()
	failingSvc.lastActive = old
	failingSvc.mu.Unlock()
	failingRec.segments <- Segment{}
	<-failingSTT.called
	time.Sleep(10 * time.Millisecond) // let the loop process the failed result
	failingSvc.mu.Lock()
	got := failingSvc.lastActive
	failingSvc.mu.Unlock()
	if !got.Equal(old) {
		t.Fatalf("failed STT reset dialog activity to %v, want %v", got, old)
	}

	// Even successful STT is not activity if there is no AI ready to receive it.
	droppedRec := &channelRecorder{segments: make(chan Segment, 1)}
	droppedSTT := &resultTranscriber{text: "ambient speech", called: make(chan struct{}, 1)}
	droppedSvc := New(session.New(), droppedRec, droppedSTT, nil, time.Minute, 600)
	if err := droppedSvc.StartDialog(); err != nil {
		t.Fatal(err)
	}
	defer droppedSvc.Stop()
	droppedSvc.mu.Lock()
	droppedSvc.lastActive = old
	droppedSvc.mu.Unlock()
	droppedRec.segments <- Segment{}
	<-droppedSTT.called
	time.Sleep(10 * time.Millisecond)
	droppedSvc.mu.Lock()
	got = droppedSvc.lastActive
	droppedSvc.mu.Unlock()
	if !got.Equal(old) {
		t.Fatalf("undelivered STT reset dialog activity to %v, want %v", got, old)
	}

	// A successful transcription resets the timer only when it reaches a
	// listening AI session.
	successRec := &channelRecorder{segments: make(chan Segment, 1)}
	successSTT := &resultTranscriber{text: "hello", called: make(chan struct{}, 1)}
	successReg := session.New()
	successReg.Attach("agent", "codex")
	if _, err := successReg.Pair("agent", "browser"); err != nil {
		t.Fatal(err)
	}
	listenCtx, cancelListen := context.WithCancel(context.Background())
	defer cancelListen()
	go successReg.Listen(listenCtx, "agent", time.Minute)
	deadline := time.Now().Add(time.Second)
	for {
		views, _ := successReg.Snapshot()
		if len(views) == 1 && views[0].Listening {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not start listening")
		}
		time.Sleep(time.Millisecond)
	}
	successSvc := New(successReg, successRec, successSTT, nil, time.Minute, 600)
	if err := successSvc.StartDialog(); err != nil {
		t.Fatal(err)
	}
	defer successSvc.Stop()
	successSvc.mu.Lock()
	successSvc.lastActive = old
	successSvc.mu.Unlock()
	successRec.segments <- Segment{}
	<-successSTT.called
	deadline = time.Now().Add(time.Second)
	for {
		successSvc.mu.Lock()
		got = successSvc.lastActive
		successSvc.mu.Unlock()
		if got.After(old) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("successful STT did not reset dialog activity")
		}
		time.Sleep(time.Millisecond)
	}

	// Successful TTS playback also counts as a completed interaction.
	ttsSvc := New(session.New(), nil, nil, NullSpeaker{}, time.Minute, 600)
	ttsSvc.mu.Lock()
	ttsSvc.mode = ModeDialog
	ttsSvc.lastActive = old
	ttsSvc.mu.Unlock()
	if _, _, err := ttsSvc.Speak(context.Background(), "reply"); err != nil {
		t.Fatal(err)
	}
	ttsSvc.mu.Lock()
	got = ttsSvc.lastActive
	ttsSvc.mu.Unlock()
	if !got.After(old) {
		t.Fatal("successful TTS did not reset dialog activity")
	}

	// A successfully played sound/WAV is also an interaction.
	soundSvc := New(session.New(), nil, nil, nil, time.Minute, 600)
	soundSvc.SetSounds(immediateSoundPlayer{})
	soundSvc.mu.Lock()
	soundSvc.mode = ModeDialog
	soundSvc.lastActive = old
	soundSvc.mu.Unlock()
	if _, err := soundSvc.PlaySound(context.Background(), "ding", ""); err != nil {
		t.Fatal(err)
	}
	soundSvc.mu.Lock()
	got = soundSvc.lastActive
	soundSvc.mu.Unlock()
	if !got.After(old) {
		t.Fatal("successful sound did not reset dialog activity")
	}
}

func TestDialogTimeoutCanBeDisabled(t *testing.T) {
	svc := New(session.New(), nil, nil, nil, time.Minute, 600)
	svc.mu.Lock()
	svc.mode = ModeDialog
	svc.lastActive = time.Now().Add(-2 * time.Minute)
	svc.mu.Unlock()

	svc.SetDialogTimeoutEnabled(false)
	if _, _, active := svc.DialogCountdown(); active {
		t.Fatal("disabled dialog timeout should hide the countdown")
	}

	svc.SetDialogTimeoutEnabled(true)
	remaining, _, active := svc.DialogCountdown()
	if !active || remaining != 0 {
		t.Fatalf("enabled expired timeout = (%v, %v), want (0, true)", remaining, active)
	}
}

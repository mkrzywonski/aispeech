// Package engine defines the audio/STT/TTS boundary and a Service that wires
// microphone capture, transcription, routing, and speech synthesis together.
//
// The concrete audio (malgo), STT (whisper.cpp), and TTS (piper) implementations
// plug in behind these interfaces; the null implementations let the rest of the
// app build and run without them.
package engine

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mkrzywonski/aispeech/internal/session"
)

// ErrUnavailable is returned by null engines when a real backend is not wired.
var ErrUnavailable = errors.New("engine unavailable")

// Recorder captures microphone audio while listening is active. Implementations
// endpoint speech (VAD) and emit one transcribed-ready PCM segment per utterance
// on the returned channel; the Service transcribes and routes them.
type Recorder interface {
	// Start opens the input device and begins emitting utterance segments.
	Start(ctx context.Context) (<-chan Segment, error)
	// Stop closes the device and stops emitting.
	Stop() error
}

// Segment is one endpointed utterance as 16 kHz mono PCM (float32).
type Segment struct {
	PCM        []float32
	SampleRate int
}

// Transcriber turns a PCM segment into text (STT).
type Transcriber interface {
	Transcribe(ctx context.Context, seg Segment) (string, error)
}

// Speaker synthesizes text to audio and plays it (TTS).
type Speaker interface {
	Speak(ctx context.Context, text string) error
}

// ListenMode describes the microphone lifecycle.
type ListenMode int

const (
	ModeIdle     ListenMode = iota // mic cold
	ModeDialog                     // PTT dialog: hot until idle timeout
	ModeConstant                   // always hot (opt-in)
)

func (m ListenMode) String() string {
	switch m {
	case ModeDialog:
		return "dialog"
	case ModeConstant:
		return "constant"
	default:
		return "idle"
	}
}

// Service owns the capture→transcribe→route loop and the speak path.
type Service struct {
	reg      *session.Registry
	rec      Recorder
	speakCap int

	engMu sync.RWMutex // guards stt/tts (hot-swappable at runtime)
	stt   Transcriber
	tts   Speaker

	// speakQ serializes synthesis and playback. A single audio output cannot
	// safely play two agent replies at once; channel order is the FIFO order in
	// which requests are accepted by Speak.
	speakQ chan speakRequest

	mu         sync.Mutex
	mode       ListenMode
	cancel     context.CancelFunc
	dialogTO   time.Duration
	lastActive time.Time
}

// speakRequest is one queued speak() call. done is buffered so a caller that
// disconnects while waiting never stalls the playback worker.
type speakRequest struct {
	ctx  context.Context
	text string
	done chan error
}

const speakQueueCapacity = 32

// New builds a Service. Any nil engine is replaced with a null implementation.
func New(reg *session.Registry, rec Recorder, stt Transcriber, tts Speaker, dialogTimeout time.Duration, speakCap int) *Service {
	if rec == nil {
		rec = NullRecorder{}
	}
	if stt == nil {
		stt = NullTranscriber{}
	}
	if tts == nil {
		tts = NullSpeaker{}
	}
	s := &Service{
		reg:      reg,
		rec:      rec,
		stt:      stt,
		tts:      tts,
		dialogTO: dialogTimeout,
		speakCap: speakCap,
		speakQ:   make(chan speakRequest, speakQueueCapacity),
	}
	go s.speakLoop()
	return s
}

// Mode reports the current listening mode.
func (s *Service) Mode() ListenMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// SetDialogTimeout updates the PTT dialog idle timeout at runtime. The mic stays
// hot as long as speech keeps arriving; after this much silence it goes cold.
func (s *Service) SetDialogTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.dialogTO = d
	s.mu.Unlock()
}

// DialogTimeout returns the current PTT dialog idle timeout.
func (s *Service) DialogTimeout() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dialogTO
}

// SetTranscriber swaps the STT engine at runtime.
func (s *Service) SetTranscriber(t Transcriber) {
	if t == nil {
		t = NullTranscriber{}
	}
	s.engMu.Lock()
	s.stt = t
	s.engMu.Unlock()
}

// SetSpeaker swaps the TTS engine at runtime.
func (s *Service) SetSpeaker(sp Speaker) {
	if sp == nil {
		sp = NullSpeaker{}
	}
	s.engMu.Lock()
	s.tts = sp
	s.engMu.Unlock()
}

func (s *Service) transcriber() Transcriber {
	s.engMu.RLock()
	defer s.engMu.RUnlock()
	return s.stt
}

func (s *Service) speaker() Speaker {
	s.engMu.RLock()
	defer s.engMu.RUnlock()
	return s.tts
}

// STTReady reports whether a real transcriber is installed.
func (s *Service) STTReady() bool {
	_, null := s.transcriber().(NullTranscriber)
	return !null
}

// TTSReady reports whether a real speaker is installed.
func (s *Service) TTSReady() bool {
	_, null := s.speaker().(NullSpeaker)
	return !null
}

// StartDialog opens a PTT listening dialog (mic hot until idle timeout).
func (s *Service) StartDialog() error { return s.start(ModeDialog) }

// StartConstant enables always-on listening.
func (s *Service) StartConstant() error { return s.start(ModeConstant) }

// Stop ends listening and closes the mic.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *Service) start(mode ListenMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	ctx, cancel := context.WithCancel(context.Background())
	segs, err := s.rec.Start(ctx)
	if err != nil {
		cancel()
		return err
	}
	s.mode = mode
	s.cancel = cancel
	s.lastActive = time.Now()
	go s.loop(ctx, segs, mode)
	return nil
}

func (s *Service) stopLocked() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
		_ = s.rec.Stop()
	}
	s.mode = ModeIdle
}

// loop transcribes utterance segments and routes them, and enforces the dialog
// idle timeout.
func (s *Service) loop(ctx context.Context, segs <-chan Segment, mode ListenMode) {
	var tick <-chan time.Time
	if mode == ModeDialog {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case seg, ok := <-segs:
			if !ok {
				return
			}
			s.mu.Lock()
			s.lastActive = time.Now()
			s.mu.Unlock()
			text, err := s.transcriber().Transcribe(ctx, seg)
			if err != nil {
				slog.Warn("transcribe failed", "err", err)
				continue
			}
			s.reg.Deliver(text)
		case <-tick:
			s.mu.Lock()
			idle := time.Since(s.lastActive)
			to := s.dialogTO
			s.mu.Unlock()
			if idle > to {
				slog.Info("dialog idle timeout")
				s.Stop()
				return
			}
		}
	}
}

// Speak enforces the character cap and queues synthesis/playback in FIFO order.
// It returns after this utterance has completed (or failed), so MCP callers can
// accurately report whether their reply was spoken. The bounded queue provides
// backpressure instead of allowing concurrent TTS jobs to overlap.
func (s *Service) Speak(ctx context.Context, text string) (spoken int, truncated bool, err error) {
	if s.speakCap > 0 && len(text) > s.speakCap {
		text = text[:s.speakCap]
		truncated = true
	}
	req := speakRequest{ctx: ctx, text: text, done: make(chan error, 1)}
	select {
	case s.speakQ <- req:
	case <-ctx.Done():
		return 0, truncated, ctx.Err()
	}
	select {
	case err := <-req.done:
		if err != nil {
			return 0, truncated, err
		}
	case <-ctx.Done():
		return 0, truncated, ctx.Err()
	}
	return len(text), truncated, nil
}

func (s *Service) speakLoop() {
	for req := range s.speakQ {
		// A disconnected MCP client should not leave stale speech in the queue.
		if err := req.ctx.Err(); err != nil {
			req.done <- err
			continue
		}
		err := s.speaker().Speak(req.ctx, req.text)
		req.done <- err
	}
}

// InjectTranscript feeds text as if it had been transcribed. Used only by the
// dev-inject endpoint to exercise routing without a microphone. It is never
// exposed on the normal control surface.
func (s *Service) InjectTranscript(text string) { s.reg.Deliver(text) }

// --- null implementations ---

// NullRecorder emits nothing.
type NullRecorder struct{}

func (NullRecorder) Start(context.Context) (<-chan Segment, error) {
	ch := make(chan Segment)
	return ch, nil // open but silent
}
func (NullRecorder) Stop() error { return nil }

// NullTranscriber always errors.
type NullTranscriber struct{}

func (NullTranscriber) Transcribe(context.Context, Segment) (string, error) {
	return "", ErrUnavailable
}

// NullSpeaker logs the text instead of speaking it.
type NullSpeaker struct{}

func (NullSpeaker) Speak(_ context.Context, text string) error {
	slog.Info("speak (null tts)", "text", text)
	return nil
}

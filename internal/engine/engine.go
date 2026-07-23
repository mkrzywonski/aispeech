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

// Speaker synthesizes text to audio and plays it (TTS). voice is a specific
// voice model path, or "" to use the speaker's default.
type Speaker interface {
	Speak(ctx context.Context, text, voice string) error
}

// SoundPlayer plays a built-in named sound or a WAV file (satisfied by
// *AudioContext).
type SoundPlayer interface {
	PlaySound(ctx context.Context, name, file string) (string, error)
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
	sound SoundPlayer // nil when no audio backend

	// playQ serializes all audio output (speech and sounds) in FIFO order. A
	// single output device cannot safely play two things at once; channel order
	// is the order in which jobs are accepted by Speak/PlaySound.
	playQ chan playJob

	mu         sync.Mutex
	mode       ListenMode
	cancel     context.CancelFunc
	dialogTO   time.Duration
	lastActive time.Time
}

// playJob is one queued playback (a spoken reply or a sound). done is buffered
// so a caller that disconnects while waiting never stalls the playback worker.
type playJob struct {
	ctx  context.Context
	run  func(context.Context) error
	done chan error
}

const playQueueCapacity = 32

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
		playQ:    make(chan playJob, playQueueCapacity),
	}
	go s.playLoop()
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

// SetSounds installs the sound player (called once at startup when audio exists).
func (s *Service) SetSounds(p SoundPlayer) {
	s.engMu.Lock()
	s.sound = p
	s.engMu.Unlock()
}

// PlaySound plays a built-in sound or a WAV file, returning a label for it. Like
// Speak, it goes through the FIFO queue so a sound never overlaps a spoken reply.
func (s *Service) PlaySound(ctx context.Context, name, file string) (string, error) {
	s.engMu.RLock()
	p := s.sound
	s.engMu.RUnlock()
	if p == nil {
		return "", ErrUnavailable
	}
	var label string
	err := s.enqueue(ctx, func(c context.Context) error {
		l, e := p.PlaySound(c, name, file)
		label = l
		return e
	})
	return label, err
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
// backpressure instead of allowing concurrent audio jobs to overlap.
func (s *Service) Speak(ctx context.Context, text string) (spoken int, truncated bool, err error) {
	return s.speakVoice(ctx, text, "")
}

// SpeakAs speaks using the voice assigned to sessionID (so each agent can have a
// distinct voice), falling back to the default when unset.
func (s *Service) SpeakAs(ctx context.Context, sessionID, text string) (spoken int, truncated bool, err error) {
	return s.speakVoice(ctx, text, s.reg.Voice(sessionID))
}

func (s *Service) speakVoice(ctx context.Context, text, voice string) (spoken int, truncated bool, err error) {
	if s.speakCap > 0 && len(text) > s.speakCap {
		text = text[:s.speakCap]
		truncated = true
	}
	if err := s.enqueue(ctx, func(c context.Context) error { return s.speaker().Speak(c, text, voice) }); err != nil {
		return 0, truncated, err
	}
	return len(text), truncated, nil
}

// enqueue submits a playback job to the FIFO queue and waits for it to finish,
// so speech and sounds never overlap and play in call order.
func (s *Service) enqueue(ctx context.Context, run func(context.Context) error) error {
	job := playJob{ctx: ctx, run: run, done: make(chan error, 1)}
	select {
	case s.playQ <- job:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-job.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) playLoop() {
	for job := range s.playQ {
		// A disconnected MCP client should not leave stale jobs in the queue.
		if err := job.ctx.Err(); err != nil {
			job.done <- err
			continue
		}
		job.done <- job.run(job.ctx)
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

func (NullSpeaker) Speak(_ context.Context, text, _ string) error {
	slog.Info("speak (null tts)", "text", text)
	return nil
}

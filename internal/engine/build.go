package engine

import (
	"fmt"
	"time"

	"github.com/mkrzywonski/aispeech/internal/session"
)

// BuildOptions carries the settings needed to construct the real engines.
type BuildOptions struct {
	WhisperBin, WhisperModel, Language string
	PiperBin, PiperVoice               string
	DialogTimeout                      time.Duration
	SpeakCap                           int
}

// Build constructs a Service with the real audio/STT/TTS engines where possible,
// falling back to null implementations otherwise. Any degradations are returned
// as human-readable warnings. The returned cleanup releases the audio backend.
func Build(reg *session.Registry, o BuildOptions) (svc *Service, devices *AudioContext, cleanup func(), warnings []string) {
	var (
		rec Recorder    = NullRecorder{}
		stt Transcriber = NullTranscriber{}
		tts Speaker     = NullSpeaker{}
		ac  *AudioContext
	)
	cleanup = func() {}

	ac, err := NewAudioContext()
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("audio unavailable (%v): running without microphone or speech output", err))
		svc = New(reg, rec, stt, tts, o.DialogTimeout, o.SpeakCap)
		return svc, nil, cleanup, warnings
	}
	cleanup = ac.Close
	rec = NewMalgoRecorder(ac)

	if w, err := NewWhisperSTT(o.WhisperBin, o.WhisperModel, o.Language); err != nil {
		warnings = append(warnings, fmt.Sprintf("STT disabled: %v", err))
	} else {
		stt = w
	}

	if p, err := NewPiperTTS(o.PiperBin, o.PiperVoice, ac); err != nil {
		warnings = append(warnings, fmt.Sprintf("TTS disabled: %v", err))
	} else {
		tts = p
	}

	svc = New(reg, rec, stt, tts, o.DialogTimeout, o.SpeakCap)
	svc.SetSounds(ac)
	return svc, ac, cleanup, warnings
}
